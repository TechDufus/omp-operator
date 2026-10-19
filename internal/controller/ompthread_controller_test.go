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

package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/threads"
)

func threadFixture() *ompv1alpha1.OmpThread {
	return &ompv1alpha1.OmpThread{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "coder-k3x9fa",
			CreationTimestamp: metav1.NewTime(time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)),
		},
		Spec: ompv1alpha1.OmpThreadSpec{
			Repo: ompv1alpha1.ThreadRepo{BaseCommit: "1111111111111111111111111111111111111111"},
		},
	}
}

func TestThreadStatusFromTurns(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 10, 0, 0, 123456789, time.UTC)
	at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }
	th := threadFixture()

	tests := []struct {
		name       string
		turns      []threads.Turn
		wantPhase  ompv1alpha1.ThreadPhase
		wantQueued int32
		wantLatest int32 // 0: none
		wantHead   string
		wantLast   time.Time
	}{
		{
			name:      "no turns is Idle since creation",
			wantPhase: ompv1alpha1.ThreadPhaseIdle,
			wantLast:  th.CreationTimestamp.Time,
		},
		{
			name: "a queued turn makes it Working; latest is the newest finished one",
			turns: []threads.Turn{
				{ID: 1, State: ompv1alpha1.TurnStateDone, QueuedAt: t0, CompletedAt: at(time.Minute), Head: "aaaa", Title: "T1"},
				{ID: 2, State: ompv1alpha1.TurnStateFailed, QueuedAt: t0, CompletedAt: at(2 * time.Minute), Error: "boom"},
				{ID: 3, State: ompv1alpha1.TurnStateQueued, QueuedAt: *at(3 * time.Minute)},
			},
			wantPhase: ompv1alpha1.ThreadPhaseWorking, wantQueued: 1, wantLatest: 2, wantHead: "aaaa",
			wantLast: t0.Add(3 * time.Minute),
		},
		{
			name: "a running turn is Working",
			turns: []threads.Turn{
				{ID: 1, State: ompv1alpha1.TurnStateRunning, QueuedAt: t0, StartedAt: at(time.Second)},
			},
			wantPhase: ompv1alpha1.ThreadPhaseWorking, wantLast: t0.Add(time.Second),
		},
		{
			name: "cancelled turns do not count as latest",
			turns: []threads.Turn{
				{ID: 1, State: ompv1alpha1.TurnStateDone, QueuedAt: t0, CompletedAt: at(time.Minute)},
				{ID: 2, State: ompv1alpha1.TurnStateCancelled, QueuedAt: t0, CompletedAt: at(2 * time.Minute)},
			},
			wantPhase: ompv1alpha1.ThreadPhaseIdle, wantLatest: 1, wantLast: t0.Add(2 * time.Minute),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := threadStatus(th, tt.turns, "coder-k3x9fa-1")
			if st.phase != tt.wantPhase || st.queued != tt.wantQueued || st.turns != int32(len(tt.turns)) {
				t.Errorf("phase %s queued %d turns %d", st.phase, st.queued, st.turns)
			}
			if (st.latest == nil) != (tt.wantLatest == 0) || (st.latest != nil && st.latest.ID != tt.wantLatest) {
				t.Errorf("latest = %+v, want id %d", st.latest, tt.wantLatest)
			}
			if (st.result == nil) != (tt.wantHead == "") || (st.result != nil && (st.result.Head != tt.wantHead ||
				st.result.Branch != "omp-cloud/coder-k3x9fa" || st.result.Base != th.Spec.Repo.BaseCommit)) {
				t.Errorf("result = %+v, want head %q", st.result, tt.wantHead)
			}
			if !st.lastActivity.Equal(tt.wantLast) {
				t.Errorf("lastActivity = %v, want %v", st.lastActivity, tt.wantLast)
			}
		})
	}
}

func TestThreadStatusTruncatesSummary(t *testing.T) {
	turns := []threads.Turn{{
		ID: 1, State: ompv1alpha1.TurnStateFailed, QueuedAt: time.Now(),
		Title: strings.Repeat("t", 200), Reply: strings.Repeat("r", 9000), Error: strings.Repeat("e", 3000),
	}}
	l := threadStatus(threadFixture(), turns, "").latest
	if len(l.Title) > 80 || len(l.Reply) > 3000 || len(l.Error) > 1000 {
		t.Errorf("summary lengths = %d %d %d", len(l.Title), len(l.Reply), len(l.Error))
	}
}

// Times are stored at second precision; a status written from nanosecond turn times must not
// look changed on the next reconcile, or every reconcile would patch the thread.
func TestApplyThreadStatusIsIdempotent(t *testing.T) {
	th := threadFixture()
	now := time.Date(2026, 10, 18, 10, 0, 0, 987654321, time.UTC)
	done := now.Add(time.Minute)
	st := threadStatus(th, []threads.Turn{{
		ID: 1, State: ompv1alpha1.TurnStateDone, QueuedAt: now, CompletedAt: &done, Head: "abc", Reply: "ok",
	}}, "")

	if !applyThreadStatus(th, st) {
		t.Fatal("first apply reported no change")
	}
	// What the API server hands back: the same status, times cut to seconds.
	stored := &ompv1alpha1.OmpThread{}
	raw, err := json.Marshal(th)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, stored); err != nil {
		t.Fatal(err)
	}
	if applyThreadStatus(stored, st) {
		t.Errorf("second apply reported a change: %+v", stored.Status)
	}
	if st.phase != ompv1alpha1.ThreadPhaseIdle || stored.Status.Phase != ompv1alpha1.ThreadPhaseIdle {
		t.Errorf("phase = %s", stored.Status.Phase)
	}

	st.queued, st.phase = 1, ompv1alpha1.ThreadPhaseWorking
	if !applyThreadStatus(stored, st) {
		t.Error("a changed status reported no change")
	}
}

func TestActiveSittingAndNumbering(t *testing.T) {
	sitting := func(name string, phase ompv1alpha1.SessionPhase) ompv1alpha1.OmpSession {
		return ompv1alpha1.OmpSession{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: ompv1alpha1.OmpSessionStatus{Phase: phase}}
	}
	if got := activeSitting(nil); got != nil {
		t.Errorf("no sittings: %v", got)
	}
	ended := []ompv1alpha1.OmpSession{
		sitting("t-1", ompv1alpha1.SessionPhaseSucceeded), sitting("t-2", ompv1alpha1.SessionPhaseFailed),
	}
	if got := activeSitting(ended); got != nil {
		t.Errorf("only ended sittings: %v", got.Name)
	}
	for _, phase := range []ompv1alpha1.SessionPhase{"", ompv1alpha1.SessionPhasePending, ompv1alpha1.SessionPhaseRunning} {
		all := append(ended[:2:2], sitting("t-3", phase))
		if got := activeSitting(all); got == nil || got.Name != "t-3" {
			t.Errorf("phase %q: active = %v", phase, got)
		}
	}

	for name, want := range map[string]int{"t-1": 1, "t-12": 12, "t-x": 0, "other-3": 0} {
		if got := sittingNumber("t", name); got != want {
			t.Errorf("sittingNumber(t, %s) = %d, want %d", name, got, want)
		}
	}
}
