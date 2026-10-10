//go:build integration

package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/testdb"
)

var t0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

type env struct {
	pool  *pgxpool.Pool
	store *Store
	key   keys.Key
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool, _ := testdb.New(t)
	k, _, err := keys.NewStore(pool).Create(context.Background(), keys.CreateParams{Name: "runs-test"})
	if err != nil {
		t.Fatal(err)
	}
	return &env{pool: pool, store: NewStore(pool), key: k}
}

func (e *env) create(t *testing.T, idem string, r Request, now time.Time) Run {
	t.Helper()
	raw, _ := json.Marshal(r)
	run, _, err := e.store.Create(context.Background(), CreateParams{KeyID: e.key.ID, IdempotencyKey: idem, Request: r, Raw: raw,
		Limits: Limits{MaxSteps: 50, MaxCost: 1_000_000, Deadline: 15 * time.Minute}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (e *env) rows(t *testing.T, id uuid.UUID) []Step {
	t.Helper()
	s, err := e.store.Steps(context.Background(), id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateIsIdempotentPerKeyAndRecordsTheQueuedRow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := Request{Input: Input{Text: "hello"}}
	raw, _ := json.Marshal(r)
	p := CreateParams{KeyID: e.key.ID, IdempotencyKey: "abc", Request: r, Raw: raw, Limits: Limits{MaxSteps: 5, MaxCost: 250_000, Deadline: time.Minute}, Now: t0}

	a, created, err := e.store.Create(ctx, p)
	if err != nil || !created {
		t.Fatalf("first create: %v %v", created, err)
	}
	if a.Status != Queued || a.Limits.MaxSteps != 5 || a.Limits.MaxCost != 250_000 || !a.DeadlineAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("run = %+v", a)
	}
	if got := outline(e.rows(t, a.ID)); len(got) != 1 || got[0] != "run_status queued" {
		t.Errorf("rows = %v", got)
	}
	b, created, err := e.store.Create(ctx, p)
	if err != nil || created || b.ID != a.ID {
		t.Fatalf("second create with the same key and body: %v created=%v same=%v", err, created, b.ID == a.ID)
	}
	if n := len(e.rows(t, a.ID)); n != 1 {
		t.Errorf("a repeated create must not add rows: %d", n)
	}
	other := p
	other.Request, other.Raw = Request{Input: Input{Text: "different"}}, json.RawMessage(`{"input":"different"}`)
	if _, _, err := e.store.Create(ctx, other); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("same key, different body: %v", err)
	}
	// Without a key there is no deduplication, and the same key under another API key is independent.
	p.IdempotencyKey = ""
	c, _, _ := e.store.Create(ctx, p)
	d, _, _ := e.store.Create(ctx, p)
	if c.ID == d.ID {
		t.Error("runs without an idempotency key are separate")
	}
	k2, _, _ := keys.NewStore(e.pool).Create(ctx, keys.CreateParams{Name: "other"})
	p.KeyID, p.IdempotencyKey = k2.ID, "abc"
	if f, created, err := e.store.Create(ctx, p); err != nil || !created || f.ID == a.ID {
		t.Errorf("another key may reuse the idempotency key: %v %v", created, err)
	}
}

func TestARunIsVisibleOnlyToItsKey(t *testing.T) {
	e := newEnv(t)
	run := e.create(t, "", Request{Input: Input{Text: "x"}}, t0)
	if _, err := e.store.GetForKey(context.Background(), run.ID, e.key.ID); err != nil {
		t.Error(err)
	}
	if _, err := e.store.GetForKey(context.Background(), run.ID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("another key: %v", err)
	}
	if _, err := e.store.GetForKey(context.Background(), uuid.New(), e.key.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestClaimOrderLeaseAndEpoch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first := e.create(t, "", Request{Input: Input{Text: "1"}}, t0)
	second := e.create(t, "", Request{Input: Input{Text: "2"}}, t0.Add(time.Second))

	a, err := e.store.Claim(ctx, "w-a", 30*time.Second, t0)
	if err != nil || a == nil || a.ID != first.ID || a.LeaseEpoch != 1 || a.Status != Running {
		t.Fatalf("first claim: %+v %v", a, err)
	}
	if got := outline(e.rows(t, first.ID)); got[len(got)-1] != "run_status running" {
		t.Errorf("claiming a queued run records that it is running: %v", got)
	}
	b, _ := e.store.Claim(ctx, "w-b", 30*time.Second, t0)
	if b == nil || b.ID != second.ID {
		t.Fatalf("the second worker gets the next run, not the held one: %+v", b)
	}
	if c, _ := e.store.Claim(ctx, "w-c", 30*time.Second, t0.Add(29*time.Second)); c != nil {
		t.Fatalf("both leases are still live at +29s, but w-c got %s", c.ID)
	}
	// +31s: the leases expired, as if both workers died. The older run goes first and the epoch is bumped.
	c, _ := e.store.Claim(ctx, "w-c", 30*time.Second, t0.Add(31*time.Second))
	if c == nil || c.ID != first.ID || c.LeaseEpoch != 2 || c.LeaseOwner != "w-c" {
		t.Fatalf("recovery claim: %+v", c)
	}
	if n := len(e.rows(t, first.ID)); n != 2 {
		t.Errorf("a recovered run is already running, so no second running row: %d rows", n)
	}
}

func TestHeartbeatKeepsTheLeaseAndTellsAStaleHolderItLostIt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.create(t, "", Request{Input: Input{Text: "x"}}, t0)
	a, _ := e.store.Claim(ctx, "w-a", 30*time.Second, t0)
	la := Lease{RunID: a.ID, Owner: "w-a", Epoch: a.LeaseEpoch}

	if ok, cancel, err := e.store.Heartbeat(ctx, la, 30*time.Second, t0.Add(20*time.Second)); err != nil || !ok || cancel {
		t.Fatalf("heartbeat: ok=%v cancel=%v err=%v", ok, cancel, err)
	}
	if b, _ := e.store.Claim(ctx, "w-b", 30*time.Second, t0.Add(31*time.Second)); b != nil {
		t.Fatal("a heartbeat at +20s keeps the lease past +31s")
	}
	b, _ := e.store.Claim(ctx, "w-b", 30*time.Second, t0.Add(51*time.Second))
	if b == nil || b.LeaseEpoch != 2 {
		t.Fatalf("after the heartbeat's lease ran out: %+v", b)
	}
	if ok, _, _ := e.store.Heartbeat(ctx, la, 30*time.Second, t0.Add(52*time.Second)); ok {
		t.Error("the old holder's heartbeat must report that the lease is gone")
	}
}

func TestReleaseMakesTheRunClaimableAtOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.create(t, "", Request{Input: Input{Text: "x"}}, t0)
	a, _ := e.store.Claim(ctx, "w-a", 30*time.Second, t0)
	if err := e.store.Release(ctx, Lease{RunID: a.ID, Owner: "w-a", Epoch: a.LeaseEpoch}, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	b, _ := e.store.Claim(ctx, "w-b", 30*time.Second, t0.Add(2*time.Second))
	if b == nil || b.ID != a.ID || b.LeaseEpoch != 2 {
		t.Errorf("claim after release: %+v", b)
	}
	// A stale release must not free somebody else's lease.
	if err := e.store.Release(ctx, Lease{RunID: a.ID, Owner: "w-a", Epoch: 1}, t0.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if c, _ := e.store.Claim(ctx, "w-c", 30*time.Second, t0.Add(4*time.Second)); c != nil {
		t.Error("a stale release freed a live lease")
	}
}

func TestFencingRejectsAZombieWorker(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.create(t, "", Request{Input: Input{Text: "x"}}, t0)
	a, _ := e.store.Claim(ctx, "w-a", 30*time.Second, t0)
	logA := e.store.Log(Lease{RunID: a.ID, Owner: "w-a", Epoch: a.LeaseEpoch}, func() time.Time { return t0 })
	if _, err := logA.Append(ctx, NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseStarted, Payload: ModelStarted{Policy: "default"}}); err != nil {
		t.Fatal(err)
	}

	// w-a is paused past its lease; w-b claims the run (epoch 2).
	b, _ := e.store.Claim(ctx, "w-b", 30*time.Second, t0.Add(31*time.Second))
	logB := e.store.Log(Lease{RunID: b.ID, Owner: "w-b", Epoch: b.LeaseEpoch}, func() time.Time { return t0.Add(32 * time.Second) })

	// w-a wakes up and tries to finish the step it started. The fence refuses.
	_, err := logA.Append(ctx, NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseFinished, Cost: 5000, Payload: ModelFinished{Message: text("zombie")}})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("zombie write: %v, want ErrLeaseLost", err)
	}
	// w-b writes fine and its row carries its own worker and epoch.
	if _, err := logB.Append(ctx, NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseReissued, Payload: Reissued{PreviousWorker: "w-a", PreviousEpoch: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := logB.Append(ctx, NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseFinished, Cost: 7000, Payload: ModelFinished{Message: text("real")}}); err != nil {
		t.Fatal(err)
	}
	rows := e.rows(t, run.ID)
	for _, s := range rows {
		if strings.Contains(string(s.Payload), "zombie") {
			t.Fatal("the zombie's row was stored")
		}
	}
	re := rows[len(rows)-2]
	if re.Phase != PhaseReissued || re.WorkerID != "w-b" || re.Epoch != 2 {
		t.Errorf("reissued row = %+v", re)
	}
	got, _ := e.store.Get(ctx, run.ID)
	if got.Cost != 7000 {
		t.Errorf("the zombie's cost leaked into the projection: %d", got.Cost)
	}
}

func TestOneTerminalRowPerStepEvenIfTheFenceWereBypassed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.create(t, "", Request{Input: Input{Text: "x"}}, t0)
	insert := func(epoch int64) error {
		_, err := e.pool.Exec(ctx, `INSERT INTO run_steps (run_id, step_no, type, phase, payload, lease_epoch, worker_id) VALUES ($1,1,'model_call','finished','{}',$2,'w')`, run.ID, epoch)
		return err
	}
	if err := insert(1); err != nil {
		t.Fatal(err)
	}
	if err := insert(2); err == nil || !strings.Contains(err.Error(), "run_steps_terminal") {
		t.Errorf("a second terminal row for step 1 under another epoch must violate the index: %v", err)
	}
}

func TestAppendUpdatesTheProjectionInTheSameTransaction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.create(t, "", Request{Input: Input{Text: "x"}}, t0)
	a, _ := e.store.Claim(ctx, "w-a", 30*time.Second, t0)
	log := e.store.Log(Lease{RunID: a.ID, Owner: "w-a", Epoch: a.LeaseEpoch}, func() time.Time { return t0.Add(time.Second) })
	add := func(ns NewStep) {
		t.Helper()
		if _, err := log.Append(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
	state := func() Run { r, _ := e.store.Get(ctx, run.ID); return r }

	add(NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseStarted, Payload: ModelStarted{}})
	if r := state(); r.StepCount != 1 || r.Status != Running {
		t.Errorf("after a model step started: %+v", r)
	}
	add(NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseFinished, Cost: 1234, Payload: ModelFinished{Message: calls("c1")}})
	add(NewStep{StepNo: ptr(2), Type: ToolCall, Phase: PhaseStarted, Key: IdempotencyKey(run.ID, 2), Payload: ToolStarted{Tool: "echo", ToolCallID: "c1"}})
	if r := state(); r.StepCount != 2 || r.Status != WaitingTool || r.Cost != 1234 {
		t.Errorf("while a tool runs: %+v", r)
	}
	add(NewStep{StepNo: ptr(2), Type: ToolCall, Phase: PhaseFinished, Key: IdempotencyKey(run.ID, 2), Payload: ToolFinished{Result: "ok"}})
	if r := state(); r.Status != Running {
		t.Errorf("after the tool: %+v", r)
	}
	add(NewStep{Type: RunStatus, Phase: PhaseFinished, Payload: StatusPayload{Status: Failed, Reason: ReasonMaxSteps}})
	r := state()
	if r.Status != Failed || r.FailureReason != ReasonMaxSteps || r.FinishedAt == nil {
		t.Errorf("after the final row: %+v", r)
	}
	rows := e.rows(t, run.ID)
	if tool := rows[4]; tool.IdempotencyKey != IdempotencyKey(run.ID, 2) || tool.Type != ToolCall {
		t.Errorf("the tool row carries its idempotency key: %+v", tool)
	}
	// A finished run accepts no more rows, from anyone.
	if _, err := log.Append(ctx, NewStep{StepNo: ptr(3), Type: ModelCall, Phase: PhaseStarted, Payload: ModelStarted{}}); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("append after the run ended: %v", err)
	}
}

func TestRequestCancel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// A queued run is cancelled on the spot, with its final row.
	q := e.create(t, "", Request{Input: Input{Text: "q"}}, t0)
	r, err := e.store.RequestCancel(ctx, q.ID, e.key.ID, t0)
	if err != nil || r.Status != Cancelled || r.FailureReason != ReasonCancelled || r.FinishedAt == nil {
		t.Fatalf("cancel queued: %+v %v", r, err)
	}
	if got := outline(e.rows(t, q.ID)); got[len(got)-1] != "run_status cancelled:cancelled" {
		t.Errorf("rows = %v", got)
	}
	if c, _ := e.store.Claim(ctx, "w", 30*time.Second, t0); c != nil {
		t.Error("a cancelled run must not be claimable")
	}
	if _, err := e.store.RequestCancel(ctx, q.ID, e.key.ID, t0); !errors.Is(err, ErrFinished) {
		t.Errorf("cancelling twice: %v", err)
	}
	if _, err := e.store.RequestCancel(ctx, q.ID, uuid.New(), t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("another key: %v", err)
	}

	// A run a live worker holds is only marked; the worker sees it on its next heartbeat.
	held := e.create(t, "", Request{Input: Input{Text: "held"}}, t0.Add(time.Second))
	a, _ := e.store.Claim(ctx, "w-a", 30*time.Second, t0.Add(time.Second))
	r, err = e.store.RequestCancel(ctx, held.ID, e.key.ID, t0.Add(2*time.Second))
	if err != nil || r.Status != Running || !r.CancelRequested {
		t.Fatalf("cancel held: %+v %v", r, err)
	}
	if ok, cancel, _ := e.store.Heartbeat(ctx, Lease{RunID: a.ID, Owner: "w-a", Epoch: a.LeaseEpoch}, 30*time.Second, t0.Add(3*time.Second)); !ok || !cancel {
		t.Errorf("heartbeat should report the cancel: ok=%v cancel=%v", ok, cancel)
	}

	// A run whose worker died (lease expired) is cancelled directly.
	dead := e.create(t, "", Request{Input: Input{Text: "dead"}}, t0.Add(4*time.Second))
	_, _ = e.store.Claim(ctx, "w-x", time.Second, t0.Add(4*time.Second))
	if r, err = e.store.RequestCancel(ctx, dead.ID, e.key.ID, t0.Add(10*time.Second)); err != nil || r.Status != Cancelled {
		t.Errorf("cancel with an expired lease: %+v %v", r, err)
	}
}

// --- the pool against the real database ---

func poolFor(e *env, m ModelCaller, tl ToolRunner, owner string, now func() time.Time) *Pool {
	eng := &Engine{Model: m, Tools: tl, Now: now, Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
	return NewPool(e.store, eng, PoolOptions{Workers: 1, LeaseTTL: 30 * time.Second, Heartbeat: 10 * time.Second, Owner: owner, Now: now})
}

func TestPoolRunsAMultiStepRunToTheEnd(t *testing.T) {
	e := newEnv(t)
	run := e.create(t, "", Request{Input: Input{Text: "go"}, Tools: []string{"echo"}}, time.Now())
	p := poolFor(e, &scriptedModel{Calls: 2}, &echoTool{}, "w-1", time.Now)
	if found, err := p.ClaimAndWork(context.Background(), context.Background()); err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
	got, _ := e.store.Get(context.Background(), run.ID)
	if got.Status != Succeeded || got.StepCount != 5 || got.Cost != 3000 || got.FinishedAt == nil {
		t.Errorf("run = %+v", got)
	}
	answer, _ := e.store.FinalAnswer(context.Background(), run.ID)
	if answer != `done: echo:{"n":0},echo:{"n":1}` {
		t.Errorf("answer = %q", answer)
	}
	checkStepNumbers(t, e.rows(t, run.ID))
	if found, _ := p.ClaimAndWork(context.Background(), context.Background()); found {
		t.Error("nothing is left to claim")
	}
}

func TestAnotherWorkerFinishesARunWhoseWorkerWasKilled(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.create(t, "", Request{Input: Input{Text: "go"}}, t0)

	// w-1 claims the run, starts the model call, and is then killed with SIGKILL: no release, no more rows.
	a, _ := e.store.Claim(ctx, "w-1", 30*time.Second, t0)
	logA := e.store.Log(Lease{RunID: a.ID, Owner: "w-1", Epoch: a.LeaseEpoch}, func() time.Time { return t0 })
	if _, err := logA.Append(ctx, NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseStarted, Payload: ModelStarted{Policy: "default"}}); err != nil {
		t.Fatal(err)
	}

	// While the lease is live nobody else may take the run.
	early := poolFor(e, &scriptedModel{}, nil, "w-2", func() time.Time { return t0.Add(10 * time.Second) })
	if found, _ := early.ClaimAndWork(ctx, ctx); found {
		t.Fatal("the run was taken while its worker's lease was live")
	}
	// After the lease expires w-2 takes over and re-issues the step.
	late := poolFor(e, &scriptedModel{}, nil, "w-2", func() time.Time { return t0.Add(31 * time.Second) })
	if found, err := late.ClaimAndWork(ctx, ctx); err != nil || !found {
		t.Fatalf("takeover: %v %v", found, err)
	}
	rows := e.rows(t, run.ID)
	want := []string{"run_status queued", "run_status running", "1 model_call started", "1 model_call reissued", "1 model_call finished", "run_status succeeded"}
	if got := outline(rows); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %v", got)
	}
	re := rows[3]
	var rp Reissued
	_ = json.Unmarshal(re.Payload, &rp)
	if re.WorkerID != "w-2" || re.Epoch != 2 || rp.PreviousWorker != "w-1" || rp.PreviousEpoch != 1 {
		t.Errorf("reissued row: worker %s epoch %d payload %s", re.WorkerID, re.Epoch, re.Payload)
	}
	if rows[2].WorkerID != "w-1" || rows[2].Epoch != 1 {
		t.Error("the stopped attempt stays in the history under w-1")
	}
	// The killed worker wakes up as a zombie and tries to write: refused.
	if _, err := logA.Append(ctx, NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseFinished, Payload: ModelFinished{Message: text("late")}}); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("zombie append: %v", err)
	}
}

func TestShutdownReleasesTheLeaseAndLeavesTheStepOpenForTheNextWorker(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.create(t, "", Request{Input: Input{Text: "go"}}, time.Now())
	entered := make(chan struct{})
	m := &scriptedModel{hook: func(c context.Context) error { close(entered); <-c.Done(); return context.Cause(c) }}
	eng := &Engine{Model: m, Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
	p := NewPool(e.store, eng, PoolOptions{Workers: 1, Heartbeat: 50 * time.Millisecond, LeaseTTL: 500 * time.Millisecond, Poll: 10 * time.Millisecond, Grace: 100 * time.Millisecond, Owner: "w-1"})

	stop, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { p.Run(stop); close(done) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never started the model call")
	}
	cancel() // SIGTERM
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the pool did not stop after its grace period")
	}
	got, _ := e.store.Get(ctx, run.ID)
	if got.Status.Terminal() || got.LeaseExpiresAt == nil || got.LeaseExpiresAt.After(time.Now().Add(50*time.Millisecond)) {
		t.Errorf("the lease should be released and the run not failed: %+v", got)
	}
	if rows := outline(e.rows(t, run.ID)); rows[len(rows)-1] != "1 model_call started" {
		t.Errorf("a shutdown writes nothing more: %v", rows)
	}
	// Another worker resumes it at once.
	p2 := poolFor(e, &scriptedModel{}, nil, "w-2", time.Now)
	if found, err := p2.ClaimAndWork(ctx, ctx); err != nil || !found {
		t.Fatalf("resume: %v %v", found, err)
	}
	if got, _ := e.store.Get(ctx, run.ID); got.Status != Succeeded {
		t.Errorf("resumed run = %+v", got)
	}
}

func TestACancelReachesARunningWorkerThroughItsHeartbeat(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.create(t, "", Request{Input: Input{Text: "go"}}, time.Now())
	entered := make(chan struct{})
	m := &scriptedModel{hook: func(c context.Context) error { close(entered); <-c.Done(); return context.Cause(c) }}
	eng := &Engine{Model: m, Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
	p := NewPool(e.store, eng, PoolOptions{Workers: 1, Heartbeat: 30 * time.Millisecond, LeaseTTL: time.Second, Poll: 10 * time.Millisecond, Owner: "w-1"})
	stop, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(stop); close(done) }()
	<-entered
	if _, err := e.store.RequestCancel(ctx, run.ID, e.key.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r, _ := e.store.Get(ctx, run.ID); r.Status == Cancelled {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := e.store.Get(ctx, run.ID)
	if got.Status != Cancelled || got.FailureReason != ReasonCancelled {
		t.Fatalf("run = %+v", got)
	}
	if rows := outline(e.rows(t, run.ID)); fmt.Sprint(rows[len(rows)-3:]) != "[1 model_call started 1 model_call failed run_status cancelled:cancelled]" {
		t.Errorf("rows = %v", rows)
	}
	cancel()
	<-done
}

func TestAdminCancelAnyAndTheDashboardCreator(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := &Creator{Store: e.store, Key: func(context.Context) (keys.Key, error) { return e.key, nil },
		Caps: Caps{MaxSteps: 10, MaxCost: 500_000, Deadline: time.Hour}, Known: func(m string) bool { return m == "default" }, Now: func() time.Time { return t0 }}

	raw := []byte(`{"input":"hello","limits":{"max_steps":3,"max_cost_usd":9}}`)
	var req Request
	_ = json.Unmarshal(raw, &req)
	run, err := c.Create(ctx, req, raw)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.Get(ctx, run.ID)
	if got.KeyID != e.key.ID || got.Status != Queued || got.Limits.MaxSteps != 3 || got.Limits.MaxCost != 500_000 {
		t.Errorf("created run = %+v: made under the service's key, with the server cap applied", got)
	}
	bad := Request{Input: Input{Text: "x"}, Model: "nope"}
	if _, err := c.Create(ctx, bad, []byte(`{}`)); err == nil {
		t.Error("an unknown model is refused")
	} else if _, ok := err.(*Validation); !ok {
		t.Errorf("as a validation error: %T", err)
	}

	// An admin can cancel a run that belongs to a key other than the service's own.
	other := e.create(t, "", Request{Input: Input{Text: "someone else's"}}, t0)
	if _, err := e.store.RequestCancel(ctx, other.ID, uuid.New(), t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a key cannot cancel another key's run: %v", err)
	}
	r, err := e.store.CancelAny(ctx, other.ID, t0)
	if err != nil || r.Status != Cancelled {
		t.Errorf("CancelAny: %+v %v", r, err)
	}
	if _, err := e.store.CancelAny(ctx, other.ID, t0); !errors.Is(err, ErrFinished) {
		t.Errorf("twice: %v", err)
	}
	if _, err := e.store.CancelAny(ctx, uuid.New(), t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
}

// --- approvals and sleeping against the real database ---

func approvalModel() *fnModel {
	return &fnModel{turns: func(n int, msgs []provider.Message) provider.Message {
		if n == 0 {
			return toolCall("c0", BuiltinApproval, `{"reason":"ship it?"}`)
		}
		return provider.Message{Role: "assistant", Content: provider.TextContent("result: " + lastToolText(msgs))}
	}}
}

func TestApprovalParksTheRunAndAnApproveResumesItOnAnotherWorker(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.create(t, "", Request{Input: Input{Text: "go"}}, t0)
	clock := func() time.Time { return t0 }

	w1 := poolFor(e, approvalModel(), nil, "w-1", clock)
	if found, err := w1.ClaimAndWork(ctx, ctx); err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
	got, _ := e.store.Get(ctx, run.ID)
	if got.Status != WaitingHuman {
		t.Fatalf("status = %s", got.Status)
	}
	// Parked: nobody claims it, however long we wait.
	later := poolFor(e, approvalModel(), nil, "w-2", func() time.Time { return t0.Add(48 * time.Hour) })
	if found, _ := later.ClaimAndWork(ctx, ctx); found {
		t.Fatal("a waiting run was claimed")
	}

	// w-1 is gone for good. The approval is a row in the log; any worker continues.
	r, err := e.store.Decide(ctx, run.ID, &e.key.ID, Decision{Decision: "approve", By: "ana", Note: "ok"}, t0.Add(time.Minute))
	if err != nil || r.Status != Running {
		t.Fatalf("decide: %+v %v", r, err)
	}
	if _, err := e.store.Decide(ctx, run.ID, &e.key.ID, Decision{Decision: "approve", By: "ana"}, t0); !errors.Is(err, ErrNotWaiting) {
		t.Errorf("second decision: %v", err)
	}
	w2 := poolFor(e, approvalModel(), nil, "w-2", func() time.Time { return t0.Add(2 * time.Minute) })
	if found, err := w2.ClaimAndWork(ctx, ctx); err != nil || !found {
		t.Fatalf("resume: %v %v", found, err)
	}
	got, _ = e.store.Get(ctx, run.ID)
	if got.Status != Succeeded {
		t.Fatalf("status = %s", got.Status)
	}
	if a, _ := e.store.FinalAnswer(ctx, run.ID); a != "result: Approved by ana. Note: ok" {
		t.Errorf("answer = %q", a)
	}
	checkStepNumbers(t, e.rows(t, run.ID))
}

func TestRejectFailsTheRunAndDecideChecksOwnership(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	run := e.create(t, "", Request{Input: Input{Text: "go"}}, t0)
	if _, err := e.store.Decide(ctx, run.ID, &e.key.ID, Decision{Decision: "approve", By: "a"}, t0); !errors.Is(err, ErrNotWaiting) {
		t.Errorf("deciding a queued run: %v", err)
	}
	p := poolFor(e, approvalModel(), nil, "w-1", func() time.Time { return t0 })
	if found, err := p.ClaimAndWork(ctx, ctx); err != nil || !found {
		t.Fatal(found, err)
	}
	other := uuid.New()
	if _, err := e.store.Decide(ctx, run.ID, &other, Decision{Decision: "reject", By: "a"}, t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("another key: %v", err)
	}
	r, err := e.store.Decide(ctx, run.ID, nil, Decision{Decision: "reject", By: "admin", Note: "too risky"}, t0.Add(time.Second))
	if err != nil || r.Status != Failed || r.FailureReason != ReasonRejected || r.FinishedAt == nil {
		t.Fatalf("reject: %+v %v", r, err)
	}
	if _, err := e.store.Decide(ctx, run.ID, nil, Decision{Decision: "approve", By: "a"}, t0); !errors.Is(err, ErrFinished) {
		t.Errorf("deciding a finished run: %v", err)
	}
	want := []string{"run_status queued", "run_status running", "1 model_call started", "1 model_call finished", "2 wait_human started", "2 wait_human finished", "run_status failed:rejected"}
	if got := outline(e.rows(t, run.ID)); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("rows = %v", got)
	}
}

func TestSleepingRunWakesOnlyAtItsTime(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := &fnModel{turns: func(n int, msgs []provider.Message) provider.Message {
		if n == 0 {
			return toolCall("c0", BuiltinSleep, `{"seconds":600}`)
		}
		return provider.Message{Role: "assistant", Content: provider.TextContent(lastToolText(msgs))}
	}}
	run := e.create(t, "", Request{Input: Input{Text: "go"}}, t0)
	if found, err := poolFor(e, m, nil, "w-1", func() time.Time { return t0 }).ClaimAndWork(ctx, ctx); err != nil || !found {
		t.Fatal(found, err)
	}
	got, _ := e.store.Get(ctx, run.ID)
	if got.Status != Sleeping {
		t.Fatalf("status = %s", got.Status)
	}
	var wake *time.Time
	if err := e.pool.QueryRow(ctx, `SELECT wake_at FROM runs WHERE id=$1`, run.ID).Scan(&wake); err != nil || wake == nil || !wake.Equal(t0.Add(600*time.Second)) {
		t.Fatalf("wake_at = %v %v", wake, err)
	}
	if found, _ := poolFor(e, m, nil, "w-2", func() time.Time { return t0.Add(599 * time.Second) }).ClaimAndWork(ctx, ctx); found {
		t.Fatal("woke early")
	}
	if found, err := poolFor(e, m, nil, "w-2", func() time.Time { return t0.Add(601 * time.Second) }).ClaimAndWork(ctx, ctx); err != nil || !found {
		t.Fatal(found, err)
	}
	got, _ = e.store.Get(ctx, run.ID)
	if got.Status != Succeeded {
		t.Fatalf("status = %s", got.Status)
	}
	if err := e.pool.QueryRow(ctx, `SELECT wake_at FROM runs WHERE id=$1`, run.ID).Scan(&wake); err != nil || wake != nil {
		t.Errorf("wake_at should be cleared: %v %v", wake, err)
	}
	// A sleeping or waiting run is cancelled on the spot.
	r2 := e.create(t, "two", Request{Input: Input{Text: "go"}}, t0)
	_, _ = poolFor(e, m, nil, "w-1", func() time.Time { return t0 }).ClaimAndWork(ctx, ctx)
	c, err := e.store.RequestCancel(ctx, r2.ID, e.key.ID, t0.Add(time.Second))
	if err != nil || c.Status != Cancelled {
		t.Errorf("cancel while sleeping: %+v %v", c, err)
	}
}
