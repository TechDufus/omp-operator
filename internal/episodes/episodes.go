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

// Package episodes holds the pure grouping, episode, dedupe and budget
// decisions for webhook events. It has no Kubernetes dependency: the hooks
// layer converts between GroupState and the OmpTrigger status group.
package episodes

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"
)

const (
	// StatusFiring and StatusResolved are the normalized event statuses.
	StatusFiring   = "firing"
	StatusResolved = "resolved"

	// SeverityCritical is the only severity exempt from the non-critical cap.
	SeverityCritical = "critical"

	// MaxTriaged is the number of triaged fingerprints a group keeps.
	MaxTriaged = 50
	// MaxGroups is the number of groups a trigger keeps in its status.
	MaxGroups = 100

	resolvedRetention = 24 * time.Hour
)

// Result is the outcome of Decide.
type Result string

// Decision results.
const (
	// ResultSession means a session should be created.
	ResultSession Result = "session"
	// ResultResolved means a resolved event closed a known group.
	ResultResolved Result = "resolved"
	// ResultFiltered means the event is ignored (resolved without a group,
	// or firing without fingerprints).
	ResultFiltered Result = "filtered"
	// ResultDeduped means there is nothing new to triage yet.
	ResultDeduped Result = "deduped"
	// ResultBusy means a session is already Pending or Running in the group.
	ResultBusy Result = "busy"
	// ResultCapped means a session budget is exhausted; see Decision.Reason.
	ResultCapped Result = "capped"
)

// Decision reasons.
const (
	ReasonNewEpisode     = "new episode"
	ReasonNewAlerts      = "new alerts in group"
	ReasonGroupLimit     = "group limit"
	ReasonDailyCap       = "daily cap"
	ReasonNonCriticalCap = "non-critical cap"
)

// GroupState mirrors an OmpTrigger status group.
type GroupState struct {
	ID                  string
	Name                string
	Severity            string
	EpisodeStart        time.Time
	LastNotified        time.Time
	ResolvedAt          *time.Time
	TriagedFingerprints []string
	LastSession         string
	LastSessionAt       *time.Time
}

// Input is the part of a normalized event that Decide needs.
type Input struct {
	// Key is the event key; it determines the group ID of a new group.
	Key string
	// Status is StatusFiring or StatusResolved.
	Status       string
	Name         string
	Severity     string
	Fingerprints []string
}

// Config holds the limits that apply to a trigger and its agent. The hooks
// layer applies defaults (episodeGap 30m, retriageAfter 1h, perGroupPerDay 2,
// eventSessionsPerDay 12, nonCriticalSessionsPerDay 8; see DefaultConfig);
// Decide uses the values literally, so a zero limit caps every session.
type Config struct {
	EpisodeGap                time.Duration
	RetriageAfter             time.Duration
	PerGroupPerDay            int
	EventSessionsPerDay       int
	NonCriticalSessionsPerDay int
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		EpisodeGap:                30 * time.Minute,
		RetriageAfter:             time.Hour,
		PerGroupPerDay:            2,
		EventSessionsPerDay:       12,
		NonCriticalSessionsPerDay: 8,
	}
}

// Counts are the live facts the caller reads (through the API reader) before
// calling Decide.
type Counts struct {
	// Busy reports a Pending or Running session in this trigger's group.
	Busy bool
	// GroupSessions24h counts sessions created in this group in the last 24 h.
	GroupSessions24h int
	// AgentEventSessions24h counts the agent's webhook-created sessions in
	// the last 24 h.
	AgentEventSessions24h int
	// AgentNonCritical24h is the subset of those with severity != critical.
	AgentNonCritical24h int
}

// Decision is the result of Decide.
type Decision struct {
	Result Result
	// Reason is set for session and capped results.
	Reason string
	// NewFingerprints are the fingerprints not yet triaged; set whenever the
	// event was firing and had any. Pass them to MarkSession on a session.
	NewFingerprints []string
	// State is the group state after the decision. It is nil only when the
	// event was filtered and no group exists. It is the same pointer that was
	// passed to Decide when that was non-nil, otherwise a newly allocated
	// state the caller must store.
	State *GroupState
}

// GroupID returns the first 16 hex characters of sha256(key).
func GroupID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

// Decide applies the episode rules to one normalized event.
//
// state is the existing group (nil when none). Decide mutates a non-nil state
// in place and allocates a new one when state is nil and the event is firing
// with fingerprints; the result is in Decision.State. Episode bookkeeping
// (episode start, resolvedAt, lastNotified, name, severity) is applied for
// every firing event with fingerprints, whatever the outcome, so the caller
// persists State whenever it is non-nil. Decide does not record a session:
// on ResultSession the caller creates the session and then calls MarkSession.
func Decide(state *GroupState, ev Input, now time.Time, cfg Config, counts Counts) Decision {
	if ev.Status == StatusResolved {
		if state == nil {
			return Decision{Result: ResultFiltered}
		}
		state.ResolvedAt = new(now)
		return Decision{Result: ResultResolved, State: state}
	}
	if len(ev.Fingerprints) == 0 {
		return Decision{Result: ResultFiltered, State: state}
	}

	newEpisode := false
	switch {
	case state == nil:
		state = &GroupState{ID: GroupID(ev.Key)}
		newEpisode = true
	case state.ResolvedAt != nil && now.Sub(*state.ResolvedAt) > cfg.EpisodeGap,
		state.EpisodeStart.IsZero():
		newEpisode = true
	}
	if newEpisode {
		state.EpisodeStart = now
		state.TriagedFingerprints = nil
	}
	state.ResolvedAt = nil
	state.LastNotified = now
	if ev.Name != "" {
		state.Name = ev.Name
	}
	state.Severity = ev.Severity

	fresh := newFingerprints(state.TriagedFingerprints, ev.Fingerprints)
	d := Decision{NewFingerprints: fresh, State: state}
	if len(fresh) == 0 ||
		(state.LastSessionAt != nil && now.Sub(*state.LastSessionAt) < cfg.RetriageAfter) {
		d.Result = ResultDeduped
		return d
	}
	if counts.Busy {
		d.Result = ResultBusy
		return d
	}
	switch {
	case counts.GroupSessions24h >= cfg.PerGroupPerDay:
		d.Result, d.Reason = ResultCapped, ReasonGroupLimit
	case counts.AgentEventSessions24h >= cfg.EventSessionsPerDay:
		d.Result, d.Reason = ResultCapped, ReasonDailyCap
	case ev.Severity != SeverityCritical && counts.AgentNonCritical24h >= cfg.NonCriticalSessionsPerDay:
		d.Result, d.Reason = ResultCapped, ReasonNonCriticalCap
	default:
		d.Result = ResultSession
		d.Reason = ReasonNewAlerts
		if newEpisode {
			d.Reason = ReasonNewEpisode
		}
	}
	return d
}

// MarkSession records a created session in state: it appends fingerprints to
// the triaged list (keeping the newest MaxTriaged) and sets lastSession and
// lastSessionAt. Call it after creating the session for a ResultSession
// decision, with that decision's NewFingerprints.
func MarkSession(state *GroupState, name string, now time.Time, fingerprints []string) {
	for _, fp := range fingerprints {
		if !slices.Contains(state.TriagedFingerprints, fp) {
			state.TriagedFingerprints = append(state.TriagedFingerprints, fp)
		}
	}
	if n := len(state.TriagedFingerprints); n > MaxTriaged {
		state.TriagedFingerprints = slices.Clone(state.TriagedFingerprints[n-MaxTriaged:])
	}
	state.LastSession = name
	state.LastSessionAt = new(now)
}

// Prune returns groups without those resolved more than 24 h before now.
// When more than MaxGroups remain, the oldest resolved groups are dropped
// first, and if that is not enough the groups notified longest ago, so the
// result always fits the CRD limit. Order of the kept groups is preserved.
func Prune(groups []GroupState, now time.Time) []GroupState {
	kept := make([]GroupState, 0, len(groups))
	for _, g := range groups {
		if g.ResolvedAt != nil && now.Sub(*g.ResolvedAt) > resolvedRetention {
			continue
		}
		kept = append(kept, g)
	}
	for len(kept) > MaxGroups {
		i := evictionIndex(kept)
		kept = slices.Delete(kept, i, i+1)
	}
	return kept
}

// evictionIndex picks the group to drop: the oldest resolved, else the one
// with the oldest lastNotified.
func evictionIndex(groups []GroupState) int {
	oldestResolved, oldestNotified := -1, 0
	for i, g := range groups {
		if g.ResolvedAt != nil &&
			(oldestResolved < 0 || g.ResolvedAt.Before(*groups[oldestResolved].ResolvedAt)) {
			oldestResolved = i
		}
		if g.LastNotified.Before(groups[oldestNotified].LastNotified) {
			oldestNotified = i
		}
	}
	if oldestResolved >= 0 {
		return oldestResolved
	}
	return oldestNotified
}

// newFingerprints returns the distinct fingerprints of in that are not in
// triaged, in input order.
func newFingerprints(triaged, in []string) []string {
	var out []string
	for _, fp := range in {
		if !slices.Contains(triaged, fp) && !slices.Contains(out, fp) {
			out = append(out, fp)
		}
	}
	return out
}
