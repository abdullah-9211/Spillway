package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/money"
)

// Store is the runs and run_steps tables. Every method that depends on the time takes it as an argument, so a test
// can move the clock instead of waiting for a lease to expire.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const runColumns = `r.id, r.api_key_id, COALESCE(r.idempotency_key,''), r.status, r.request, r.limits, COALESCE(r.failure_reason,''),
  r.step_count, r.cost_usd, r.deadline_at, COALESCE(r.lease_owner,''), r.lease_expires_at, r.lease_epoch,
  r.cancel_requested_at IS NOT NULL, r.created_at, r.finished_at`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	var cost pgtype.Numeric
	var status string
	var limits []byte
	if err := row.Scan(&r.ID, &r.KeyID, &r.IdempotencyKey, &status, &r.RawRequest, &limits, &r.FailureReason, &r.StepCount, &cost, &r.DeadlineAt,
		&r.LeaseOwner, &r.LeaseExpiresAt, &r.LeaseEpoch, &r.CancelRequested, &r.CreatedAt, &r.FinishedAt); err != nil {
		return Run{}, err
	}
	r.Status = Status(status)
	var err error
	if r.Cost, err = db.MicrosFromNumeric(cost); err != nil {
		return Run{}, err
	}
	if err := json.Unmarshal(r.RawRequest, &r.Request); err != nil {
		return Run{}, fmt.Errorf("run %s has an unreadable request: %w", r.ID, err)
	}
	if err := json.Unmarshal(limits, &r.Limits); err != nil {
		return Run{}, fmt.Errorf("run %s has unreadable limits: %w", r.ID, err)
	}
	return r, nil
}

type CreateParams struct {
	KeyID          uuid.UUID
	IdempotencyKey string // optional
	Request        Request
	Raw            json.RawMessage // the body as received
	Limits         Limits
	Now            time.Time
}

// Create inserts a queued run, or returns the run an earlier call with the same Idempotency-Key made. The second
// result says whether this call created it. Reusing a key with a different body is ErrIdempotencyConflict.
func (s *Store) Create(ctx context.Context, p CreateParams) (Run, bool, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Run{}, false, err
	}
	limits, _ := json.Marshal(p.Limits)
	var idem any
	if p.IdempotencyKey != "" {
		idem = p.IdempotencyKey
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Run{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rolled back unless committed

	tag, err := tx.Exec(ctx, `INSERT INTO runs (id, api_key_id, idempotency_key, status, request, limits, deadline_at, created_at)
		VALUES ($1,$2,$3,'queued',$4,$5,$6,$7) ON CONFLICT (api_key_id, idempotency_key) DO NOTHING`,
		id, p.KeyID, idem, p.Raw, limits, p.Now.Add(p.Limits.Deadline), p.Now)
	if err != nil {
		return Run{}, false, err
	}
	if tag.RowsAffected() == 0 { // the key was used before
		var same bool
		var existing uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id, request = $3::jsonb FROM runs WHERE api_key_id=$1 AND idempotency_key=$2`, p.KeyID, p.IdempotencyKey, p.Raw).Scan(&existing, &same); err != nil {
			return Run{}, false, err
		}
		if !same {
			return Run{}, false, ErrIdempotencyConflict
		}
		r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM runs r WHERE r.id=$1`, existing))
		return r, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_steps (run_id, type, phase, payload, lease_epoch, worker_id, created_at)
		VALUES ($1,'run_status','finished',$2,0,'',$3)`, id, `{"status":"queued"}`, p.Now); err != nil {
		return Run{}, false, err
	}
	r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM runs r WHERE r.id=$1`, id))
	if err != nil {
		return Run{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Run{}, false, err
	}
	return r, true, nil
}

// Get returns a run, whoever owns it. The API uses GetForKey.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs r WHERE r.id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// GetForKey is Get for the key that created the run. Another key's run is ErrNotFound, so its existence does not leak.
func (s *Store) GetForKey(ctx context.Context, id, key uuid.UUID) (Run, error) {
	r, err := s.Get(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if r.KeyID != key {
		return Run{}, ErrNotFound
	}
	return r, nil
}

// Steps returns rows with an id greater than after, oldest first; limit <= 0 means all of them.
func (s *Store) Steps(ctx context.Context, run uuid.UUID, after int64, limit int) ([]Step, error) {
	q := `SELECT id, run_id, step_no, type, phase, COALESCE(idempotency_key,''), payload, cost_usd, lease_epoch, worker_id, created_at
		FROM run_steps WHERE run_id=$1 AND id>$2 ORDER BY id`
	args := []any{run, after}
	if limit > 0 {
		q += ` LIMIT $3`
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var st Step
		var typ, ph string
		var cost pgtype.Numeric
		if err := rows.Scan(&st.ID, &st.RunID, &st.StepNo, &typ, &ph, &st.IdempotencyKey, &st.Payload, &cost, &st.Epoch, &st.WorkerID, &st.At); err != nil {
			return nil, err
		}
		st.Type, st.Phase = StepType(typ), Phase(ph)
		if st.Cost, err = db.MicrosFromNumeric(cost); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// FinalAnswer is the text of the last finished model call, for a run that succeeded.
func (s *Store) FinalAnswer(ctx context.Context, run uuid.UUID) (string, error) {
	var payload []byte
	err := s.pool.QueryRow(ctx, `SELECT payload FROM run_steps WHERE run_id=$1 AND type='model_call' AND phase='finished' ORDER BY id DESC LIMIT 1`, run).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var p ModelFinished
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", err
	}
	return p.Message.Content.PlainText(), nil
}

func seconds(d time.Duration) float64 { return d.Seconds() }

// Claim takes the oldest run that no live worker holds: queued, or running with an expired lease (its worker died),
// or sleeping and due. It bumps the epoch, which is the fencing token. A nil run means there was nothing to claim.
func (s *Store) Claim(ctx context.Context, owner string, ttl time.Duration, now time.Time) (*Run, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rolled back unless committed

	var prev string
	row := tx.QueryRow(ctx, `WITH picked AS (
		SELECT id, status AS prev FROM runs
		WHERE (status IN ('queued','running','waiting_tool') AND (lease_expires_at IS NULL OR lease_expires_at < $3))
		   OR (status = 'sleeping' AND wake_at <= $3 AND (lease_expires_at IS NULL OR lease_expires_at < $3))
		ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
	UPDATE runs r SET lease_owner=$1, lease_expires_at=$3 + make_interval(secs => $2), lease_epoch = r.lease_epoch + 1,
	       status = CASE WHEN r.status IN ('queued','sleeping') THEN 'running' ELSE r.status END
	FROM picked WHERE r.id = picked.id
	RETURNING `+runColumns+`, picked.prev`, owner, seconds(ttl), now)
	var r Run
	var cost pgtype.Numeric
	var status string
	var limits []byte
	if err := row.Scan(&r.ID, &r.KeyID, &r.IdempotencyKey, &status, &r.RawRequest, &limits, &r.FailureReason, &r.StepCount, &cost, &r.DeadlineAt,
		&r.LeaseOwner, &r.LeaseExpiresAt, &r.LeaseEpoch, &r.CancelRequested, &r.CreatedAt, &r.FinishedAt, &prev); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.Status = Status(status)
	if r.Cost, err = db.MicrosFromNumeric(cost); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(r.RawRequest, &r.Request); err != nil {
		return nil, fmt.Errorf("run %s has an unreadable request: %w", r.ID, err)
	}
	if err := json.Unmarshal(limits, &r.Limits); err != nil {
		return nil, fmt.Errorf("run %s has unreadable limits: %w", r.ID, err)
	}
	if prev == "queued" || prev == "sleeping" {
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO run_steps (run_id, type, phase, payload, lease_epoch, worker_id, created_at)
			VALUES ($1,'run_status','finished',$2,$3,$4,$5) RETURNING id`, r.ID, `{"status":"running"}`, r.LeaseEpoch, owner, now).Scan(&id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_notify('run_steps', $1)`, fmt.Sprintf("%s:%d", r.ID, id)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &r, nil
}

// Lease identifies a claim: the worker and the epoch it was granted at.
type Lease struct {
	RunID uuid.UUID
	Owner string
	Epoch int64
}

// Heartbeat extends the lease. ok is false when the lease is gone (another worker claimed the run, or it ended);
// cancel reports a cancel request waiting for the holder.
func (s *Store) Heartbeat(ctx context.Context, l Lease, ttl time.Duration, now time.Time) (ok, cancel bool, err error) {
	err = s.pool.QueryRow(ctx, `UPDATE runs SET lease_expires_at = $4::timestamptz + make_interval(secs => $5)
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND status NOT IN ('succeeded','failed','cancelled')
		RETURNING cancel_requested_at IS NOT NULL`, l.RunID, l.Owner, l.Epoch, now, seconds(ttl)).Scan(&cancel)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, cancel, err
}

// Release gives the lease up (it expires now), so another worker can resume the run at once.
func (s *Store) Release(ctx context.Context, l Lease, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE runs SET lease_expires_at=$4 WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$3 AND status NOT IN ('succeeded','failed','cancelled')`,
		l.RunID, l.Owner, l.Epoch, now)
	return err
}

// Log returns the Log for one lease holder.
func (s *Store) Log(l Lease, now func() time.Time) Log { return &pgLog{s: s, l: l, now: now} }

type pgLog struct {
	s   *Store
	l   Lease
	now func() time.Time
}

// Append writes one row and updates the run's projection, in one transaction. The insert is fenced: it writes only if
// the run is still leased to this worker at this epoch, so a worker that was paused past its lease and wakes up cannot
// write. The terminal-row unique index is the second line of defence.
func (g *pgLog) Append(ctx context.Context, ns NewStep) (Step, error) {
	payload, err := json.Marshal(ns.Payload)
	if err != nil {
		return Step{}, err
	}
	var key any
	if ns.Key != "" {
		key = ns.Key
	}
	now := g.now()
	tx, err := g.s.pool.Begin(ctx)
	if err != nil {
		return Step{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rolled back unless committed

	st := Step{RunID: g.l.RunID, StepNo: ns.StepNo, Type: ns.Type, Phase: ns.Phase, IdempotencyKey: ns.Key, Payload: payload, Cost: ns.Cost, Epoch: g.l.Epoch, WorkerID: g.l.Owner, At: now}
	err = tx.QueryRow(ctx, `INSERT INTO run_steps (run_id, step_no, type, phase, idempotency_key, payload, cost_usd, lease_epoch, worker_id, created_at)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10
		WHERE EXISTS (SELECT 1 FROM runs WHERE id=$1 AND lease_owner=$9 AND lease_epoch=$8 AND status NOT IN ('succeeded','failed','cancelled'))
		RETURNING id`,
		g.l.RunID, ns.StepNo, string(ns.Type), string(ns.Phase), key, payload, db.NumericFromMicros(ns.Cost), g.l.Epoch, g.l.Owner, now).Scan(&st.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Step{}, ErrLeaseLost
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return Step{}, ErrDuplicateStep
	}
	if err != nil {
		return Step{}, err
	}

	// The projection, from the row just written.
	var inc int
	var status, reason *string
	finish := false
	switch {
	case ns.Type == RunStatus && ns.Phase == PhaseFinished:
		p := ns.Payload.(StatusPayload)
		s := string(p.Status)
		status = &s
		if p.Reason != "" {
			reason = &p.Reason
		}
		finish = p.Status.Terminal()
	case ns.Type == ToolCall && (ns.Phase == PhaseStarted || ns.Phase == PhaseReissued):
		s := string(WaitingTool)
		status = &s
		if ns.Phase == PhaseStarted {
			inc = 1
		}
	case ns.Type == ToolCall && ns.Phase.Terminal():
		s := string(Running)
		status = &s
	case ns.Phase == PhaseStarted:
		inc = 1
	}
	tag, err := tx.Exec(ctx, `UPDATE runs SET step_count = step_count + $3, cost_usd = cost_usd + $4,
		status = COALESCE($5, status), failure_reason = COALESCE($6, failure_reason),
		finished_at = CASE WHEN $7 THEN $8 ELSE finished_at END
		WHERE id=$1 AND lease_owner=$2 AND lease_epoch=$9`,
		g.l.RunID, g.l.Owner, inc, db.NumericFromMicros(ns.Cost), status, reason, finish, now, g.l.Epoch)
	if err != nil {
		return Step{}, err
	}
	if tag.RowsAffected() == 0 {
		return Step{}, ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify('run_steps', $1)`, fmt.Sprintf("%s:%d", g.l.RunID, st.ID)); err != nil {
		return Step{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Step{}, err
	}
	return st, nil
}

// RequestCancel cancels a run for the key that owns it. A run no worker is holding (queued, waiting, sleeping, or with
// an expired lease) is cancelled on the spot. A run a live worker holds gets a cancel request, which the worker sees
// within a heartbeat and acts on at once; the worker writes the final row. Cancelling twice is harmless.
func (s *Store) RequestCancel(ctx context.Context, id, key uuid.UUID, now time.Time) (Run, error) {
	return s.requestCancel(ctx, id, &key, now)
}

// CancelAny is RequestCancel for an admin of the dashboard, who may cancel any run, not only their own key's.
func (s *Store) CancelAny(ctx context.Context, id uuid.UUID, now time.Time) (Run, error) {
	return s.requestCancel(ctx, id, nil, now)
}

func (s *Store) requestCancel(ctx context.Context, id uuid.UUID, key *uuid.UUID, now time.Time) (Run, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rolled back unless committed

	var status string
	var owner string
	var expires *time.Time
	var epoch int64
	var keyID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT status, COALESCE(lease_owner,''), lease_expires_at, lease_epoch, api_key_id FROM runs WHERE id=$1 FOR UPDATE`, id).Scan(&status, &owner, &expires, &epoch, &keyID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && key != nil && keyID != *key) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	if Status(status).Terminal() {
		return Run{}, ErrFinished
	}
	held := (status == string(Running) || status == string(WaitingTool)) && expires != nil && expires.After(now)
	if held {
		if _, err := tx.Exec(ctx, `UPDATE runs SET cancel_requested_at = COALESCE(cancel_requested_at, $2) WHERE id=$1`, id, now); err != nil {
			return Run{}, err
		}
	} else {
		var rowID int64
		if err := tx.QueryRow(ctx, `INSERT INTO run_steps (run_id, type, phase, payload, lease_epoch, worker_id, created_at)
			VALUES ($1,'run_status','finished',$2,$3,'api',$4) RETURNING id`, id, `{"status":"cancelled","reason":"cancelled"}`, epoch, now).Scan(&rowID); err != nil {
			return Run{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET status='cancelled', failure_reason='cancelled', finished_at=$2, lease_expires_at=NULL WHERE id=$1`, id, now); err != nil {
			return Run{}, err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_notify('run_steps', $1)`, fmt.Sprintf("%s:%d", id, rowID)); err != nil {
			return Run{}, err
		}
	}
	r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM runs r WHERE r.id=$1`, id))
	if err != nil {
		return Run{}, err
	}
	return r, tx.Commit(ctx)
}

var _ = money.Micros(0)
