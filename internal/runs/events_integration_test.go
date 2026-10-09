//go:build integration

package runs

import (
	"context"
	"testing"
	"time"
)

func TestBrokerWakesWatchersOnInsertAndOnlyForTheirRun(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := NewBroker(e.pool, nil)
	go b.Run(ctx)

	mine := e.create(t, "", Request{Input: Input{Text: "mine"}}, t0)
	other := e.create(t, "", Request{Input: Input{Text: "other"}}, t0.Add(time.Second))
	wakeMine, stopMine := b.Subscribe(mine.ID)
	defer stopMine()
	wakeOther, stopOther := b.Subscribe(other.ID)
	defer stopOther()
	select {
	case <-b.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never started")
	}
	for _, ch := range []<-chan struct{}{wakeMine, wakeOther} { // the signal sent when listening began, if it came after subscribing
		select {
		case <-ch:
		default:
		}
	}

	a, _ := e.store.Claim(ctx, "w-a", 30*time.Second, t0) // claiming `mine` writes a run_status row, which notifies
	if a == nil || a.ID != mine.ID {
		t.Fatalf("claimed %+v", a)
	}
	select {
	case <-wakeMine:
	case <-time.After(3 * time.Second):
		t.Fatal("a step insert did not wake the watcher of that run")
	}
	select {
	case <-wakeOther:
		t.Error("a watcher of another run was woken")
	case <-time.After(200 * time.Millisecond):
	}

	log := e.store.Log(Lease{RunID: a.ID, Owner: "w-a", Epoch: a.LeaseEpoch}, func() time.Time { return t0 })
	if _, err := log.Append(ctx, NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseStarted, Payload: ModelStarted{}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wakeMine:
	case <-time.After(3 * time.Second):
		t.Fatal("an appended step did not wake the watcher")
	}

	stopMine()
	if _, err := log.Append(ctx, NewStep{StepNo: ptr(1), Type: ModelCall, Phase: PhaseFailed, Payload: ErrorPayload{Error: "x"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wakeMine:
		t.Error("an unsubscribed watcher was woken")
	case <-time.After(200 * time.Millisecond):
	}
}
