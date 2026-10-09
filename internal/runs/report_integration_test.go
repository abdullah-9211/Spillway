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
