//go:build integration

package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fixture inserts a run in a given state directly, with step rows to match, so every status can be read back.
type fixture struct {
	id uuid.UUID
}

func (e *env) fixture(t *testing.T, goal string, status Status, createdAt time.Time, finishedAt *time.Time, reason string, steps string) fixture {
	t.Helper()
	ctx := context.Background()
	id, _ := uuid.NewV7()
	req, _ := json.Marshal(Request{Input: Input{Text: goal}})
	limits, _ := json.Marshal(Limits{MaxSteps: 50, MaxCost: 1_000_000, Deadline: time.Hour})
	if _, err := e.pool.Exec(ctx, `INSERT INTO runs (id, api_key_id, status, request, limits, failure_reason, deadline_at, created_at, finished_at, step_count, cost_usd)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11)`,
		id, e.key.ID, string(status), req, limits, reason, createdAt.Add(time.Hour), createdAt, finishedAt, len(steps)/2, 0.1234); err != nil {
		t.Fatal(err)
	}
	// steps is a string of two-letter codes: m or t for the kind, then d(one), f(ailed) or o(pen).
	for i := 0; i+1 < len(steps); i += 2 {
		no := i/2 + 1
		typ := "model_call"
		if steps[i] == 't' {
			typ = "tool_call"
		}
		if steps[i] == 'h' {
			typ = "wait_human"
		}
		insert := func(phase string) {
			if _, err := e.pool.Exec(ctx, `INSERT INTO run_steps (run_id, step_no, type, phase, payload, lease_epoch, worker_id, created_at) VALUES ($1,$2,$3,$4,'{}',1,'w-1',$5)`,
				id, no, typ, phase, createdAt.Add(time.Duration(no)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		insert("started")
		switch steps[i+1] {
		case 'd':
			insert("finished")
		case 'f':
			insert("failed")
		}
	}
	return fixture{id: id}
}

func TestReaderSummaryCountsEveryStatus(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	ago := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	e.fixture(t, "queued", Queued, now.Add(-time.Minute), nil, "", "")
	e.fixture(t, "running", Running, now.Add(-time.Minute), nil, "", "mdto")
	e.fixture(t, "tooling", WaitingTool, now.Add(-time.Minute), nil, "", "mdto")
	e.fixture(t, "sleeping", Sleeping, now.Add(-time.Minute), nil, "", "md")
	waiting := e.fixture(t, "Send the renewal email", WaitingHuman, now.Add(-9*time.Minute), nil, "", "mdho")
	e.fixture(t, "ok recent", Succeeded, now.Add(-2*time.Hour), ago(time.Hour), "", "mdmd")
	e.fixture(t, "ok old", Succeeded, now.Add(-30*time.Hour), ago(29*time.Hour), "", "mdmd")
	e.fixture(t, "failed recent", Failed, now.Add(-2*time.Hour), ago(time.Hour), ReasonMaxSteps, "md")
	e.fixture(t, "cancelled recent", Cancelled, now.Add(-2*time.Hour), ago(time.Hour), ReasonCancelled, "md")
	if _, err := e.pool.Exec(ctx, `UPDATE run_steps SET payload='{"tool":"send_email","reason":"customer email"}' WHERE run_id=$1 AND type='wait_human' AND phase='started'`, waiting.id); err != nil {
		t.Fatal(err)
	}

	rd := NewReader(e.pool)
	s, err := rd.Summary(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := Counts{Running: 3, Sleeping: 1, NeedsYou: 1, Succeeded: 1, Failed: 1, Cancelled: 1}
	if s.Counts != want {
		t.Errorf("counts (24h) = %+v, want %+v", s.Counts, want)
	}
	if len(s.Waiting) != 1 || s.Waiting[0].ID != waiting.id || s.Waiting[0].Goal != "Send the renewal email" || s.Waiting[0].Tool != "send_email" || s.Waiting[0].Key != "runs-test" {
		t.Errorf("waiting = %+v", s.Waiting)
	}
	if d := now.Add(-9 * time.Minute).Sub(s.Waiting[0].WaitingSince); d < -2*time.Minute || d > 2*time.Minute {
		t.Errorf("waiting since = %v", s.Waiting[0].WaitingSince)
	}
	week, _ := rd.Summary(ctx, now.Add(-168*time.Hour))
	if week.Counts.Succeeded != 2 || week.Counts.Running != 3 {
		t.Errorf("counts (7d) = %+v: the finished counts widen with the range and the live ones do not", week.Counts)
	}
	hour, _ := rd.Summary(ctx, now.Add(-30*time.Minute))
	if hour.Counts.Succeeded != 0 || hour.Counts.Failed != 0 {
		t.Errorf("counts (30m) = %+v", hour.Counts)
	}
}

func TestReaderActivityBuckets(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	e.fixture(t, "a", Succeeded, now.Add(-30*time.Minute), &now, "", "")
	e.fixture(t, "b", Succeeded, now.Add(-40*time.Minute), &now, "", "")
	e.fixture(t, "c", Failed, now.Add(-35*time.Minute), &now, ReasonDeadline, "")
	e.fixture(t, "d", Running, now.Add(-5*time.Minute), nil, "", "")
	e.fixture(t, "e", Cancelled, now.Add(-3*time.Hour), &now, ReasonCancelled, "")
	e.fixture(t, "old", Succeeded, now.Add(-30*time.Hour), &now, "", "")

	step, b, err := NewReader(e.pool).Activity(context.Background(), 24, now)
	if err != nil {
		t.Fatal(err)
	}
	if step != time.Hour || len(b) != 24 {
		t.Fatalf("bucket %v, %d buckets", step, len(b))
	}
	var tot Bucket
	for i, x := range b {
		if i > 0 && !x.Start.After(b[i-1].Start) {
			t.Fatal("buckets must be in time order")
		}
		tot.Succeeded += x.Succeeded
		tot.Failed += x.Failed
		tot.Cancelled += x.Cancelled
		tot.InProgress += x.InProgress
	}
	if tot != (Bucket{Succeeded: 2, Failed: 1, Cancelled: 1, InProgress: 1}) {
		t.Errorf("totals = %+v: the run from 30 hours ago is outside the range", tot)
	}
	if !b[len(b)-1].Start.Add(time.Hour).After(now.Add(-time.Second)) {
		t.Errorf("the last bucket runs up to now: %v", b[len(b)-1].Start)
	}
	if _, w, _ := NewReader(e.pool).Activity(context.Background(), 168, now); len(w) != 7 {
		t.Errorf("a week is 7 daily buckets, got %d", len(w))
	}
	if _, h, _ := NewReader(e.pool).Activity(context.Background(), 1, now); len(h) != 12 {
		t.Errorf("an hour is 12 five-minute buckets, got %d", len(h))
	}
}

func TestReaderListFiltersStripsAndPaging(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rd := NewReader(e.pool)
	now := time.Now().UTC()
	at := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	fin := func(m int) *time.Time { x := at(m); return &x }

	running := e.fixture(t, "Research competitor pricing", Running, at(1), nil, "", "mdtdmo")
	person := e.fixture(t, "Needs approval", WaitingHuman, at(2), nil, "", "mdho")
	okRun := e.fixture(t, "Reconcile invoices", Succeeded, at(10), fin(8), "", "mdtdmd")
	bad := e.fixture(t, "Backfill records", Failed, at(20), fin(15), ReasonMaxSteps, "mdtdmd")
	killed := e.fixture(t, "Cancelled mid-step", Cancelled, at(30), fin(29), ReasonCancelled, "mdto")
	old := e.fixture(t, "Ancient", Succeeded, at(60*30), fin(60*29), "", "md")
	var long string
	for i := 0; i < 40; i++ {
		long += "md"
	}
	longRun := e.fixture(t, "A very long run", Running, at(3), nil, "", long[:len(long)-1]+"o")

	list := func(f ListFilter) []Item {
		t.Helper()
		items, err := rd.List(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	ids := func(items []Item) []uuid.UUID {
		var out []uuid.UUID
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}
	same := func(got []Item, want ...fixture) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i].ID != want[i].id {
				return false
			}
		}
		return true
	}

	active := list(ListFilter{State: "active", Since: at(60 * 24)})
	if !same(active, running, longRun, person) && !same(active, running, person, longRun) {
		t.Fatalf("active = %v", ids(active))
	}
	for _, it := range active {
		switch it.ID {
		case running.id:
			if it.Goal != "Research competitor pricing" || it.Key != "runs-test" || it.Model != "default" || it.Status != Running || render(it.Strip) != "m t m* " && render(it.Strip) != "m t m*" {
				t.Errorf("running = %+v strip %q", it, render(it.Strip))
			}
		case person.id:
			if got := render(it.Strip); got != "m t?" {
				t.Errorf("a run waiting for a person: strip %q", got)
			}
		case longRun.id:
			if len(it.Strip.Steps) != StripLen || it.Strip.More != 40-StripLen || it.Strip.Steps[StripLen-1].State != "current" {
				t.Errorf("a long run shows its latest %d steps: %d shown, %d hidden", StripLen, len(it.Strip.Steps), it.Strip.More)
			}
		}
	}

	fin24 := list(ListFilter{State: "finished", Since: at(60 * 24)})
	if !same(fin24, okRun, bad, killed) {
		t.Fatalf("finished (24h) = %v", ids(fin24))
	}
	if got := render(fin24[1].Strip); got != "m t m t m! m!" && got != "m t m t m m!" {
		// the last step of a failed run is marked failed
		if fin24[1].Strip.Steps[len(fin24[1].Strip.Steps)-1].State != "failed" {
			t.Errorf("failed run strip = %q", got)
		}
	}
	if st := fin24[2].Strip.Steps; st[len(st)-1].State != "failed" || st[len(st)-1].Kind != "tool" {
		t.Errorf("the cancelled run's open tool step is shown failed: %+v", st)
	}
	if fin24[1].FailureReason != ReasonMaxSteps || fin24[0].FinishedAt == nil {
		t.Errorf("reason / finished: %+v", fin24[1])
	}
	if week := list(ListFilter{State: "finished", Since: at(60 * 24 * 7)}); len(week) != 4 {
		t.Errorf("finished (7d) = %d runs", len(week))
	}
	if only := list(ListFilter{Status: Failed, Since: at(60 * 24 * 7)}); !same(only, bad) {
		t.Errorf("status=failed = %v", ids(only))
	}
	_ = old

	// Keyset paging walks newest to oldest with no repeats and no gaps.
	var seen []uuid.UUID
	var cur *Cursor
	for page := 0; page < 10; page++ {
		items := list(ListFilter{Limit: 3, Since: at(60 * 24 * 7), Before: cur})
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			seen = append(seen, it.ID)
		}
		last := items[len(items)-1]
		cur = &Cursor{At: last.CreatedAt, ID: last.ID}
	}
	all := list(ListFilter{Limit: 100, Since: at(60 * 24 * 7)})
	if len(seen) != 7 || fmt.Sprint(seen) != fmt.Sprint(ids(all)) {
		t.Errorf("paging saw %d runs, want 7, in order", len(seen))
	}
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.After(all[i-1].CreatedAt) {
			t.Error("newest first")
		}
	}

	got, err := rd.Get(ctx, okRun.id)
	if err != nil || got.Goal != "Reconcile invoices" || got.Cost != 123_400 || got.StepCount != 3 {
		t.Errorf("get = %+v %v", got, err)
	}
	if _, err := rd.Get(ctx, uuid.New()); err != ErrNotFound {
		t.Errorf("unknown run: %v", err)
	}
}

func TestReaderSearchAndKeyFilter(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rd := NewReader(e.pool)
	now := time.Now().UTC()
	a := e.fixture(t, "Research competitor PRICING for 40 vendors", Running, now.Add(-time.Minute), nil, "", "")
	e.fixture(t, "Draft the weekly digest", Running, now.Add(-2*time.Minute), nil, "", "")
	e.fixture(t, "Cover 100% of the cases_with_underscores", Running, now.Add(-3*time.Minute), nil, "", "")
	other := e.fixture(t, "Pricing for someone else", Running, now.Add(-4*time.Minute), nil, "", "")
	k2 := newEnvKey(t, e, "other-key")
	if _, err := e.pool.Exec(ctx, `UPDATE runs SET api_key_id=$2 WHERE id=$1`, other.id, k2); err != nil {
		t.Fatal(err)
	}
	find := func(f ListFilter) int {
		f.State = "active"
		items, err := rd.List(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	if n := find(ListFilter{Q: "pricing"}); n != 2 {
		t.Errorf("a search ignores case: %d", n)
	}
	if n := find(ListFilter{Q: "PRICING for 40"}); n != 1 {
		t.Errorf("a phrase: %d", n)
	}
	if n := find(ListFilter{Q: "%"}); n != 1 {
		t.Errorf("a percent sign is a character, not a wildcard: %d", n)
	}
	if n := find(ListFilter{Q: "_"}); n != 1 {
		t.Errorf("an underscore is a character, not a wildcard: %d", n)
	}
	if n := find(ListFilter{Q: `\`}); n != 0 {
		t.Errorf("a backslash is a character: %d", n)
	}
	if n := find(ListFilter{Q: "nothing like this"}); n != 0 {
		t.Errorf("no match: %d", n)
	}
	if n := find(ListFilter{KeyID: &e.key.ID}); n != 3 {
		t.Errorf("one key's runs: %d", n)
	}
	if n := find(ListFilter{KeyID: &k2, Q: "pricing"}); n != 1 {
		t.Errorf("key and search together: %d", n)
	}
	_ = a
}

func newEnvKey(t *testing.T, e *env, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO api_keys (id, name, key_prefix, key_hash) VALUES (gen_random_uuid(), $1, 'spw_xxxx', decode(md5($1),'hex')) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestReaderGraphAfterARealCrashAndRecovery(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rd := NewReader(e.pool)
	run := e.create(t, "", Request{Input: Input{Text: "Research pricing"}, Tools: nil}, t0)

	// w-a claims, does a model call (with a usage row that shows a fallback), starts a tool call and is killed.
	a, _ := e.store.Claim(ctx, "w-a", 30*time.Second, t0)
	la := e.store.Log(Lease{RunID: a.ID, Owner: "w-a", Epoch: a.LeaseEpoch}, func() time.Time { return t0.Add(time.Second) })
	usageID := uuid.New()
	if _, err := e.pool.Exec(ctx, `INSERT INTO usage (id, api_key_id, run_id, policy, provider, model, latency_ms, cache_status, outcome, attempts, created_at)
		VALUES ($1,$2,$3,'default','anthropic','sonnet',2900,'miss','ok',$4,$5)`, usageID, e.key.ID, run.ID,
		`[{"provider":"openai","model":"mini","kind":"primary","status":503,"error_kind":"server","latency_ms":340},{"provider":"anthropic","model":"sonnet","kind":"fallback","latency_ms":2560}]`, t0); err != nil {
		t.Fatal(err)
	}
	must := func(_ Step, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(la.Append(ctx, NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseStarted, Payload: ModelStarted{Policy: "default"}}))
	must(la.Append(ctx, NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseFinished, Cost: 7100, Payload: ModelFinished{Message: calls("c1"), Provider: "anthropic", Model: "sonnet", UsageID: usageID.String()}}))
	must(la.Append(ctx, NewStep{StepNo: ptr(2), Type: ToolCall, Phase: PhaseStarted, Key: IdempotencyKey(run.ID, 2), Payload: ToolStarted{Tool: "fetch_page", Arguments: json.RawMessage(`{"url":"https://x"}`), ToolCallID: "c1"}}))

	// w-b takes over after the lease expires and re-issues step 2, which finishes.
	b, _ := e.store.Claim(ctx, "w-b", 30*time.Second, t0.Add(40*time.Second))
	lb := e.store.Log(Lease{RunID: b.ID, Owner: "w-b", Epoch: b.LeaseEpoch}, func() time.Time { return t0.Add(41 * time.Second) })
	must(lb.Append(ctx, NewStep{StepNo: ptr(2), Type: ToolCall, Phase: PhaseReissued, Key: IdempotencyKey(run.ID, 2), Payload: Reissued{PreviousWorker: "w-a", PreviousEpoch: 1}}))
	must(lb.Append(ctx, NewStep{StepNo: ptr(2), Type: ToolCall, Phase: PhaseFinished, Key: IdempotencyKey(run.ID, 2), Payload: ToolFinished{Result: "page text"}}))
	must(lb.Append(ctx, NewStep{StepNo: ptr(3), Type: ModelCall, Phase: PhaseStarted, Payload: ModelStarted{}}))

	g, err := rd.Graph(ctx, run.ID, t0.Add(45*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range g.Nodes {
		got = append(got, fmt.Sprintf("%d/%s/e%d/%s/%v", n.StepNo, n.Worker, n.Epoch, n.State, n.Reissued))
	}
	want := "[1/w-a/e1/finished/false 2/w-a/e1/stopped/false 2/w-b/e2/finished/true 3/w-b/e2/running/false]"
	if fmt.Sprint(got) != want {
		t.Fatalf("nodes = %v, want %s", got, want)
	}
	if len(g.Recoveries) != 1 || g.Recoveries[0].FromWorker != "w-a" || g.Recoveries[0].ToWorker != "w-b" || g.Recoveries[0].AfterStep != 2 || g.Recoveries[0].Epoch != 2 {
		t.Errorf("recoveries = %+v", g.Recoveries)
	}
	first := g.Nodes[0]
	if len(first.Attempts) != 2 || first.Attempts[0].Status != 503 || first.Cache != "miss" || first.CostUSD != "0.007100" {
		t.Errorf("the model call carries its fallback from the usage row: %+v", first)
	}
	if g.Nodes[1].IdempotencyKey != g.Nodes[2].IdempotencyKey || g.Nodes[1].IdempotencyKey == "" || g.Nodes[2].PreviousWorker != "w-a" {
		t.Errorf("the re-issue shares the key: %+v / %+v", g.Nodes[1], g.Nodes[2])
	}
	if g.Run.Goal != "Research pricing" || g.Run.Key != "runs-test" || g.Run.MaxSteps != 50 || g.Run.LeaseEpoch != 2 || g.Run.LeaseOwner == nil || *g.Run.LeaseOwner != "w-b" || g.LastEvent == 0 {
		t.Errorf("run = %+v last=%d", g.Run, g.LastEvent)
	}
	if g.Run.StepCount != 3 {
		t.Errorf("step count = %d", g.Run.StepCount)
	}
	if _, err := rd.Graph(ctx, uuid.New(), t0); err != ErrNotFound {
		t.Errorf("unknown run: %v", err)
	}
}
