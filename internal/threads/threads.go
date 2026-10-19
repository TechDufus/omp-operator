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

// Package threads is the turn log of cloud threads: the messages a user sent
// to a thread, which sitting (OmpSession) is working on which, and how each
// turned out. It lives in the thread's turns.json in the store, so it is
// outside the Kubernetes API and cheap to update. The cloud API enqueues turns,
// the in-pod driver takes and completes them through the agent API, and the
// thread reconciler reads them, requeues them when a sitting dies and notifies
// about finished ones.
//
// A turn moves queued -> running -> done | failed, or queued -> cancelled.
// Every mutation is an atomic read-modify-write of turns.json
// (store.UpdateThreadFile), so concurrent API handlers never lose an update.
// Mutations do not wake waiters or the reconciler by themselves; callers call
// Signal afterwards.
//
// The package imports no Kubernetes client and neither agentapi nor podspec.
package threads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/prompt"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	// MaxTurns is the most turns a thread holds, in any state; Enqueue refuses more.
	MaxTurns = 200
	// MaxTextBytes is the longest accepted message.
	MaxTextBytes = 32 << 10

	// OutcomeCompleted is the Result.Outcome of a turn the agent finished; any
	// other outcome fails the turn.
	OutcomeCompleted = "completed"

	turnsFile    = "turns.json"
	eventsBuffer = 1024
)

var (
	// ErrThreadFull is returned by Enqueue when the thread already holds MaxTurns turns.
	ErrThreadFull = errors.New("thread is full")
	// ErrNotRunning is returned by Complete when the turn does not exist, is not
	// running, or is running for another session. A driver that retried a
	// Complete whose response was lost sees it and treats the turn as done.
	ErrNotRunning = errors.New("turn is not running for this session")
	// ErrInvalidText is returned by Enqueue for a message that is empty or longer than MaxTextBytes.
	ErrInvalidText = fmt.Errorf("message must be 1 to %d bytes", MaxTextBytes)

	errUnchanged = errors.New("unchanged")
)

// Usage is token and cost accounting; it has the fields and JSON names of agentapi.Usage.
type Usage struct {
	InputTokens      int64  `json:"inputTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	CacheReadTokens  int64  `json:"cacheReadTokens"`
	CacheWriteTokens int64  `json:"cacheWriteTokens"`
	Cost             string `json:"cost"`
}

// Turn is one message of a thread and what became of it.
type Turn struct {
	ID       int                `json:"id"`
	Text     string             `json:"text"`
	By       string             `json:"by"`
	QueuedAt time.Time          `json:"queuedAt"`
	State    v1alpha1.TurnState `json:"state"`
	// Session is the OmpSession (sitting) that took the turn.
	Session     string     `json:"session,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	// NotifiedAt is set by MarkNotified once the turn's sinks were told.
	NotifiedAt *time.Time `json:"notifiedAt,omitempty"`
	// StopRequested is set by Stop on the running turn; the driver aborts it.
	StopRequested bool   `json:"stopRequested,omitempty"`
	StoppedBy     string `json:"stoppedBy,omitempty"`
	Reply         string `json:"reply,omitempty"`
	Title         string `json:"title,omitempty"`
	Error         string `json:"error,omitempty"`
	Usage         *Usage `json:"usage,omitempty"`

	// Head is the result branch tip after the turn, with its diffstat against the base.
	Head         string `json:"head,omitempty"`
	FilesChanged int    `json:"filesChanged,omitempty"`
	Insertions   int    `json:"insertions,omitempty"`
	Deletions    int    `json:"deletions,omitempty"`
	Pushed       bool   `json:"pushed,omitempty"`
	PushError    string `json:"pushError,omitempty"`

	// Questions are the questions the agent asked during the turn (omp's ask tool).
	Questions []Question `json:"questions,omitempty"`
}

// Finished reports whether the turn will not change any more: done, failed or cancelled.
func (t Turn) Finished() bool {
	switch t.State {
	case v1alpha1.TurnStateDone, v1alpha1.TurnStateFailed, v1alpha1.TurnStateCancelled:
		return true
	}
	return false
}

// Result is what the driver reports for a turn.
type Result struct {
	// Outcome is OutcomeCompleted for a turn the agent finished; anything else fails it.
	Outcome string
	Error   string
	Reply   string
	Usage   Usage
	Head    string

	FilesChanged int
	Insertions   int
	Deletions    int
	Pushed       bool
	PushError    string
}

// Log is the turn log of every thread, over the store. Build it with NewLog.
type Log struct {
	Store *store.Store
	// Now is the clock; NewLog sets time.Now.
	Now func() time.Time

	events chan event.GenericEvent

	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{} // "ns/thread" -> Next waiters
}

// NewLog returns a Log over st.
func NewLog(st *store.Store) *Log {
	return &Log{
		Store:   st,
		Now:     time.Now,
		events:  make(chan event.GenericEvent, eventsBuffer),
		waiters: map[string]map[chan struct{}]struct{}{},
	}
}

func (l *Log) now() time.Time { return l.Now().UTC() }

func key(ns, thread string) string { return ns + "/" + thread }

// Events is the source of reconcile requests: Signal sends an OmpThread with
// only namespace and name set. The channel is buffered; events are dropped when
// it is full, which only delays a reconcile that the thread's periodic requeue
// covers anyway.
func (l *Log) Events() <-chan event.GenericEvent { return l.events }

// Signal wakes the Next waiters of the thread and asks the thread reconciler to
// reconcile it. It never blocks.
func (l *Log) Signal(ns, thread string) {
	l.mu.Lock()
	for ch := range l.waiters[key(ns, thread)] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	l.mu.Unlock()

	var obj client.Object = &v1alpha1.OmpThread{}
	obj.SetNamespace(ns)
	obj.SetName(thread)
	select {
	case l.events <- event.GenericEvent{Object: obj}:
	default:
	}
}

func (l *Log) subscribe(k string) (ch chan struct{}, cancel func()) {
	ch = make(chan struct{}, 1)
	l.mu.Lock()
	if l.waiters[k] == nil {
		l.waiters[k] = map[chan struct{}]struct{}{}
	}
	l.waiters[k][ch] = struct{}{}
	l.mu.Unlock()
	return ch, func() {
		l.mu.Lock()
		delete(l.waiters[k], ch)
		if len(l.waiters[k]) == 0 {
			delete(l.waiters, k)
		}
		l.mu.Unlock()
	}
}

func decode(raw []byte) ([]Turn, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var ts []Turn
	if err := json.Unmarshal(raw, &ts); err != nil {
		return nil, fmt.Errorf("decode %s: %w", turnsFile, err)
	}
	return ts, nil
}

// update is an atomic read-modify-write of the thread's turns. fn edits the
// turns in place and reports whether it changed anything; without a change
// nothing is written.
func (l *Log) update(ns, thread string, fn func(ts *[]Turn) (changed bool, err error)) error {
	err := l.Store.UpdateThreadFile(ns, thread, turnsFile, func(old []byte) ([]byte, error) {
		ts, err := decode(old)
		if err != nil {
			return nil, err
		}
		changed, err := fn(&ts)
		if err != nil {
			return nil, err
		}
		if !changed {
			return nil, errUnchanged
		}
		return json.Marshal(ts)
	})
	if errors.Is(err, errUnchanged) {
		return nil
	}
	return err
}

// Turns returns the thread's turns in the order they were enqueued; a thread
// without turns yields nil.
func (l *Log) Turns(ns, thread string) ([]Turn, error) {
	f, err := l.Store.OpenThreadFile(ns, thread, turnsFile)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return decode(raw)
}

// Enqueue appends a queued turn with the next id (ids start at 1 and are never
// reused) and returns it. text must be 1 to MaxTextBytes bytes (ErrInvalidText);
// a thread already holding MaxTurns turns returns ErrThreadFull.
func (l *Log) Enqueue(ns, thread, text, by string) (Turn, error) {
	if text == "" || len(text) > MaxTextBytes {
		return Turn{}, ErrInvalidText
	}
	var added Turn
	err := l.update(ns, thread, func(ts *[]Turn) (bool, error) {
		if len(*ts) >= MaxTurns {
			return false, ErrThreadFull
		}
		id := 1
		for _, t := range *ts {
			id = max(id, t.ID+1)
		}
		added = Turn{ID: id, Text: text, By: by, QueuedAt: l.now(), State: v1alpha1.TurnStateQueued}
		*ts = append(*ts, added)
		return true, nil
	})
	if err != nil {
		return Turn{}, err
	}
	return added, nil
}

// Next hands the session its next turn.
//
// A turn that is running for this session comes first: it was handed out
// before and its Complete never arrived (the call or the driver was retried), so
// it is delivered again unchanged. Otherwise the oldest queued turn becomes
// running for the session, with StartedAt set, and is returned.
//
// With nothing to hand out Next waits up to wait, or until Signal is called for
// the thread, looks once more and returns nil if there is still nothing. A
// cancelled ctx ends the wait with ctx.Err().
func (l *Log) Next(ctx context.Context, ns, thread, session string, wait time.Duration) (*Turn, error) {
	// Subscribe before the first look so a Signal in between is not missed.
	wake, cancel := l.subscribe(key(ns, thread))
	defer cancel()

	if t, err := l.take(ns, thread, session); err != nil || t != nil {
		return t, err
	}
	if wait <= 0 {
		return nil, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-wake:
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return l.take(ns, thread, session)
}

func (l *Log) take(ns, thread, session string) (*Turn, error) {
	var out *Turn
	err := l.update(ns, thread, func(ts *[]Turn) (bool, error) {
		for _, t := range *ts {
			if t.State == v1alpha1.TurnStateRunning && t.Session == session {
				out = new(t)
				return false, nil
			}
		}
		for i := range *ts {
			t := &(*ts)[i]
			if t.State != v1alpha1.TurnStateQueued {
				continue
			}
			now := l.now()
			t.State, t.Session, t.StartedAt = v1alpha1.TurnStateRunning, session, &now
			out = new(*t)
			return true, nil
		}
		return false, nil
	})
	return out, err
}

// Control reports whether the turn running for the session was asked to stop
// (see Stop). The driver polls it while a turn runs and aborts omp when true.
func (l *Log) Control(ns, thread, session string) (stop bool, err error) {
	ts, err := l.Turns(ns, thread)
	if err != nil {
		return false, err
	}
	for _, t := range ts {
		if t.State == v1alpha1.TurnStateRunning && t.Session == session && t.StopRequested {
			return true, nil
		}
	}
	return false, nil
}

// Complete records how turn id ended. It returns ErrNotRunning unless the turn
// is running for session. The turn becomes done for outcome OutcomeCompleted
// and failed for any other, except that a turn with StopRequested always
// becomes failed with error "stopped by <StoppedBy>". Title is derived from the
// reply.
func (l *Log) Complete(ns, thread, session string, id int, r Result) error {
	var notRunning bool
	err := l.update(ns, thread, func(ts *[]Turn) (bool, error) {
		i := slices.IndexFunc(*ts, func(t Turn) bool { return t.ID == id })
		if i < 0 || (*ts)[i].State != v1alpha1.TurnStateRunning || (*ts)[i].Session != session {
			notRunning = true
			return false, nil
		}
		t := &(*ts)[i]
		now := l.now()
		t.CompletedAt = &now
		t.State, t.Error = v1alpha1.TurnStateDone, ""
		if r.Outcome != OutcomeCompleted {
			t.State, t.Error = v1alpha1.TurnStateFailed, r.Error
		}
		if t.StopRequested {
			t.State, t.Error = v1alpha1.TurnStateFailed, "stopped by "+t.StoppedBy
		}
		t.Reply, t.Title = r.Reply, prompt.TitleFromReply(r.Reply)
		u := r.Usage
		t.Usage = &u
		t.Head, t.FilesChanged, t.Insertions, t.Deletions = r.Head, r.FilesChanged, r.Insertions, r.Deletions
		t.Pushed, t.PushError = r.Pushed, r.PushError
		return true, nil
	})
	if err != nil {
		return err
	}
	if notRunning {
		return ErrNotRunning
	}
	return nil
}

// Stop ends the thread's work on behalf of by: the running turn, if any, gets
// StopRequested and StoppedBy so its driver aborts it (the turn then completes
// as failed "stopped by <by>"), and every queued turn becomes cancelled. The
// running turn's unanswered questions are expired with AnsweredBy "stop".
// stoppedRunning reports that a turn is running and has been asked to stop;
// cancelled counts the queued turns cancelled.
func (l *Log) Stop(ns, thread, by string) (stoppedRunning bool, cancelled int, err error) {
	err = l.update(ns, thread, func(ts *[]Turn) (bool, error) {
		changed := false
		now := l.now()
		for i := range *ts {
			t := &(*ts)[i]
			switch t.State {
			case v1alpha1.TurnStateRunning:
				stoppedRunning = true
				if !t.StopRequested {
					t.StopRequested, t.StoppedBy = true, by
					changed = true
				}
				if expireOpen(t.Questions, "", AnsweredByStop, now) {
					changed = true
				}
			case v1alpha1.TurnStateQueued:
				t.State, t.CompletedAt, t.StoppedBy, t.Error = v1alpha1.TurnStateCancelled, &now, by, "cancelled by "+by
				cancelled++
				changed = true
			}
		}
		return changed, nil
	})
	if err != nil {
		return false, 0, err
	}
	return stoppedRunning, cancelled, nil
}

// Requeue puts the turns running for a dead session back in the queue, clearing
// Session, StartedAt and Questions, so the next sitting redoes them, and returns how many.
// A turn whose stop was requested is failed as "stopped by <StoppedBy>" instead
// of being redone, and is not counted.
func (l *Log) Requeue(ns, thread, session string) (int, error) {
	n := 0
	err := l.update(ns, thread, func(ts *[]Turn) (bool, error) {
		changed := false
		now := l.now()
		for i := range *ts {
			t := &(*ts)[i]
			if t.State != v1alpha1.TurnStateRunning || t.Session != session {
				continue
			}
			changed = true
			if t.StopRequested {
				t.State, t.Error, t.CompletedAt = v1alpha1.TurnStateFailed, "stopped by "+t.StoppedBy, &now
				continue
			}
			t.State, t.Session, t.StartedAt, t.Questions = v1alpha1.TurnStateQueued, "", nil, nil
			n++
		}
		return changed, nil
	})
	return n, err
}

// FailOpen fails every queued and running turn with msg, used when a sitting
// failed for good, and returns how many it failed.
func (l *Log) FailOpen(ns, thread, msg string) (int, error) {
	n := 0
	err := l.update(ns, thread, func(ts *[]Turn) (bool, error) {
		now := l.now()
		for i := range *ts {
			t := &(*ts)[i]
			if t.State != v1alpha1.TurnStateQueued && t.State != v1alpha1.TurnStateRunning {
				continue
			}
			t.State, t.Error, t.CompletedAt = v1alpha1.TurnStateFailed, msg, &now
			n++
		}
		return n > 0, nil
	})
	return n, err
}

// MarkNotified sets NotifiedAt on the turns with the given ids. Unknown ids are ignored.
func (l *Log) MarkNotified(ns, thread string, ids []int) error {
	return l.update(ns, thread, func(ts *[]Turn) (bool, error) {
		changed := false
		now := l.now()
		for i := range *ts {
			t := &(*ts)[i]
			if slices.Contains(ids, t.ID) && t.NotifiedAt == nil {
				t.NotifiedAt = &now
				changed = true
			}
		}
		return changed, nil
	})
}
