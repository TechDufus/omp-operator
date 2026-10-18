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

package hooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/adapters"
	"github.com/TechDufus/omp-operator/internal/episodes"
	"github.com/TechDufus/omp-operator/internal/metrics"
	"github.com/TechDufus/omp-operator/internal/prompt"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	// Metric result label values (omp_operator_webhook_events_total).
	resultError = "error"

	dayWindow   = 24 * time.Hour
	noticeEvery = 24 * time.Hour

	// DefaultWebhookPriority is the priority of a webhook session when the trigger sets none.
	DefaultWebhookPriority int32 = 2
	// CriticalPriority is the priority of a critical event's session.
	CriticalPriority int32 = 1

	nameCreateAttempts = 5
)

// process runs the events of one request while holding the agent's mutex and
// returns the number of sessions created. A non-nil error means an event could
// not be recorded and the sender should retry.
func (h *Handler) process(ctx context.Context, t *v1alpha1.OmpTrigger, agent *v1alpha1.OmpAgent, cfg adapters.Config, events []adapters.Event) (int, error) {
	mu, _ := h.locks.LoadOrStore(agent.Namespace+"/"+agent.Name, &sync.Mutex{})
	lock := mu.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	key := client.ObjectKeyFromObject(t)
	created := 0
	var failed error
	for _, ev := range events {
		result, err := h.handleEvent(ctx, key, agent, cfg, ev)
		if err != nil {
			log.Error(err, "handling webhook event", "namespace", key.Namespace, "trigger", key.Name, "event", ev.Key)
			failed = errors.Join(failed, err)
		}
		metrics.WebhookEventsTotal.WithLabelValues(key.Namespace, key.Name, result).Inc()
		if result == string(episodes.ResultSession) {
			created++
		}
	}
	return created, failed
}

// handleEvent returns the metric result of one event. A render failure
// counts as "error" without failing the request: the event stays untriaged.
func (h *Handler) handleEvent(ctx context.Context, key client.ObjectKey, agent *v1alpha1.OmpAgent, cfg adapters.Config, ev adapters.Event) (string, error) {
	match, err := cfg.MatchEvent(ev)
	if err != nil {
		log.V(1).Info("webhook filter error", "namespace", key.Namespace, "trigger", key.Name, "error", err.Error())
		return resultError, nil
	}
	if !match {
		return string(episodes.ResultFiltered), nil
	}

	t := &v1alpha1.OmpTrigger{}
	if err := h.APIReader.Get(ctx, key, t); err != nil {
		return resultError, err
	}
	sessions := &v1alpha1.OmpSessionList{}
	if err := h.APIReader.List(ctx, sessions, client.InNamespace(key.Namespace),
		client.MatchingLabels{v1alpha1.LabelAgent: agent.Name}); err != nil {
		return resultError, err
	}

	now := h.now()
	gid := episodes.GroupID(ev.Key)
	facts := readSessions(sessions.Items, key.Name, gid, now)

	stored := findGroup(t.Status.Groups, gid)
	var state *episodes.GroupState
	if stored != nil {
		s := stateFromAPI(*stored)
		state = &s
	}
	d := episodes.Decide(state, episodes.Input{
		Key: ev.Key, Status: ev.Status, Name: ev.Name, Severity: ev.Severity, Fingerprints: ev.Fingerprints,
	}, now, episodeConfig(t, agent), facts.counts)

	result := string(d.Result)
	switch d.Result {
	case episodes.ResultSession:
		session, err := h.createSession(ctx, t, agent, ev, d, gid, facts.previous, now)
		if err != nil {
			// The decision already advanced the episode bookkeeping; keep it so a
			// retry of the same notification is not mistaken for a new episode.
			if saveErr := h.saveGroup(ctx, key, *d.State, now); saveErr != nil {
				err = errors.Join(err, saveErr)
			}
			if errors.Is(err, errRender) {
				log.Error(err, "rendering webhook prompt", "namespace", key.Namespace, "trigger", key.Name)
				return resultError, nil
			}
			return resultError, err
		}
		episodes.MarkSession(d.State, session, now, d.NewFingerprints)
		return result, h.saveGroup(ctx, key, *d.State, now)
	case episodes.ResultCapped:
		h.capNotice(agent, key.Name, now, d.Reason, ev, gid)
	}
	if d.State != nil && groupChanged(stored, *d.State) {
		if err := h.saveGroup(ctx, key, *d.State, now); err != nil {
			return resultError, err
		}
	}
	return result, nil
}

// errRender marks prompt template failures.
var errRender = errors.New("render prompt")

// sessionFacts are the counts and the previous summary read from the agent's sessions.
type sessionFacts struct {
	counts   episodes.Counts
	previous string
}

func readSessions(items []v1alpha1.OmpSession, trigger, gid string, now time.Time) sessionFacts {
	var f sessionFacts
	cutoff := now.Add(-dayWindow)
	var newest time.Time
	for i := range items {
		s := &items[i]
		recent := s.CreationTimestamp.After(cutoff)
		if s.Labels[v1alpha1.LabelKind] == string(v1alpha1.SessionKindWebhook) && recent {
			f.counts.AgentEventSessions24h++
			if s.Labels[v1alpha1.LabelSeverity] != episodes.SeverityCritical {
				f.counts.AgentNonCritical24h++
			}
		}
		if s.Labels[v1alpha1.LabelTrigger] != trigger || s.Labels[v1alpha1.LabelGroup] != gid {
			continue
		}
		if recent {
			f.counts.GroupSessions24h++
		}
		switch s.Status.Phase {
		case v1alpha1.SessionPhaseSucceeded:
			if f.previous == "" || s.CreationTimestamp.After(newest) {
				newest = s.CreationTimestamp.Time
				f.previous = s.Status.Summary
			}
		case v1alpha1.SessionPhaseFailed:
		default: // "", Pending, Running
			f.counts.Busy = true
		}
	}
	return f
}

func episodeConfig(t *v1alpha1.OmpTrigger, agent *v1alpha1.OmpAgent) episodes.Config {
	cfg := episodes.DefaultConfig()
	g := t.Spec.Webhook.Grouping
	if g.EpisodeGap.Duration > 0 {
		cfg.EpisodeGap = g.EpisodeGap.Duration
	}
	if g.RetriageAfter.Duration > 0 {
		cfg.RetriageAfter = g.RetriageAfter.Duration
	}
	if g.PerGroupPerDay > 0 {
		cfg.PerGroupPerDay = int(g.PerGroupPerDay)
	}
	if l := agent.Spec.Limits; l.EventSessionsPerDay > 0 {
		cfg.EventSessionsPerDay = int(l.EventSessionsPerDay)
	}
	if l := agent.Spec.Limits; l.NonCriticalSessionsPerDay > 0 {
		cfg.NonCriticalSessionsPerDay = int(l.NonCriticalSessionsPerDay)
	}
	return cfg
}

// createSession renders the prompt and creates the OmpSession, returning its name.
func (h *Handler) createSession(ctx context.Context, t *v1alpha1.OmpTrigger, agent *v1alpha1.OmpAgent, ev adapters.Event, d episodes.Decision, gid, previous string, now time.Time) (string, error) {
	continuityKey := ""
	switch t.Spec.Session.Resume {
	case v1alpha1.ResumeEpisode:
		continuityKey = fmt.Sprintf("%s/%s/%d", t.Name, gid, d.State.EpisodeStart.Unix())
	case v1alpha1.ResumeKey:
		continuityKey = fmt.Sprintf("%s/%s", t.Name, gid)
	}

	alerts := make([]prompt.Alert, len(ev.Alerts))
	for i, a := range ev.Alerts {
		alerts[i] = prompt.Alert{Labels: a.Labels, Annotations: a.Annotations, StartsAt: a.StartsAt, Fingerprint: a.Fingerprint}
	}
	alertsFence, err := prompt.AlertsFence(alerts)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errRender, err)
	}
	payloadFence, err := prompt.PayloadFence(ev.Payload)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errRender, err)
	}

	priority := DefaultWebhookPriority
	if t.Spec.Priority != nil {
		priority = *t.Spec.Priority
	}
	if ev.Severity == episodes.SeverityCritical {
		priority = CriticalPriority
	}

	for range nameCreateAttempts {
		name, err := sessionName(t.Name, now)
		if err != nil {
			return "", err
		}
		text, err := prompt.Render(t.Spec.Prompt, prompt.WebhookData{
			Agent:   agent.Name,
			Trigger: t.Name,
			Session: name,
			Event: prompt.Event{
				Name: ev.Name, Key: ev.Key, Severity: ev.Severity, Status: ev.Status, Labels: ev.Labels,
			},
			Reason:     d.Reason,
			AlertCount: len(ev.Alerts),
			Previous:   prompt.PreviousSummary(previous),
			Alerts:     alertsFence,
			Payload:    payloadFence,
		})
		if err != nil {
			return "", fmt.Errorf("%w: %w", errRender, err)
		}
		labels := map[string]string{
			v1alpha1.LabelAgent:   agent.Name,
			v1alpha1.LabelTrigger: t.Name,
			v1alpha1.LabelKind:    string(v1alpha1.SessionKindWebhook),
			v1alpha1.LabelGroup:   gid,
		}
		if sev := LabelValue(ev.Severity); sev != "" {
			labels[v1alpha1.LabelSeverity] = sev
		}
		if continuityKey != "" {
			labels[v1alpha1.LabelContinuity] = store.ContinuityID(continuityKey)
		}
		session := &v1alpha1.OmpSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: t.Namespace, Name: name, Labels: labels},
			Spec: v1alpha1.OmpSessionSpec{
				AgentRef:      v1alpha1.LocalObjectRef{Name: agent.Name},
				TriggerRef:    &v1alpha1.LocalObjectRef{Name: t.Name},
				Prompt:        text,
				Priority:      &priority,
				ContinuityKey: continuityKey,
				Event:         &v1alpha1.SessionEvent{Key: ev.Key, GroupID: gid, Name: ev.Name, Severity: ev.Severity},
			},
		}
		scheme := h.Client.Scheme()
		if err := controllerutil.SetOwnerReference(agent, session, scheme); err != nil {
			return "", err
		}
		if err := controllerutil.SetOwnerReference(t, session, scheme); err != nil {
			return "", err
		}
		err = h.Client.Create(ctx, session)
		if apierrors.IsAlreadyExists(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("no free session name for trigger %s", t.Name)
}

// sessionName is <trigger>-<YYYYMMDD-HHMMSS>-<4 hex> in UTC.
func sessionName(trigger string, now time.Time) (string, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%s", trigger, now.UTC().Format("20060102-150405"), hex.EncodeToString(b[:])), nil
}

// LabelValue makes s usable as a label value: invalid characters become '-',
// the result is cut to 63 characters and trimmed to start and end alphanumeric.
func LabelValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 63 {
		out = out[:63]
	}
	return strings.Trim(out, "-_.")
}

// capNotice tells the agent's sinks, once per day and trigger, that a session
// budget was exhausted. It runs in the background: sinks retry for up to
// 30 s and the sender must not wait for that.
func (h *Handler) capNotice(agent *v1alpha1.OmpAgent, trigger string, now time.Time, reason string, ev adapters.Event, gid string) {
	if h.Notify == nil {
		return
	}
	key := "cap:" + trigger + ":" + now.Format(time.DateOnly)
	guard := agent.Namespace + "/" + agent.Name + "/" + key
	if _, loaded := h.notices.LoadOrStore(guard, struct{}{}); loaded {
		return
	}
	a := agent.DeepCopy()
	title := fmt.Sprintf("%s: session limit reached", agent.Name)
	text := fmt.Sprintf("Trigger %s skipped a session for %q (group %s): %s. Further skipped events are not reported today.",
		trigger, ev.Name, gid, reason)
	h.wg.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := h.Notify.Notice(ctx, a, key, noticeEvery, title, text); err != nil {
			h.notices.Delete(guard) // the claim may not have been recorded; the next cap tries again
			log.Error(err, "sending cap notice", "namespace", a.Namespace, "agent", a.Name)
		}
	})
}
