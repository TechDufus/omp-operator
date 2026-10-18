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

package episodes

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func firing(sev string, fps ...string) Input {
	return Input{Key: "k", Status: StatusFiring, Name: "HighCPU/prod", Severity: sev, Fingerprints: fps}
}

func TestGroupID(t *testing.T) {
	// sha256("abc") = ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
	if got := GroupID("abc"); got != "ba7816bf8f01cfea" {
		t.Fatalf("GroupID = %q", got)
	}
}

func TestDecide(t *testing.T) {
	cfg := DefaultConfig()
	tests := []struct {
		name      string
		state     *GroupState
		ev        Input
		counts    Counts
		want      Result
		reason    string
		wantNew   []string
		wantState bool
		check     func(t *testing.T, st *GroupState)
	}{
		{
			name: "resolved without state is filtered",
			ev:   Input{Key: "k", Status: StatusResolved},
			want: ResultFiltered,
		},
		{
			name:      "resolved with state sets resolvedAt",
			state:     &GroupState{ID: "g", EpisodeStart: t0.Add(-time.Hour)},
			ev:        Input{Key: "k", Status: StatusResolved},
			want:      ResultResolved,
			wantState: true,
			check: func(t *testing.T, st *GroupState) {
				if st.ResolvedAt == nil || !st.ResolvedAt.Equal(t0) {
					t.Fatalf("resolvedAt = %v", st.ResolvedAt)
				}
			},
		},
		{
			name: "firing without fingerprints is filtered and creates no state",
			ev:   firing("critical"),
			want: ResultFiltered,
		},
		{
			name:      "firing without fingerprints leaves existing state untouched",
			state:     &GroupState{ID: "g", EpisodeStart: t0.Add(-time.Hour), ResolvedAt: new(t0.Add(-time.Hour))},
			ev:        firing("critical"),
			want:      ResultFiltered,
			wantState: true,
			check: func(t *testing.T, st *GroupState) {
				if st.ResolvedAt == nil || !st.LastNotified.IsZero() {
					t.Fatalf("state mutated: %+v", st)
				}
			},
		},
		{
			name:      "new episode",
			ev:        firing("warning", "a", "b"),
			want:      ResultSession,
			reason:    ReasonNewEpisode,
			wantNew:   []string{"a", "b"},
			wantState: true,
			check: func(t *testing.T, st *GroupState) {
				if st.ID != GroupID("k") || !st.EpisodeStart.Equal(t0) || !st.LastNotified.Equal(t0) ||
					st.Name != "HighCPU/prod" || st.Severity != "warning" {
					t.Fatalf("state = %+v", st)
				}
			},
		},
		{
			name: "reopen within gap continues the episode",
			state: &GroupState{
				ID: "g", EpisodeStart: t0.Add(-2 * time.Hour), ResolvedAt: new(t0.Add(-10 * time.Minute)),
				TriagedFingerprints: []string{"a"}, LastSessionAt: new(t0.Add(-2 * time.Hour)),
			},
			ev:        firing("warning", "a", "b"),
			want:      ResultSession,
			reason:    ReasonNewAlerts,
			wantNew:   []string{"b"},
			wantState: true,
			check: func(t *testing.T, st *GroupState) {
				if st.ResolvedAt != nil || !st.EpisodeStart.Equal(t0.Add(-2*time.Hour)) ||
					!slices.Equal(st.TriagedFingerprints, []string{"a"}) {
					t.Fatalf("state = %+v", st)
				}
			},
		},
		{
			name: "resolved exactly at the gap still continues",
			state: &GroupState{
				ID: "g", EpisodeStart: t0.Add(-2 * time.Hour), ResolvedAt: new(t0.Add(-cfg.EpisodeGap)),
				TriagedFingerprints: []string{"a"},
			},
			ev:        firing("warning", "a", "b"),
			want:      ResultSession,
			reason:    ReasonNewAlerts,
			wantNew:   []string{"b"},
			wantState: true,
		},
		{
			name: "after the gap a new episode starts and clears triaged",
			state: &GroupState{
				ID: "g", EpisodeStart: t0.Add(-3 * time.Hour), ResolvedAt: new(t0.Add(-31 * time.Minute)),
				TriagedFingerprints: []string{"a"}, LastSessionAt: new(t0.Add(-3 * time.Hour)),
			},
			ev:        firing("warning", "a"),
			want:      ResultSession,
			reason:    ReasonNewEpisode,
			wantNew:   []string{"a"},
			wantState: true,
			check: func(t *testing.T, st *GroupState) {
				if st.ResolvedAt != nil || !st.EpisodeStart.Equal(t0) || len(st.TriagedFingerprints) != 0 {
					t.Fatalf("state = %+v", st)
				}
			},
		},
		{
			name: "all fingerprints triaged is deduped but notified",
			state: &GroupState{
				ID: "g", EpisodeStart: t0.Add(-time.Hour), TriagedFingerprints: []string{"a", "b"},
			},
			ev:        firing("warning", "a", "b"),
			want:      ResultDeduped,
			wantState: true,
			check: func(t *testing.T, st *GroupState) {
				if !st.LastNotified.Equal(t0) {
					t.Fatalf("lastNotified = %v", st.LastNotified)
				}
			},
		},
		{
			name: "new fingerprints inside retriageAfter are deduped and stay untriaged",
			state: &GroupState{
				ID: "g", EpisodeStart: t0.Add(-time.Hour), TriagedFingerprints: []string{"a"},
				LastSessionAt: new(t0.Add(-59 * time.Minute)),
			},
			ev:        firing("warning", "a", "b"),
			want:      ResultDeduped,
			wantNew:   []string{"b"},
			wantState: true,
			check: func(t *testing.T, st *GroupState) {
				if !slices.Equal(st.TriagedFingerprints, []string{"a"}) {
					t.Fatalf("triaged = %v", st.TriagedFingerprints)
				}
			},
		},
		{
			name: "session once retriageAfter has passed",
			state: &GroupState{
				ID: "g", EpisodeStart: t0.Add(-2 * time.Hour), TriagedFingerprints: []string{"a"},
				LastSessionAt: new(t0.Add(-time.Hour)),
			},
			ev:        firing("warning", "a", "b"),
			want:      ResultSession,
			reason:    ReasonNewAlerts,
			wantNew:   []string{"b"},
			wantState: true,
		},
		{
			name:      "duplicate fingerprints in the event count once",
			ev:        firing("warning", "a", "a"),
			want:      ResultSession,
			reason:    ReasonNewEpisode,
			wantNew:   []string{"a"},
			wantState: true,
		},
		{
			name:      "busy",
			ev:        firing("critical", "a"),
			counts:    Counts{Busy: true},
			want:      ResultBusy,
			wantNew:   []string{"a"},
			wantState: true,
		},
		{
			name:      "busy is checked before caps",
			ev:        firing("critical", "a"),
			counts:    Counts{Busy: true, GroupSessions24h: 99},
			want:      ResultBusy,
			wantNew:   []string{"a"},
			wantState: true,
		},
		{
			name:      "group limit",
			ev:        firing("critical", "a"),
			counts:    Counts{GroupSessions24h: 2},
			want:      ResultCapped,
			reason:    ReasonGroupLimit,
			wantNew:   []string{"a"},
			wantState: true,
		},
		{
			name:      "group limit wins over daily cap",
			ev:        firing("warning", "a"),
			counts:    Counts{GroupSessions24h: 2, AgentEventSessions24h: 12, AgentNonCritical24h: 8},
			want:      ResultCapped,
			reason:    ReasonGroupLimit,
			wantNew:   []string{"a"},
			wantState: true,
		},
		{
			name:      "daily cap applies to critical",
			ev:        firing("critical", "a"),
			counts:    Counts{GroupSessions24h: 1, AgentEventSessions24h: 12},
			want:      ResultCapped,
			reason:    ReasonDailyCap,
			wantNew:   []string{"a"},
			wantState: true,
		},
		{
			name:      "daily cap wins over non-critical cap",
			ev:        firing("warning", "a"),
			counts:    Counts{AgentEventSessions24h: 12, AgentNonCritical24h: 8},
			want:      ResultCapped,
			reason:    ReasonDailyCap,
			wantNew:   []string{"a"},
			wantState: true,
		},
		{
			name:      "non-critical cap",
			ev:        firing("warning", "a"),
			counts:    Counts{AgentEventSessions24h: 11, AgentNonCritical24h: 8},
			want:      ResultCapped,
			reason:    ReasonNonCriticalCap,
			wantNew:   []string{"a"},
			wantState: true,
		},
		{
			name:      "critical ignores the non-critical cap",
			ev:        firing("critical", "a"),
			counts:    Counts{AgentEventSessions24h: 11, AgentNonCritical24h: 8},
			want:      ResultSession,
			reason:    ReasonNewEpisode,
			wantNew:   []string{"a"},
			wantState: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.state, tt.ev, t0, cfg, tt.counts)
			if d.Result != tt.want || d.Reason != tt.reason {
				t.Fatalf("got %s/%q, want %s/%q", d.Result, d.Reason, tt.want, tt.reason)
			}
			if !slices.Equal(d.NewFingerprints, tt.wantNew) {
				t.Fatalf("new fingerprints = %v, want %v", d.NewFingerprints, tt.wantNew)
			}
			if (d.State != nil) != tt.wantState {
				t.Fatalf("state present = %v, want %v", d.State != nil, tt.wantState)
			}
			if tt.state != nil && d.State != tt.state {
				t.Fatal("Decide must return the passed state pointer")
			}
			if tt.check != nil {
				tt.check(t, d.State)
			}
		})
	}
}

func TestMarkSession(t *testing.T) {
	st := &GroupState{TriagedFingerprints: []string{"a"}}
	MarkSession(st, "sess-1", t0, []string{"a", "b"})
	if !slices.Equal(st.TriagedFingerprints, []string{"a", "b"}) {
		t.Fatalf("triaged = %v", st.TriagedFingerprints)
	}
	if st.LastSession != "sess-1" || st.LastSessionAt == nil || !st.LastSessionAt.Equal(t0) {
		t.Fatalf("state = %+v", st)
	}

	many := make([]string, 0, 60)
	for i := range 60 {
		many = append(many, fmt.Sprintf("fp%02d", i))
	}
	MarkSession(st, "sess-2", t0, many)
	if len(st.TriagedFingerprints) != MaxTriaged {
		t.Fatalf("len = %d", len(st.TriagedFingerprints))
	}
	if st.TriagedFingerprints[0] != "fp10" || st.TriagedFingerprints[MaxTriaged-1] != "fp59" {
		t.Fatalf("not newest 50: %v", st.TriagedFingerprints)
	}
}

func TestPrune(t *testing.T) {
	resolved := func(id string, ago time.Duration) GroupState {
		return GroupState{ID: id, ResolvedAt: new(t0.Add(-ago)), LastNotified: t0.Add(-ago)}
	}
	active := func(id string, ago time.Duration) GroupState {
		return GroupState{ID: id, LastNotified: t0.Add(-ago)}
	}
	ids := func(gs []GroupState) []string {
		out := make([]string, 0, len(gs))
		for _, g := range gs {
			out = append(out, g.ID)
		}
		return out
	}

	t.Run("drops groups resolved over 24h ago", func(t *testing.T) {
		got := Prune([]GroupState{
			resolved("old", 25*time.Hour), active("live", time.Hour),
			resolved("recent", time.Hour), resolved("edge", 24*time.Hour),
		}, t0)
		if want := []string{"live", "recent", "edge"}; !slices.Equal(ids(got), want) {
			t.Fatalf("got %v, want %v", ids(got), want)
		}
	})

	t.Run("above 100 drops oldest resolved first", func(t *testing.T) {
		gs := make([]GroupState, 0, 102)
		for i := range 99 {
			gs = append(gs, active(fmt.Sprintf("a%d", i), time.Duration(i+1)*time.Hour*100))
		}
		gs = append(gs, resolved("r-new", time.Minute), resolved("r-mid", time.Hour), resolved("r-old", 10*time.Hour))
		got := Prune(gs, t0)
		if len(got) != MaxGroups {
			t.Fatalf("len = %d", len(got))
		}
		if slices.Contains(ids(got), "r-old") || slices.Contains(ids(got), "r-mid") || !slices.Contains(ids(got), "r-new") {
			t.Fatalf("wrong groups dropped: %v", ids(got)[95:])
		}
	})

	t.Run("without resolved groups the longest-silent group goes", func(t *testing.T) {
		gs := make([]GroupState, 0, 101)
		for i := range 101 {
			gs = append(gs, active(fmt.Sprintf("a%d", i), time.Duration(i)*time.Minute))
		}
		got := Prune(gs, t0)
		if len(got) != MaxGroups || slices.Contains(ids(got), "a100") || !slices.Contains(ids(got), "a0") {
			t.Fatalf("len=%d", len(got))
		}
	})
}
