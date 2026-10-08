package usage

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/db/sqlcgen"
	"github.com/abdullah-9211/spillway/internal/money"
)

// Range is a span of whole UTC days, from the start of From to the start of To (so To is exclusive).
type Range struct {
	From, To time.Time
}

func (r Range) Days() int { return int(r.To.Sub(r.From).Hours() / 24) }

// MaxRangeDays bounds a report so one request cannot scan years of rows.
const MaxRangeDays = 366

const dateLayout = "2006-01-02"

var ErrBadRange = errors.New("bad date range")

// ParseRange reads inclusive UTC dates (YYYY-MM-DD). With none given the range is the last 14 days ending today.
func ParseRange(from, to string, now time.Time) (Range, error) {
	today := now.UTC().Truncate(24 * time.Hour)
	end, start := today, today.AddDate(0, 0, -13)
	var err error
	if to != "" {
		if end, err = time.Parse(dateLayout, to); err != nil {
			return Range{}, fmt.Errorf("%w: to must be a date like 2026-10-08", ErrBadRange)
		}
	}
	if from != "" {
		if start, err = time.Parse(dateLayout, from); err != nil {
			return Range{}, fmt.Errorf("%w: from must be a date like 2026-10-01", ErrBadRange)
		}
	} else if to != "" {
		start = end.AddDate(0, 0, -13)
	}
	if start.After(end) {
		return Range{}, fmt.Errorf("%w: from is after to", ErrBadRange)
	}
	r := Range{From: start, To: end.AddDate(0, 0, 1)}
	if r.Days() > MaxRangeDays {
		return Range{}, fmt.Errorf("%w: at most %d days at a time", ErrBadRange, MaxRangeDays)
	}
	return r, nil
}

// Metrics are the figures every grouping carries.
type Metrics struct {
	Requests     int64
	InputTokens  int64
	OutputTokens int64
	Cost         money.Micros
	Saved        money.Micros
	CacheHits    int64
	Errors       int64
}

type CacheSplit struct {
	Miss, HitExact, HitSemantic, Bypass int64
}

type Totals struct {
	Metrics
	Rejected int64 // refused by a rate limit or a budget before reaching a provider
	Cache    CacheSplit
}

// Group is one row of a breakdown: a day, a model or a key.
type Group struct {
	Key   string // the day (2026-10-08), the model id, or the key id
	Label string
	Metrics
	TimedRequests int64                   // requests the latency figures are based on (successful, not served from a cache)
	P50Ms, P95Ms  float64                 // model groups only
	Models        map[string]money.Micros // day groups only: that day's cost per model
}

type ProviderAttempts struct {
	Provider       string
	FailedAttempts int64
	FallbacksTo    int64
}

type Summary struct {
	Range     Range
	GroupBy   string
	Totals    Totals
	Groups    []Group
	Providers []ProviderAttempts
	Fallbacks int64
}

type Reader struct{ q *sqlcgen.Queries }

func NewReader(pool *pgxpool.Pool) *Reader { return &Reader{q: sqlcgen.New(pool)} }

var ErrBadGroup = errors.New("group_by must be day, model or key")

func nullKey(id *uuid.UUID) uuid.NullUUID {
	if id == nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: *id, Valid: true}
}

func micros(n pgtype.Numeric) (money.Micros, error) { return db.MicrosFromNumeric(n) }

// Summary builds the report for a range, optionally for one key, grouped by day, model or key.
func (r *Reader) Summary(ctx context.Context, rng Range, keyID *uuid.UUID, groupBy string) (*Summary, error) {
	if groupBy == "" {
		groupBy = "day"
	}
	if groupBy != "day" && groupBy != "model" && groupBy != "key" {
		return nil, ErrBadGroup
	}
	out := &Summary{Range: rng, GroupBy: groupBy}
	key := nullKey(keyID)

	t, err := r.q.ReportTotals(ctx, sqlcgen.ReportTotalsParams{FromTs: rng.From, ToTs: rng.To, KeyID: key})
	if err != nil {
		return nil, fmt.Errorf("report totals: %w", err)
	}
	cost, err := micros(t.CostUsd)
	if err != nil {
		return nil, err
	}
	saved, err := micros(t.SavedUsd)
	if err != nil {
		return nil, err
	}
	out.Totals = Totals{
		Metrics: Metrics{Requests: t.Requests, InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, Cost: cost, Saved: saved,
			CacheHits: t.CacheHitExact + t.CacheHitSemantic, Errors: t.Errors},
		Rejected: t.Rejected,
		Cache:    CacheSplit{Miss: t.CacheMiss, HitExact: t.CacheHitExact, HitSemantic: t.CacheHitSemantic, Bypass: t.CacheBypass},
	}

	switch groupBy {
	case "day":
		rows, err := r.q.ReportByDayModel(ctx, sqlcgen.ReportByDayModelParams{FromTs: rng.From, ToTs: rng.To, KeyID: key})
		if err != nil {
			return nil, fmt.Errorf("report by day: %w", err)
		}
		byDay := map[string]*Group{}
		for d := rng.From; d.Before(rng.To); d = d.AddDate(0, 0, 1) { // every day in the range, even a quiet one
			k := d.Format(dateLayout)
			byDay[k] = &Group{Key: k, Label: k, Models: map[string]money.Micros{}}
		}
		for _, row := range rows {
			g := byDay[row.Day.Time.UTC().Format(dateLayout)]
			if g == nil {
				continue
			}
			c, err := micros(row.CostUsd)
			if err != nil {
				return nil, err
			}
			s, err := micros(row.SavedUsd)
			if err != nil {
				return nil, err
			}
			g.Requests += row.Requests
			g.InputTokens += row.InputTokens
			g.OutputTokens += row.OutputTokens
			g.Cost += c
			g.Saved += s
			g.CacheHits += row.CacheHits
			g.Errors += row.Errors
			if row.Model != "" {
				g.Models[row.Model] += c
			}
		}
		for d := rng.From; d.Before(rng.To); d = d.AddDate(0, 0, 1) {
			out.Groups = append(out.Groups, *byDay[d.Format(dateLayout)])
		}

	case "model":
		rows, err := r.q.ReportByModel(ctx, sqlcgen.ReportByModelParams{FromTs: rng.From, ToTs: rng.To, KeyID: key})
		if err != nil {
			return nil, fmt.Errorf("report by model: %w", err)
		}
		for _, row := range rows {
			c, err := micros(row.CostUsd)
			if err != nil {
				return nil, err
			}
			s, err := micros(row.SavedUsd)
			if err != nil {
				return nil, err
			}
			label := row.Model
			if label == "" {
				label = "(no model: refused before routing)"
			}
			out.Groups = append(out.Groups, Group{Key: row.Model, Label: label,
				Metrics:       Metrics{Requests: row.Requests, InputTokens: row.InputTokens, OutputTokens: row.OutputTokens, Cost: c, Saved: s, CacheHits: row.CacheHits, Errors: row.Errors},
				TimedRequests: row.TimedRequests, P50Ms: row.P50Ms, P95Ms: row.P95Ms})
		}

	case "key":
		rows, err := r.q.ReportByKey(ctx, sqlcgen.ReportByKeyParams{FromTs: rng.From, ToTs: rng.To, KeyID: key})
		if err != nil {
			return nil, fmt.Errorf("report by key: %w", err)
		}
		for _, row := range rows {
			c, err := micros(row.CostUsd)
			if err != nil {
				return nil, err
			}
			s, err := micros(row.SavedUsd)
			if err != nil {
				return nil, err
			}
			id := ""
			if row.KeyID.Valid {
				id = row.KeyID.UUID.String()
			}
			out.Groups = append(out.Groups, Group{Key: id, Label: row.KeyName,
				Metrics: Metrics{Requests: row.Requests, InputTokens: row.InputTokens, OutputTokens: row.OutputTokens, Cost: c, Saved: s, CacheHits: row.CacheHits, Errors: row.Errors}})
		}
	}

	prov, err := r.q.ReportProviderAttempts(ctx, sqlcgen.ReportProviderAttemptsParams{FromTs: rng.From, ToTs: rng.To, KeyID: key})
	if err != nil {
		return nil, fmt.Errorf("report providers: %w", err)
	}
	for _, p := range prov {
		out.Providers = append(out.Providers, ProviderAttempts{Provider: p.Provider, FailedAttempts: p.FailedAttempts, FallbacksTo: p.FallbacksTo})
		out.Fallbacks += p.FallbacksTo
	}
	return out, nil
}

// --- the request log ---

type RequestFilter struct {
	Range   Range
	KeyID   *uuid.UUID
	Model   string
	Outcome string
	Policy  string
	Limit   int
	Cursor  string
}

type RequestRow struct {
	ID           uuid.UUID
	KeyID        *uuid.UUID
	KeyName      string
	Policy       string
	Provider     string
	Model        string
	InputTokens  int
	OutputTokens int
	Cost         money.Micros
	Saved        money.Micros
	LatencyMs    int
	TTFBMs       *int
	CacheStatus  string
	Outcome      string
	Attempts     []Attempt
	CreatedAt    time.Time
}

var ErrBadCursor = errors.New("bad cursor")

func encodeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(c string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, ErrBadCursor
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, ErrBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, uuid.Nil, ErrBadCursor
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return time.Time{}, uuid.Nil, ErrBadCursor
	}
	return t, u, nil
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

const DefaultPageSize, MaxPageSize = 50, 200

// Requests lists requests newest first, with the attempts each one made. next is empty on the last page.
func (r *Reader) Requests(ctx context.Context, f RequestFilter) (rows []RequestRow, next string, err error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	p := sqlcgen.ReportRequestsParams{FromTs: f.Range.From, ToTs: f.Range.To, KeyID: nullKey(f.KeyID),
		Model: text(f.Model), Outcome: text(f.Outcome), Policy: text(f.Policy), RowLimit: int32(limit + 1)}
	if f.Cursor != "" {
		t, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		p.BeforeTs, p.BeforeID = &t, uuid.NullUUID{UUID: id, Valid: true}
	}
	raw, err := r.q.ReportRequests(ctx, p)
	if err != nil {
		return nil, "", fmt.Errorf("list requests: %w", err)
	}
	more := len(raw) > limit
	if more {
		raw = raw[:limit]
	}
	for _, x := range raw {
		c, err := micros(x.CostUsd)
		if err != nil {
			return nil, "", err
		}
		s, err := micros(x.SavedUsd)
		if err != nil {
			return nil, "", err
		}
		row := RequestRow{ID: x.ID, KeyName: x.KeyName, Policy: x.Policy, Provider: x.Provider.String, Model: x.Model.String,
			InputTokens: int(x.InputTokens), OutputTokens: int(x.OutputTokens), Cost: c, Saved: s, LatencyMs: int(x.LatencyMs),
			CacheStatus: x.CacheStatus, Outcome: x.Outcome, CreatedAt: x.CreatedAt.UTC()}
		if x.ApiKeyID.Valid {
			id := x.ApiKeyID.UUID
			row.KeyID = &id
		}
		if x.TtfbMs.Valid {
			v := int(x.TtfbMs.Int32)
			row.TTFBMs = &v
		}
		if len(x.Attempts) > 0 {
			if err := json.Unmarshal(x.Attempts, &row.Attempts); err != nil {
				return nil, "", fmt.Errorf("attempts of %s: %w", x.ID, err)
			}
		}
		rows = append(rows, row)
	}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return rows, next, nil
}

// --- CSV ---

var csvHeader = []string{"day", "key", "model", "requests", "input_tokens", "output_tokens", "cost_usd", "saved_usd", "cache_hits", "errors"}

// safeCell stops a spreadsheet from running a key name such as =HYPERLINK(...) as a formula.
func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// WriteCSV writes one row per day, key and model, with exact dollar amounts.
func (r *Reader) WriteCSV(ctx context.Context, w io.Writer, rng Range, keyID *uuid.UUID) error {
	rows, err := r.q.ReportCSVRows(ctx, sqlcgen.ReportCSVRowsParams{FromTs: rng.From, ToTs: rng.To, KeyID: nullKey(keyID)})
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return err
	}
	for _, x := range rows {
		c, err := micros(x.CostUsd)
		if err != nil {
			return err
		}
		s, err := micros(x.SavedUsd)
		if err != nil {
			return err
		}
		i := strconv.FormatInt
		if err := cw.Write([]string{x.Day.Time.UTC().Format(dateLayout), safeCell(x.KeyName), safeCell(x.Model), i(x.Requests, 10),
			i(x.InputTokens, 10), i(x.OutputTokens, 10), c.String(), s.String(), i(x.CacheHits, 10), i(x.Errors, 10)}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// SortedModels returns model ids ordered by total cost, highest first, for stable chart series.
func SortedModels(days []Group) []string {
	total := map[string]money.Micros{}
	for _, d := range days {
		for m, c := range d.Models {
			total[m] += c
		}
	}
	names := make([]string, 0, len(total))
	for m := range total {
		names = append(names, m)
	}
	sort.Slice(names, func(i, j int) bool {
		if total[names[i]] != total[names[j]] {
			return total[names[i]] > total[names[j]]
		}
		return names[i] < names[j]
	})
	return names
}
