package runs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/money"
)

// Reader answers the dashboard's questions about runs. It reads; it never writes.
type Reader struct{ pool *pgxpool.Pool }

func NewReader(pool *pgxpool.Pool) *Reader { return &Reader{pool: pool} }

// Counts is what the page's counters show. Running, Sleeping and NeedsYou are the runs in those states now; the rest
// are runs that ended inside the range.
type Counts struct {
	Running   int `json:"running"`
	Sleeping  int `json:"sleeping"`
	NeedsYou  int `json:"needs_you"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

// Waiting is a run that is waiting for a person.
type Waiting struct {
	ID           uuid.UUID `json:"id"`
	Goal         string    `json:"goal"`
	Tool         string    `json:"tool"`
	WaitingSince time.Time `json:"waiting_since"`
	Key          string    `json:"key"`
}

type Summary struct {
	Counts  Counts    `json:"counts"`
	Waiting []Waiting `json:"waiting"`
}

// Summary counts runs by status and lists the ones waiting for a person.
func (rd *Reader) Summary(ctx context.Context, since time.Time) (Summary, error) {
	var s Summary
	err := rd.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status IN ('queued','running','waiting_tool')),
		count(*) FILTER (WHERE status = 'sleeping'),
		count(*) FILTER (WHERE status = 'waiting_human'),
		count(*) FILTER (WHERE status = 'succeeded' AND finished_at >= $1),
		count(*) FILTER (WHERE status = 'failed' AND finished_at >= $1),
		count(*) FILTER (WHERE status = 'cancelled' AND finished_at >= $1)
		FROM runs`, since).Scan(&s.Counts.Running, &s.Counts.Sleeping, &s.Counts.NeedsYou, &s.Counts.Succeeded, &s.Counts.Failed, &s.Counts.Cancelled)
	if err != nil {
		return s, err
	}
	rows, err := rd.pool.Query(ctx, `SELECT r.id, r.request, COALESCE(k.name,''), COALESCE(w.payload,'{}'::jsonb), COALESCE(w.created_at, r.created_at)
		FROM runs r LEFT JOIN api_keys k ON k.id = r.api_key_id
		LEFT JOIN LATERAL (SELECT payload, created_at FROM run_steps WHERE run_id = r.id AND type = 'wait_human' AND phase = 'started' ORDER BY id DESC LIMIT 1) w ON true
		WHERE r.status = 'waiting_human' ORDER BY 5 LIMIT 50`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	s.Waiting = []Waiting{}
	for rows.Next() {
		var w Waiting
		var raw, payload []byte
		if err := rows.Scan(&w.ID, &raw, &w.Key, &payload, &w.WaitingSince); err != nil {
			return s, err
		}
		w.Goal = goalOf(raw)
		var p struct{ Tool, Reason string }
		_ = json.Unmarshal(payload, &p)
		w.Tool = p.Tool
		if w.Tool == "" {
			w.Tool = "request_human_approval"
		}
		s.Waiting = append(s.Waiting, w)
	}
	return s, rows.Err()
}

// Bucket is runs started in one slice of time, by where they stand now.
type Bucket struct {
	Start      time.Time `json:"start"`
	Succeeded  int       `json:"succeeded"`
	Failed     int       `json:"failed"`
	Cancelled  int       `json:"cancelled"`
	InProgress int       `json:"in_progress"`
}

// BucketFor is how a range is sliced: 5 minutes for an hour, an hour for a day, a day for a week.
func BucketFor(hours int) time.Duration {
	switch {
	case hours <= 1:
		return 5 * time.Minute
	case hours <= 24:
		return time.Hour
	}
	return 24 * time.Hour
}

// Activity returns the runs started in each slice of the range ending at now, oldest first, empty slices included.
func (rd *Reader) Activity(ctx context.Context, hours int, now time.Time) (time.Duration, []Bucket, error) {
	step := BucketFor(hours)
	n := int(time.Duration(hours) * time.Hour / step)
	first := now.Add(-time.Duration(n) * step)
	rows, err := rd.pool.Query(ctx, `SELECT b.start,
		count(r.id) FILTER (WHERE r.status = 'succeeded'),
		count(r.id) FILTER (WHERE r.status = 'failed'),
		count(r.id) FILTER (WHERE r.status = 'cancelled'),
		count(r.id) FILTER (WHERE r.status NOT IN ('succeeded','failed','cancelled'))
		FROM generate_series($1::timestamptz, $2::timestamptz, make_interval(secs => $3)) b(start)
		LEFT JOIN runs r ON r.created_at >= b.start AND r.created_at < b.start + make_interval(secs => $3)
		GROUP BY b.start ORDER BY b.start`, first, now.Add(-step), step.Seconds())
	if err != nil {
		return step, nil, err
	}
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Start, &b.Succeeded, &b.Failed, &b.Cancelled, &b.InProgress); err != nil {
			return step, nil, err
		}
		out = append(out, b)
	}
	return step, out, rows.Err()
}

// StripNode is one step in a run's strip.
type StripNode struct {
	Kind  string `json:"kind"`  // model | tool
	State string `json:"state"` // done | current | failed | waiting
}

// StripLen is how many steps a strip shows. A longer run shows its latest steps and says how many it hides.
const StripLen = 16

type Strip struct {
	Steps []StripNode `json:"steps"`
	More  int         `json:"more"`
}

type Item struct {
	ID            uuid.UUID
	Status        Status
	Goal          string
	Key           string
	Model         string
	StepCount     int
	Cost          money.Micros
	CreatedAt     time.Time
	FinishedAt    *time.Time
	WakeAt        *time.Time
	FailureReason string
	Strip         Strip
}

// ListFilter selects runs. State is active (not finished), finished, or empty for all. Since limits finished runs to
// those that ended in the range; active runs are always listed.
type ListFilter struct {
	State  string
	Status Status
	Since  time.Time
	Limit  int
	Before *Cursor
	// Q matches runs whose task contains this text, ignoring case. KeyName narrows to one API key.
	Q     string
	KeyID *uuid.UUID
}

type Cursor struct {
	At time.Time
	ID uuid.UUID
}

func (rd *Reader) List(ctx context.Context, f ListFilter) ([]Item, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 50
	}
	var before, beforeID any
	if f.Before != nil {
		before, beforeID = f.Before.At, f.Before.ID
	}
	var status any
	if f.Status != "" {
		status = string(f.Status)
	}
	rows, err := rd.pool.Query(ctx, itemSQL+`
		WHERE ($1::text = '' OR ($1 = 'active' AND r.status NOT IN ('succeeded','failed','cancelled')) OR ($1 = 'finished' AND r.status IN ('succeeded','failed','cancelled') AND r.finished_at >= $2))
		  AND ($3::text IS NULL OR r.status = $3)
		  AND ($4::timestamptz IS NULL OR (r.created_at, r.id) < ($4::timestamptz, $5::uuid))
		  AND ($7::text = '' OR (r.request->>'input') ILIKE '%' || $7 || '%' ESCAPE '\')
		  AND ($8::uuid IS NULL OR r.api_key_id = $8)
		ORDER BY r.created_at DESC, r.id DESC LIMIT $6`, f.State, f.Since, status, before, beforeID, f.Limit, likeEscape(f.Q), f.KeyID)
	if err != nil {
		return nil, err
	}
	return rd.collect(ctx, rows)
}

// Get returns one run in the same shape as a list row.
func (rd *Reader) Get(ctx context.Context, id uuid.UUID) (Item, error) {
	rows, err := rd.pool.Query(ctx, itemSQL+` WHERE r.id = $1`, id)
	if err != nil {
		return Item{}, err
	}
	items, err := rd.collect(ctx, rows)
	if err != nil {
		return Item{}, err
	}
	if len(items) == 0 {
		return Item{}, ErrNotFound
	}
	return items[0], nil
}

const itemSQL = `SELECT r.id, r.status, r.request, COALESCE(k.name,''), r.step_count, r.cost_usd, r.created_at, r.finished_at, r.wake_at, COALESCE(r.failure_reason,'')
	FROM runs r LEFT JOIN api_keys k ON k.id = r.api_key_id`

// collect reads rows of itemSQL and adds each run's strip.
func (rd *Reader) collect(ctx context.Context, rows pgx.Rows) ([]Item, error) {
	defer rows.Close()
	var items []Item
	var ids []uuid.UUID
	for rows.Next() {
		var it Item
		var raw []byte
		var st string
		var cost pgtype.Numeric
		if err := rows.Scan(&it.ID, &st, &raw, &it.Key, &it.StepCount, &cost, &it.CreatedAt, &it.FinishedAt, &it.WakeAt, &it.FailureReason); err != nil {
			return nil, err
		}
		it.Status, it.Goal = Status(st), goalOf(raw)
		var req Request
		_ = json.Unmarshal(raw, &req)
		it.Model = req.ModelName()
		var err error
		if it.Cost, err = db.MicrosFromNumeric(cost); err != nil {
			return nil, err
		}
		items = append(items, it)
		ids = append(ids, it.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	strips, err := rd.strips(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Strip = buildStrip(strips[items[i].ID], items[i].Status)
	}
	return items, nil
}

type stepSummary struct {
	No     int
	Type   StepType
	Done   bool
	Failed bool
}

// strips reads one summary per logical step for the given runs, in a single query.
func (rd *Reader) strips(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]stepSummary, error) {
	out := map[uuid.UUID][]stepSummary{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := rd.pool.Query(ctx, `SELECT run_id, step_no, (array_agg(type ORDER BY id))[1],
		bool_or(phase = 'finished'), bool_or(phase = 'failed')
		FROM run_steps WHERE run_id = ANY($1) AND step_no IS NOT NULL GROUP BY run_id, step_no ORDER BY run_id, step_no`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var s stepSummary
		var typ string
		if err := rows.Scan(&id, &s.No, &typ, &s.Done, &s.Failed); err != nil {
			return nil, err
		}
		s.Type = StepType(typ)
		out[id] = append(out[id], s)
	}
	return out, rows.Err()
}

// buildStrip turns step summaries into the strip. The last step of a live run is current (or waiting, for a person); a
// run that ended badly marks its last step failed, so the strip ends on the point where it went wrong.
func buildStrip(steps []stepSummary, status Status) Strip {
	hidden := 0
	if len(steps) > StripLen {
		hidden = len(steps) - StripLen
		steps = steps[hidden:]
	}
	out := Strip{Steps: make([]StripNode, 0, len(steps)), More: hidden}
	for i, s := range steps {
		n := StripNode{Kind: "model", State: "done"}
		if s.Type == ToolCall || s.Type == WaitHuman || s.Type == Sleep {
			n.Kind = "tool"
		}
		last := i == len(steps)-1
		switch {
		case s.Failed:
			n.State = "failed"
		case !s.Done && !status.Terminal():
			n.State = "current"
			if s.Type == WaitHuman {
				n.State = "waiting"
			}
		case !s.Done: // the run ended with this step still open
			n.State = "failed"
		}
		if last && status == Failed && n.State == "done" {
			n.State = "failed"
		}
		out.Steps = append(out.Steps, n)
	}
	return out
}

// goalOf is the run's task in a line: the input text, or the last user message.
func goalOf(raw []byte) string {
	var r Request
	if json.Unmarshal(raw, &r) != nil {
		return ""
	}
	text := r.Input.Text
	if r.Input.Messages != nil {
		for i := len(r.Input.Messages) - 1; i >= 0; i-- {
			if r.Input.Messages[i].Role == "user" {
				text = r.Input.Messages[i].Content.PlainText()
				break
			}
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) > 160 {
		text = string([]rune(text)[:157]) + "..."
	}
	return text
}

var ErrBadCursor = errors.New("runs: bad cursor")

// EncodeCursor / DecodeCursor make the keyset cursor: where the last row of a page was, as an opaque string.
func EncodeCursor(c Cursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.At.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()))
}

func DecodeCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return Cursor{}, ErrBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	return Cursor{At: t, ID: u}, nil
}

// likeEscape makes text safe to put between % signs in an ILIKE ... ESCAPE '\' pattern.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
