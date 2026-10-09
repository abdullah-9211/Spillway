package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/runs"
)

const sampleRunID = "00000000-0000-7000-8000-000000000000"

var runsNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

type fakeRunsAdmin struct {
	items   []runs.Item
	summary runs.Summary
	err     error
	last    runs.ListFilter
	hours   int
	since   time.Time
}

func (f *fakeRunsAdmin) Summary(_ context.Context, since time.Time) (runs.Summary, error) {
	f.since = since
	return f.summary, f.err
}

func (f *fakeRunsAdmin) Activity(_ context.Context, hours int, now time.Time) (time.Duration, []runs.Bucket, error) {
	f.hours = hours
	step := runs.BucketFor(hours)
	return step, []runs.Bucket{{Start: now.Add(-step), Succeeded: 3, Failed: 1, InProgress: 2}}, f.err
}

func (f *fakeRunsAdmin) List(_ context.Context, fl runs.ListFilter) ([]runs.Item, error) {
	f.last = fl
	if f.err != nil {
		return nil, f.err
	}
	var out []runs.Item
	for _, it := range f.items {
		if fl.Before != nil && !(it.CreatedAt.Before(fl.Before.At)) {
			continue
		}
		out = append(out, it)
		if len(out) == fl.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeRunsAdmin) Get(_ context.Context, id uuid.UUID) (runs.Item, error) {
	for _, it := range f.items {
		if it.ID == id {
			return it, nil
		}
	}
	return runs.Item{}, runs.ErrNotFound
}

func sampleRuns() *fakeRunsAdmin {
	var items []runs.Item
	for i := 0; i < 5; i++ {
		fin := runsNow.Add(-time.Duration(i) * time.Minute)
		it := runs.Item{ID: uuid.MustParse(fmt.Sprintf("00000000-0000-7000-8000-00000000000%d", i)), Status: runs.Succeeded, Goal: fmt.Sprintf("goal %d", i), Key: "support-agent", Model: "default", StepCount: 3, Cost: 14_700,
			CreatedAt: runsNow.Add(-time.Duration(i+10) * time.Minute), FinishedAt: &fin,
			Strip: runs.Strip{Steps: []runs.StripNode{{Kind: "model", State: "done"}, {Kind: "tool", State: "done"}}}}
		items = append(items, it)
	}
	items[1].Status, items[1].FailureReason = runs.Failed, "max_steps"
	items[2].Status, items[2].FinishedAt, items[2].Strip = runs.Running, nil, runs.Strip{}
	return &fakeRunsAdmin{items: items, summary: runs.Summary{
		Counts:  runs.Counts{Running: 6, NeedsYou: 2, Succeeded: 104, Failed: 11},
		Waiting: []runs.Waiting{{ID: uuid.New(), Goal: "Send the Q3 renewal email", Tool: "send_email", WaitingSince: runsNow.Add(-2 * time.Minute), Key: "support-agent"}}}}
}

func newRunsAdminRig(t *testing.T) (*adminRig, *fakeRunsAdmin) {
	t.Helper()
	r := newAdminRig(t)
	f := sampleRuns()
	r.admin.d.Runs = f
	r.admin.d.Now = func() time.Time { return runsNow }
	r.admin.registerRuns()
	return r, f
}

func TestRunsSummaryAndActivity(t *testing.T) {
	r, f := newRunsAdminRig(t)
	viewer := r.token(t, "viewer", "viewer-password")
	code, body := r.json(t, "GET", "/admin/runs/summary", viewer, "")
	c := body["counts"].(map[string]any)
	if code != 200 || c["running"].(float64) != 6 || c["needs_you"].(float64) != 2 || c["succeeded"].(float64) != 104 || body["hours"].(float64) != 24 {
		t.Fatalf("%d %v", code, body)
	}
	w := body["waiting"].([]any)[0].(map[string]any)
	if w["tool"] != "send_email" || w["goal"] != "Send the Q3 renewal email" || w["key"] != "support-agent" {
		t.Errorf("waiting = %v", w)
	}
	if !f.since.Equal(runsNow.Add(-24 * time.Hour)) {
		t.Errorf("24h range starts %v", f.since)
	}
	r.json(t, "GET", "/admin/runs/summary?hours=168", viewer, "")
	if !f.since.Equal(runsNow.Add(-168 * time.Hour)) {
		t.Errorf("7d range starts %v", f.since)
	}
	if code, _ := r.json(t, "GET", "/admin/runs/summary?hours=5", viewer, ""); code != 400 {
		t.Errorf("hours=5: %d", code)
	}
	if code, body := r.json(t, "GET", "/admin/runs/activity?hours=1", viewer, ""); code != 200 || body["bucket_seconds"].(float64) != 300 || f.hours != 1 || len(body["buckets"].([]any)) != 1 {
		t.Errorf("activity: %d %v", code, body)
	}
	if code, _ := r.json(t, "GET", "/admin/runs/activity?hours=x", viewer, ""); code != 400 {
		t.Errorf("activity hours=x: %d", code)
	}
}

func TestRunsListShapeFiltersAndPaging(t *testing.T) {
	r, f := newRunsAdminRig(t)
	tok := r.token(t, "viewer", "viewer-password")
	code, body := r.json(t, "GET", "/admin/runs?state=finished&hours=168&limit=2", tok, "")
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if f.last.State != "finished" || f.last.Limit != 3 || !f.last.Since.Equal(runsNow.Add(-168*time.Hour)) {
		t.Errorf("the filter passed down = %+v (one extra row is fetched to know if there is a next page)", f.last)
	}
	rs := body["runs"].([]any)
	if len(rs) != 2 || body["next_cursor"] == nil {
		t.Fatalf("page one: %d runs, cursor %v", len(rs), body["next_cursor"])
	}
	first := rs[0].(map[string]any)
	if first["goal"] != "goal 0" || first["key"] != "support-agent" || first["cost_usd"] != "0.014700" || first["status"] != "succeeded" || first["duration_ms"].(float64) != 10*60*1000-0 && first["duration_ms"].(float64) != 600000 || first["failure_reason"] != nil {
		t.Errorf("first = %v", first)
	}
	if st := first["strip"].(map[string]any); len(st["steps"].([]any)) != 2 || st["more"].(float64) != 0 {
		t.Errorf("strip = %v", st)
	}
	// Page two continues after the cursor, and the last page has no cursor.
	cursor := body["next_cursor"].(string)
	_, body = r.json(t, "GET", "/admin/runs?limit=2&cursor="+cursor, tok, "")
	rs2 := body["runs"].([]any)
	if len(rs2) != 2 || rs2[0].(map[string]any)["goal"] != "goal 2" || body["next_cursor"] == nil {
		t.Fatalf("page two: %v", body)
	}
	_, body = r.json(t, "GET", "/admin/runs?limit=2&cursor="+body["next_cursor"].(string), tok, "")
	if len(body["runs"].([]any)) != 1 || body["next_cursor"] != nil {
		t.Errorf("last page: %v", body)
	}
	// A running run has no finish time, and a failed one has its reason.
	_, body = r.json(t, "GET", "/admin/runs?limit=5", tok, "")
	all := body["runs"].([]any)
	if all[2].(map[string]any)["finished_at"] != nil || all[2].(map[string]any)["strip"].(map[string]any)["steps"] == nil {
		t.Errorf("running run = %v", all[2])
	}
	if all[1].(map[string]any)["failure_reason"] != "max_steps" {
		t.Errorf("failed run = %v", all[1])
	}
	for _, bad := range []string{"state=nope", "status=nope", "limit=0", "limit=101", "limit=x", "cursor=!!", "hours=2"} {
		if code, _ := r.json(t, "GET", "/admin/runs?"+bad, tok, ""); code != 400 {
			t.Errorf("?%s: %d", bad, code)
		}
	}
	if code, _ := r.json(t, "GET", "/admin/runs?status=failed&state=active", tok, ""); code != 200 || f.last.Status != runs.Failed || f.last.State != "active" {
		t.Errorf("status filter passed down: %+v", f.last)
	}
}

func TestRunGet(t *testing.T) {
	r, f := newRunsAdminRig(t)
	tok := r.token(t, "viewer", "viewer-password")
	id := f.items[1].ID.String()
	if code, body := r.json(t, "GET", "/admin/runs/"+id, tok, ""); code != 200 || body["id"] != id || body["failure_reason"] != "max_steps" {
		t.Errorf("%d %v", code, body)
	}
	for _, p := range []string{uuid.NewString(), "not-a-uuid"} {
		if code, body := r.json(t, "GET", "/admin/runs/"+p, tok, ""); code != 404 || body["error"].(map[string]any)["code"] != "not_found" {
			t.Errorf("%s: %d %v", p, code, body)
		}
	}
	// "summary" and "activity" are routes of their own, not run ids.
	if code, _ := r.json(t, "GET", "/admin/runs/summary", tok, ""); code != 200 {
		t.Errorf("summary: %d", code)
	}
}

func TestRunsEndpointsNeedASessionAndHideErrors(t *testing.T) {
	r, f := newRunsAdminRig(t)
	for _, p := range []string{"/admin/runs", "/admin/runs/summary", "/admin/runs/activity", "/admin/runs/" + uuid.NewString()} {
		if code, _ := r.json(t, "GET", p, "", ""); code != 401 {
			t.Errorf("%s without a token: %d", p, code)
		}
	}
	f.err = fmt.Errorf("pq: password authentication failed for user spillway")
	tok := r.token(t, "admin", "admin-password")
	for _, p := range []string{"/admin/runs", "/admin/runs/summary", "/admin/runs/activity"} {
		resp := r.do(t, "GET", p, tok, "")
		body := decode(t, resp)
		if resp.StatusCode != 500 || strings.Contains(fmt.Sprint(body), "password") {
			t.Errorf("%s: %d %v (internal errors must not leak)", p, resp.StatusCode, body)
		}
	}
}
