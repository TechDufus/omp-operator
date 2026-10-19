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

package notify

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/threads"
)

// Dispatcher resolves an agent's notify sinks from the cluster and delivers
// session results and operator notices to them.
type Dispatcher struct {
	// Client patches OmpAgent.status.notices.
	Client client.Client
	// APIReader reads the sinks' Secrets and the fresh agent.
	APIReader client.Reader
	// Sender delivers the messages; defaults to NewSender().
	Sender *Sender
	// Options supplies DashboardURL for links.
	Options *config.Options
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dispatcher) sender() *Sender {
	if d.Sender != nil {
		return d.Sender
	}
	return NewSender()
}

func (d *Dispatcher) reader() client.Reader {
	if d.APIReader != nil {
		return d.APIReader
	}
	return d.Client
}

// link returns "<dashboard-url><path>", or "" when no dashboard URL is set.
func (d *Dispatcher) link(path string) string {
	if d.Options == nil || d.Options.DashboardURL == "" {
		return ""
	}
	return strings.TrimRight(d.Options.DashboardURL, "/") + path
}

// SessionDone notifies the agent's sinks whose `on` includes the session's result:
// its verdict, or "failed" for a Failed session. Sinks are independent: a failing
// sink never keeps the others from being tried, and every failure is returned,
// joined.
func (d *Dispatcher) SessionDone(ctx context.Context, agent *ompv1alpha1.OmpAgent, s *ompv1alpha1.OmpSession) error {
	verdict := string(s.Status.Verdict)
	if s.Status.Phase == ompv1alpha1.SessionPhaseFailed {
		verdict = string(ompv1alpha1.NotifyOnFailed)
	}
	if verdict == "" {
		return nil
	}
	sinks := slices.DeleteFunc(slices.Clone(agent.Spec.Notify), func(n ompv1alpha1.NotifySink) bool {
		return !sinkWants(n, verdict)
	})
	if len(sinks) == 0 {
		return nil
	}
	return d.deliver(ctx, agent.Namespace, sinks, d.sessionMessage(agent, s, verdict))
}

func sinkWants(n ompv1alpha1.NotifySink, verdict string) bool {
	if len(n.On) == 0 {
		return true
	}
	return slices.Contains(n.On, ompv1alpha1.NotifyOn(verdict))
}

func (d *Dispatcher) sessionMessage(agent *ompv1alpha1.OmpAgent, s *ompv1alpha1.OmpSession, verdict string) Message {
	m := Message{
		Session:          s.Namespace + "/" + s.Name,
		Agent:            agent.Name,
		Verdict:          verdict,
		Title:            s.Status.Title,
		Summary:          s.Status.Summary,
		ProposedCommands: slices.Clone(s.Status.ProposedCommands),
		URL:              d.link("/sessions/" + s.Namespace + "/" + s.Name),
	}
	if s.Spec.TriggerRef != nil {
		m.Trigger = s.Spec.TriggerRef.Name
	}
	if m.Title == "" {
		m.Title = fmt.Sprintf("%s: session %s %s", agent.Name, s.Name, strings.ReplaceAll(verdict, "_", " "))
	}
	if m.Summary == "" && s.Status.Error != "" {
		m.Summary = s.Status.Error
	}
	for _, a := range s.Status.Actions {
		m.Actions = append(m.Actions, ActionRecord{Action: a.Action, Target: a.Target, OK: a.OK, At: a.At.Time, Result: a.Result})
	}
	return m
}

// maxTurnSummaryChars bounds the reply excerpt a turn notification carries.
const maxTurnSummaryChars = 800

// TurnDone notifies the agent's sinks whose `on` includes the turn's result about a finished
// thread turn. The result is "failed" for a failed turn, "fixed" when the turn changed files
// and "ok" otherwise. Sinks are independent: a failing sink never keeps the others from being
// tried, and every failure is returned, joined.
func (d *Dispatcher) TurnDone(ctx context.Context, agent *ompv1alpha1.OmpAgent, thread *ompv1alpha1.OmpThread, turn threads.Turn) error {
	verdict := string(ompv1alpha1.NotifyOnOK)
	switch {
	case turn.State == ompv1alpha1.TurnStateFailed:
		verdict = string(ompv1alpha1.NotifyOnFailed)
	case turn.FilesChanged > 0:
		verdict = string(ompv1alpha1.NotifyOnFixed)
	}
	sinks := slices.DeleteFunc(slices.Clone(agent.Spec.Notify), func(n ompv1alpha1.NotifySink) bool {
		return !sinkWants(n, verdict)
	})
	if len(sinks) == 0 {
		return nil
	}
	return d.deliver(ctx, agent.Namespace, sinks, d.turnMessage(agent, thread, turn, verdict))
}

func (d *Dispatcher) turnMessage(agent *ompv1alpha1.OmpAgent, thread *ompv1alpha1.OmpThread, turn threads.Turn, verdict string) Message {
	what := turn.Title
	if what == "" {
		what = fmt.Sprintf("turn %d", turn.ID)
	}
	summary := turn.Reply
	if turn.State == ompv1alpha1.TurnStateFailed || summary == "" {
		summary = turn.Error
	}
	if r := []rune(summary); len(r) > maxTurnSummaryChars {
		summary = string(r[:maxTurnSummaryChars])
	}
	return Message{
		Session: thread.Namespace + "/" + thread.Name,
		Agent:   agent.Name,
		Verdict: verdict,
		Title:   thread.Spec.Title + ": " + what,
		Summary: summary,
		URL:     d.link("/threads/" + thread.Namespace + "/" + thread.Name),
	}
}

// Notice sends a notice to every sink of the agent at most once per `every` for
// each key, recording the time in agent.status.notices[key]. The record is
// claimed with an optimistic-lock patch before anything is sent, so concurrent
// callers send at most one notice between them. It returns sent=false when the
// notice was suppressed or the agent has no sinks. A claimed notice stays
// claimed even when every sink fails; the sink errors are returned, joined.
func (d *Dispatcher) Notice(ctx context.Context, agent *ompv1alpha1.OmpAgent, key string, every time.Duration, title, text string) (sent bool, err error) {
	var sinks []ompv1alpha1.NotifySink
	claimed := false
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur ompv1alpha1.OmpAgent
		if err := d.reader().Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: agent.Name}, &cur); err != nil {
			return err
		}
		sinks = cur.Spec.Notify
		if len(sinks) == 0 {
			return nil
		}
		now := d.now()
		if last, ok := cur.Status.Notices[key]; ok && now.Sub(last.Time) < every {
			return nil
		}
		orig := cur.DeepCopy()
		if cur.Status.Notices == nil {
			cur.Status.Notices = map[string]metav1.Time{}
		}
		cur.Status.Notices[key] = metav1.NewTime(now)
		if err := d.Client.Status().Patch(ctx, &cur, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		claimed = true
		agent.Status.Notices = maps.Clone(cur.Status.Notices)
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("record notice %q: %w", key, err)
	}
	if !claimed {
		return false, nil
	}
	msg := Message{Agent: agent.Name, Title: title, Summary: text, URL: d.link("/agents/" + agent.Namespace + "/" + agent.Name)}
	return true, d.deliver(ctx, agent.Namespace, sinks, msg)
}

// deliver sends msg to every sink, each independently, and joins the errors.
func (d *Dispatcher) deliver(ctx context.Context, namespace string, sinks []ompv1alpha1.NotifySink, msg Message) error {
	sender := d.sender()
	var errs []error
	for i, n := range sinks {
		sink, err := d.resolve(ctx, namespace, n)
		if err == nil {
			err = sender.Send(ctx, sink, msg)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("notify[%d] %s: %w", i, n.Type, err))
		}
	}
	return errors.Join(errs...)
}

// resolve builds the sink for n, reading its Secrets from namespace. Errors name
// the Secret and key, never a value.
func (d *Dispatcher) resolve(ctx context.Context, namespace string, n ompv1alpha1.NotifySink) (Sink, error) {
	switch n.Type {
	case ompv1alpha1.SinkNtfy:
		if n.Ntfy == nil {
			return nil, errors.New("ntfy sink has no ntfy settings")
		}
		topic, err := d.secretValue(ctx, namespace, n.Ntfy.TopicSecretRef)
		if err != nil {
			return nil, err
		}
		return NewNtfy(n.Ntfy.URL, strings.TrimSpace(string(topic))), nil
	case ompv1alpha1.SinkSlack:
		if n.Slack == nil {
			return nil, errors.New("slack sink has no slack settings")
		}
		u, err := d.secretValue(ctx, namespace, n.Slack.WebhookURLSecretRef)
		if err != nil {
			return nil, err
		}
		return NewSlack(strings.TrimSpace(string(u))), nil
	case ompv1alpha1.SinkDiscord:
		if n.Discord == nil {
			return nil, errors.New("discord sink has no discord settings")
		}
		u, err := d.secretValue(ctx, namespace, n.Discord.WebhookURLSecretRef)
		if err != nil {
			return nil, err
		}
		return NewDiscord(strings.TrimSpace(string(u))), nil
	case ompv1alpha1.SinkWebhook:
		if n.Webhook == nil {
			return nil, errors.New("webhook sink has no webhook settings")
		}
		u, err := d.secretValue(ctx, namespace, n.Webhook.URLSecretRef)
		if err != nil {
			return nil, err
		}
		var key []byte
		if n.Webhook.HMACSecretRef != nil {
			if key, err = d.secretValue(ctx, namespace, *n.Webhook.HMACSecretRef); err != nil {
				return nil, err
			}
		}
		return NewWebhook(strings.TrimSpace(string(u)), key), nil
	}
	return nil, fmt.Errorf("unknown sink type %q", n.Type)
}

func (d *Dispatcher) secretValue(ctx context.Context, namespace string, ref ompv1alpha1.SecretKeyRef) ([]byte, error) {
	var s corev1.Secret
	if err := d.reader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &s); err != nil {
		return nil, fmt.Errorf("read secret %q: %w", ref.Name, err)
	}
	v, ok := s.Data[ref.Key]
	if !ok || len(v) == 0 {
		return nil, fmt.Errorf("secret %q has no value for key %q", ref.Name, ref.Key)
	}
	return v, nil
}
