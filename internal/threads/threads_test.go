/*
Copyright 2026 TechDufus.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package threads

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	ns     = "dev"
	thread = "coder-k3x9fa"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func newLog(t *testing.T) *Log {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := NewLog(st)
	l.Now = (&clock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}).now
	return l
}

func enqueue(t *testing.T, l *Log, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if _, err := l.Enqueue(ns, thread, text, "token:dev"); err != nil {
			t.Fatal(err)
		}
	}
}

func next(t *testing.T, l *Log, session string) *Turn {
	t.Helper()
	got, err := l.Next(context.Background(), ns, thread, session, 0)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func turns(t *testing.T, l *Log) []Turn {
	t.Helper()
	ts, err := l.Turns(ns, thread)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func states(ts []Turn) []v1alpha1.TurnState {
	out := make([]v1alpha1.TurnState, len(ts))
	for i, t := range ts {
		out[i] = t.State
	}
	return out
}

func wantStates(t *testing.T, l *Log, want ...v1alpha1.TurnState) {
	t.Helper()
	got := states(turns(t, l))
	if len(got) != len(want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("states = %v, want %v", got, want)
		}
	}
}

const (
	queued    = v1alpha1.TurnStateQueued
	running   = v1alpha1.TurnStateRunning
	done      = v1alpha1.TurnStateDone
	failed    = v1alpha1.TurnStateFailed
	cancelled = v1alpha1.TurnStateCancelled
)

func TestEnqueue(t *testing.T) {
	l := newLog(t)
	if ts := turns(t, l); ts != nil {
		t.Fatalf("turns of an empty thread = %v", ts)
	}
	for _, text := range []string{"", strings.Repeat("x", MaxTextBytes+1)} {
		if _, err := l.Enqueue(ns, thread, text, "u"); !errors.Is(err, ErrInvalidText) {
			t.Errorf("Enqueue(%d bytes) = %v, want ErrInvalidText", len(text), err)
		}
	}
	a, err := l.Enqueue(ns, thread, "first", "token:dev")
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.Enqueue(ns, thread, strings.Repeat("x", MaxTextBytes), "token:dev")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != 1 || b.ID != 2 || a.State != queued || a.By != "token:dev" || a.QueuedAt.IsZero() {
		t.Errorf("enqueued %+v and %+v", a, b)
	}
	// Another thread has its own log.
	if got, err := l.Turns(ns, "other"); err != nil || got != nil {
		t.Errorf("other thread turns = %v, %v", got, err)
	}
}

func TestEnqueueThreadFull(t *testing.T) {
	l := newLog(t)
	for i := range MaxTurns {
		if _, err := l.Enqueue(ns, thread, "m", "u"); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}
	if _, err := l.Enqueue(ns, thread, "one too many", "u"); !errors.Is(err, ErrThreadFull) {
		t.Fatalf("turn %d = %v, want ErrThreadFull", MaxTurns+1, err)
	}
	// Finished turns still count: the thread stays full.
	if got := next(t, l, "s1"); got == nil {
		t.Fatal("no turn to take")
	}
	if err := l.Complete(ns, thread, "s1", 1, Result{Outcome: OutcomeCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Enqueue(ns, thread, "still full", "u"); !errors.Is(err, ErrThreadFull) {
		t.Fatalf("after completing a turn = %v, want ErrThreadFull", err)
	}
	if n := len(turns(t, l)); n != MaxTurns {
		t.Fatalf("%d turns, want %d", n, MaxTurns)
	}
}

func TestNextOrderAndRedelivery(t *testing.T) {
	l := newLog(t)
	if got := next(t, l, "s1"); got != nil {
		t.Fatalf("Next on an empty thread = %+v", got)
	}
	enqueue(t, l, "one", "two", "three")

	first := next(t, l, "s1")
	if first == nil || first.ID != 1 || first.State != running || first.Session != "s1" || first.StartedAt == nil {
		t.Fatalf("first = %+v", first)
	}
	// The turn was handed out but never completed: the same session gets it again, unchanged.
	again := next(t, l, "s1")
	if again == nil || again.ID != 1 || !again.StartedAt.Equal(*first.StartedAt) {
		t.Fatalf("redelivery = %+v, want turn 1 with the same StartedAt", again)
	}
	wantStates(t, l, running, queued, queued)

	if err := l.Complete(ns, thread, "s1", 1, Result{Outcome: OutcomeCompleted, Reply: "ok"}); err != nil {
		t.Fatal(err)
	}
	if got := next(t, l, "s1"); got == nil || got.ID != 2 {
		t.Fatalf("after completing turn 1 = %+v, want turn 2", got)
	}
	// A new sitting takes the oldest queued turn; it does not steal the running one.
	if got := next(t, l, "s2"); got == nil || got.ID != 3 || got.Session != "s2" {
		t.Fatalf("second session = %+v, want turn 3", got)
	}
	if got := next(t, l, "s3"); got != nil {
		t.Fatalf("nothing left, got %+v", got)
	}
	wantStates(t, l, done, running, running)
}

func TestNextLongPollWakesOnSignal(t *testing.T) {
	l := newLog(t)
	type result struct {
		turn *Turn
		err  error
	}
	res := make(chan result, 1)
	started := time.Now()
	go func() {
		turn, err := l.Next(context.Background(), ns, thread, "s1", 30*time.Second)
		res <- result{turn, err}
	}()
	// Give the waiter time to subscribe, then enqueue and signal.
	time.Sleep(50 * time.Millisecond)
	enqueue(t, l, "wake up")
	l.Signal(ns, thread)

	select {
	case r := <-res:
		if r.err != nil || r.turn == nil || r.turn.ID != 1 {
			t.Fatalf("Next = %+v, %v", r.turn, r.err)
		}
		if time.Since(started) > 10*time.Second {
			t.Fatalf("Next took %s: the signal did not wake it", time.Since(started))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Next did not return after Signal")
	}
}

func TestNextSignalOfAnotherThreadDoesNotWake(t *testing.T) {
	l := newLog(t)
	res := make(chan *Turn, 1)
	go func() {
		turn, _ := l.Next(context.Background(), ns, thread, "s1", 300*time.Millisecond)
		res <- turn
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := l.Enqueue(ns, "other", "m", "u"); err != nil {
		t.Fatal(err)
	}
	l.Signal(ns, "other")
	if turn := <-res; turn != nil {
		t.Fatalf("Next returned %+v for another thread's turn", turn)
	}
}

func TestNextTimeoutAndCancel(t *testing.T) {
	l := newLog(t)
	if got, err := l.Next(context.Background(), ns, thread, "s1", 30*time.Millisecond); got != nil || err != nil {
		t.Fatalf("timed out Next = %+v, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if got, err := l.Next(ctx, ns, thread, "s1", 30*time.Second); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Next = %+v, %v", got, err)
	}
	// Waiters unsubscribe: nothing is left behind.
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.waiters) != 0 {
		t.Fatalf("waiters left: %v", l.waiters)
	}
}

func TestSignalEmitsEvent(t *testing.T) {
	l := newLog(t)
	l.Signal(ns, thread)
	select {
	case ev := <-l.Events():
		if ev.Object.GetNamespace() != ns || ev.Object.GetName() != thread {
			t.Fatalf("event for %s/%s", ev.Object.GetNamespace(), ev.Object.GetName())
		}
	default:
		t.Fatal("no event")
	}
	// A full buffer never blocks Signal.
	for range eventsBuffer + 10 {
		l.Signal(ns, thread)
	}
}

func TestCompleteStoresResult(t *testing.T) {
	l := newLog(t)
	enqueue(t, l, "do it")
	next(t, l, "s1")
	err := l.Complete(ns, thread, "s1", 1, Result{
		Outcome: OutcomeCompleted, Reply: "## Added Greet\nDetails follow.",
		Usage: Usage{InputTokens: 10, OutputTokens: 5, Cost: "0.0123"},
		Head:  "abc", FilesChanged: 2, Insertions: 30, Deletions: 4, Pushed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := turns(t, l)[0]
	if got.State != done || got.Title != "Added Greet" || got.Reply != "## Added Greet\nDetails follow." || got.Error != "" ||
		got.CompletedAt == nil || got.Usage == nil || got.Usage.Cost != "0.0123" || got.Usage.InputTokens != 10 ||
		got.Head != "abc" || got.FilesChanged != 2 || got.Insertions != 30 || got.Deletions != 4 || !got.Pushed {
		t.Fatalf("completed turn = %+v", got)
	}
	if !got.Finished() {
		t.Error("a done turn is finished")
	}

	enqueue(t, l, "again")
	next(t, l, "s1")
	if err := l.Complete(ns, thread, "s1", 2, Result{Outcome: "error", Error: "omp crashed", PushError: "denied"}); err != nil {
		t.Fatal(err)
	}
	if got := turns(t, l)[1]; got.State != failed || got.Error != "omp crashed" || got.PushError != "denied" {
		t.Fatalf("failed turn = %+v", got)
	}
}

func TestCompleteRefusals(t *testing.T) {
	l := newLog(t)
	enqueue(t, l, "one", "two")
	next(t, l, "s1")
	tests := []struct {
		name    string
		session string
		id      int
	}{
		{"another session", "s2", 1},
		{"queued turn", "s1", 2},
		{"unknown turn", "s1", 99},
	}
	for _, tt := range tests {
		if err := l.Complete(ns, thread, tt.session, tt.id, Result{Outcome: OutcomeCompleted}); !errors.Is(err, ErrNotRunning) {
			t.Errorf("%s: Complete = %v, want ErrNotRunning", tt.name, err)
		}
	}
	wantStates(t, l, running, queued)
	if err := l.Complete(ns, thread, "s1", 1, Result{Outcome: OutcomeCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := l.Complete(ns, thread, "s1", 1, Result{Outcome: OutcomeCompleted}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("second Complete = %v, want ErrNotRunning", err)
	}
}

func TestStopAndCompleteStoppedTurn(t *testing.T) {
	l := newLog(t)
	enqueue(t, l, "long job", "queued behind", "also queued")
	next(t, l, "s1")

	if stop, err := l.Control(ns, thread, "s1"); err != nil || stop {
		t.Fatalf("Control before Stop = %v, %v", stop, err)
	}
	running1, cancelledN, err := l.Stop(ns, thread, "token:dev")
	if err != nil || !running1 || cancelledN != 2 {
		t.Fatalf("Stop = %v, %d, %v; want true, 2, nil", running1, cancelledN, err)
	}
	wantStates(t, l, running, cancelled, cancelled)
	if got := turns(t, l); !got[0].StopRequested || got[0].StoppedBy != "token:dev" || got[1].Error != "cancelled by token:dev" || got[1].CompletedAt == nil {
		t.Fatalf("turns after Stop = %+v", got)
	}
	if stop, _ := l.Control(ns, thread, "s1"); !stop {
		t.Error("Control(s1) = false after Stop")
	}
	if stop, _ := l.Control(ns, thread, "s2"); stop {
		t.Error("Control(s2) = true for a session that runs nothing")
	}
	// Stop again: nothing more to cancel, the running turn is still reported.
	if r, c, err := l.Stop(ns, thread, "token:other"); err != nil || !r || c != 0 {
		t.Fatalf("second Stop = %v, %d, %v", r, c, err)
	}
	if got := turns(t, l)[0].StoppedBy; got != "token:dev" {
		t.Errorf("StoppedBy = %q, the first stopper must stay", got)
	}

	// The driver aborts omp and reports whatever happened; the turn fails as stopped.
	err = l.Complete(ns, thread, "s1", 1, Result{Outcome: OutcomeCompleted, Reply: "partial"})
	if err != nil {
		t.Fatal(err)
	}
	if got := turns(t, l)[0]; got.State != failed || got.Error != "stopped by token:dev" || got.Reply != "partial" {
		t.Fatalf("stopped turn = %+v", got)
	}
	if r, c, err := l.Stop(ns, thread, "x"); err != nil || r || c != 0 {
		t.Fatalf("Stop of an idle thread = %v, %d, %v", r, c, err)
	}
}

func TestRequeue(t *testing.T) {
	l := newLog(t)
	enqueue(t, l, "one", "two", "three")
	next(t, l, "s1") // turn 1 running for s1
	if n, err := l.Requeue(ns, thread, "s2"); err != nil || n != 0 {
		t.Fatalf("Requeue of another session = %d, %v", n, err)
	}
	n, err := l.Requeue(ns, thread, "s1")
	if err != nil || n != 1 {
		t.Fatalf("Requeue = %d, %v; want 1", n, err)
	}
	wantStates(t, l, queued, queued, queued)
	if got := turns(t, l)[0]; got.Session != "" || got.StartedAt != nil {
		t.Fatalf("requeued turn keeps %q, %v", got.Session, got.StartedAt)
	}
	// The next sitting starts with the requeued turn, in the original order.
	if got := next(t, l, "s2"); got == nil || got.ID != 1 || got.Session != "s2" {
		t.Fatalf("next sitting got %+v, want turn 1", got)
	}

	// A turn that was asked to stop is not redone.
	if _, _, err := l.Stop(ns, thread, "token:dev"); err != nil {
		t.Fatal(err)
	}
	if n, err := l.Requeue(ns, thread, "s2"); err != nil || n != 0 {
		t.Fatalf("Requeue of a stopped turn = %d, %v; want 0", n, err)
	}
	if got := turns(t, l)[0]; got.State != failed || got.Error != "stopped by token:dev" {
		t.Fatalf("stopped turn after Requeue = %+v", got)
	}
}

func TestFailOpenAndMarkNotified(t *testing.T) {
	l := newLog(t)
	enqueue(t, l, "one", "two", "three", "four")
	next(t, l, "s1")
	if err := l.Complete(ns, thread, "s1", 1, Result{Outcome: OutcomeCompleted}); err != nil {
		t.Fatal(err)
	}
	next(t, l, "s1") // turn 2 running, 3 and 4 queued
	if _, _, err := l.Stop(ns, thread, "u"); err != nil {
		t.Fatal(err)
	}
	enqueue(t, l, "five") // turn 5 queued after the stop

	n, err := l.FailOpen(ns, thread, "cloud session failed: pod evicted")
	if err != nil || n != 2 {
		t.Fatalf("FailOpen = %d, %v; want 2 (turn 2 running, turn 5 queued)", n, err)
	}
	wantStates(t, l, done, failed, cancelled, cancelled, failed)
	got := turns(t, l)
	if got[1].Error != "cloud session failed: pod evicted" || got[1].CompletedAt == nil || got[0].Error != "" {
		t.Fatalf("turns after FailOpen = %+v", got)
	}
	if n, err := l.FailOpen(ns, thread, "again"); err != nil || n != 0 {
		t.Fatalf("second FailOpen = %d, %v", n, err)
	}

	if err := l.MarkNotified(ns, thread, []int{1, 2, 99}); err != nil {
		t.Fatal(err)
	}
	got = turns(t, l)
	if got[0].NotifiedAt == nil || got[1].NotifiedAt == nil || got[2].NotifiedAt != nil || got[4].NotifiedAt != nil {
		t.Fatalf("NotifiedAt = %v %v %v %v", got[0].NotifiedAt, got[1].NotifiedAt, got[2].NotifiedAt, got[4].NotifiedAt)
	}
	first := *got[0].NotifiedAt
	if err := l.MarkNotified(ns, thread, []int{1}); err != nil {
		t.Fatal(err)
	}
	if again := turns(t, l)[0].NotifiedAt; !again.Equal(first) {
		t.Errorf("NotifiedAt moved from %v to %v", first, again)
	}
}

func TestConcurrentEnqueueAndNext(t *testing.T) {
	l := newLog(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { enqueue(t, l, "m") })
	}
	wg.Wait()
	ids := map[int]bool{}
	for _, tr := range turns(t, l) {
		ids[tr.ID] = true
	}
	if len(ids) != 20 {
		t.Fatalf("%d distinct ids after 20 concurrent Enqueues, want 20", len(ids))
	}
	// Concurrent sessions never take the same turn.
	taken := make(chan int, 20)
	for i := range 5 {
		wg.Go(func() {
			session := string(rune('a' + i))
			for {
				got, err := l.Next(context.Background(), ns, thread, session, 0)
				if err != nil {
					t.Error(err)
					return
				}
				if got == nil {
					return
				}
				taken <- got.ID
				if err := l.Complete(ns, thread, session, got.ID, Result{Outcome: OutcomeCompleted}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	close(taken)
	seen := map[int]bool{}
	for id := range taken {
		if seen[id] {
			t.Fatalf("turn %d was handed out twice", id)
		}
		seen[id] = true
	}
	if len(seen) != 20 {
		t.Fatalf("%d turns handed out, want 20", len(seen))
	}
}
