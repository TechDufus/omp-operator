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

package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/metrics"
	"github.com/TechDufus/omp-operator/internal/store"
)

// ErrUnknownTool is returned by Call for a tool the agent does not have; the API answers 404.
var ErrUnknownTool = agentapi.ErrUnknownTool

const (
	// maxActionRecords is the most entries kept in OmpSession.status.actions.
	maxActionRecords = 20
	// maxRecordResult is the longest result text stored in a status.actions entry.
	maxRecordResult = 300
	maxRecordTarget = 200
	maxEventText    = 500

	defaultPollInterval = 5 * time.Second
	defaultSessionLimit = 30 * time.Minute
	httpTimeout         = 30 * time.Second
)

// Runner executes agent tool calls and describes the tools an agent has.
type Runner struct {
	// Client writes: patches of action targets and of session status.
	Client client.Client
	// APIReader reads uncached: targets, sessions, OmpActions and Secrets.
	APIReader client.Reader
	Store     *store.Store
	Options   *config.Options
	// HTTP is the client used for alertmanagerSilence and httpRequest. Optional; its
	// timeout is capped at 30 s and redirects are never followed.
	HTTP *http.Client
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
	// PollInterval is the rollout and reconcile polling period; defaults to 5 s.
	PollInterval time.Duration
}

var _ agentapi.ActionRunner = (*Runner)(nil)

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) pollInterval() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return defaultPollInterval
}

// errNotUsable marks a listed action that is missing, not Ready or invalid.
var errNotUsable = errors.New("action not usable")

// loadAction reads an OmpAction of the agent's namespace and returns it only when it is Ready
// and still valid.
func (r *Runner) loadAction(ctx context.Context, namespace, name string) (*v1alpha1.OmpAction, error) {
	var a v1alpha1.OmpAction
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &a); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errNotUsable
		}
		return nil, err
	}
	if !meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionReady) {
		return nil, errNotUsable
	}
	if _, ok := ValidateName(a.Name); !ok {
		return nil, errNotUsable
	}
	if ValidateSpec(&a.Spec) != nil {
		return nil, errNotUsable
	}
	return &a, nil
}

// ToolDefs returns one tool per Ready OmpAction listed by the agent, in order, followed by
// finish, and the OmpActions behind them.
func (r *Runner) ToolDefs(ctx context.Context, agent *v1alpha1.OmpAgent) ([]agentapi.ToolDef, []v1alpha1.OmpAction, error) {
	var (
		defs []agentapi.ToolDef
		acts []v1alpha1.OmpAction
		seen = map[string]bool{}
	)
	for _, ref := range agent.Spec.Actions {
		a, err := r.loadAction(ctx, agent.Namespace, ref.Name)
		if errors.Is(err, errNotUsable) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		tool := ToolName(a.Name)
		if seen[tool] {
			continue
		}
		seen[tool] = true
		params, err := parametersFor(&a.Spec)
		if err != nil {
			return nil, nil, fmt.Errorf("action %s: %w", a.Name, err)
		}
		defs = append(defs, agentapi.ToolDef{Name: tool, Description: a.Spec.Description, Parameters: params})
		acts = append(acts, *a)
	}
	defs = append(defs, finishDef())
	return defs, acts, nil
}

// outcome is the result of running one tool.
type outcome struct {
	// target is what the call acted on, for status.actions and the cooldown.
	target string
	text   string
	isErr  bool
	// result is the metric label: ok, refused (policy or validation) or error (failed while running).
	result string
}

func okOutcome(target, format string, a ...any) outcome {
	return outcome{target: target, text: fmt.Sprintf(format, a...), result: "ok"}
}

func refusal(target, format string, a ...any) outcome {
	return outcome{target: target, text: "refused: " + fmt.Sprintf(format, a...), isErr: true, result: "refused"}
}

func failure(target, format string, a ...any) outcome {
	return outcome{target: target, text: fmt.Sprintf(format, a...), isErr: true, result: "error"}
}

// callCtx is what a running action needs to know about its call.
type callCtx struct {
	agent   *v1alpha1.OmpAgent
	session *v1alpha1.OmpSession
	action  *v1alpha1.OmpAction
	tool    string
}

// Call runs one host tool call for a Running session. An unknown tool is ErrUnknownTool.
// Refusals and failures are results with IsError (HTTP 200 at the API). Every executed call
// is limited by maxPerSession, recorded in status.actions and events.jsonl and counted in
// omp_operator_action_calls_total.
func (r *Runner) Call(ctx context.Context, agent *v1alpha1.OmpAgent, s *v1alpha1.OmpSession, tool string, args json.RawMessage) (agentapi.ActionResult, error) {
	if tool == FinishTool {
		if s.Spec.ThreadRef != nil {
			return agentapi.ActionResult{Text: "finish is not available in cloud threads", IsError: true}, nil
		}
		return r.finish(ctx, agent, s, args)
	}
	act, err := r.findAction(ctx, agent, tool)
	if err != nil {
		return agentapi.ActionResult{}, err
	}
	var cur v1alpha1.OmpSession
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(s), &cur); err != nil {
		return agentapi.ActionResult{}, err
	}
	calls := 0
	for _, rec := range cur.Status.Actions {
		if rec.Action == tool {
			calls++
		}
	}
	if calls >= maxPerSession(&act.Spec) {
		out := refusal("", "call limit reached: %s may be called %d times per session", tool, maxPerSession(&act.Spec))
		r.observe(ctx, &cur, tool, out)
		return agentapi.ActionResult{Text: out.text, IsError: true}, nil
	}
	if len(cur.Status.Actions) >= maxActionRecords {
		out := refusal("", "this session has already made %d action calls", maxActionRecords)
		r.observe(ctx, &cur, tool, out)
		return agentapi.ActionResult{Text: out.text, IsError: true}, nil
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	c := callCtx{agent: agent, session: &cur, action: act, tool: tool}
	var out outcome
	switch {
	case act.Spec.KubernetesRolloutRestart != nil:
		out = r.restart(ctx, c, act.Spec.KubernetesRolloutRestart, args)
	case act.Spec.FluxReconcile != nil:
		out = r.fluxReconcile(ctx, c, act.Spec.FluxReconcile, args)
	case act.Spec.AlertmanagerSilence != nil:
		out = r.silence(ctx, c, act.Spec.AlertmanagerSilence, args)
	default:
		out = r.httpRequest(ctx, c, act.Spec.HTTPRequest, args)
	}
	r.observe(ctx, &cur, tool, out)
	if err := r.recordAction(ctx, &cur, tool, out); err != nil {
		logf.FromContext(ctx).Error(err, "recording action call", "session", client.ObjectKeyFromObject(&cur), "tool", tool)
	}
	return agentapi.ActionResult{Text: out.text, IsError: out.isErr}, nil
}

// findAction maps a tool name to the agent's Ready OmpAction.
func (r *Runner) findAction(ctx context.Context, agent *v1alpha1.OmpAgent, tool string) (*v1alpha1.OmpAction, error) {
	for _, ref := range agent.Spec.Actions {
		if ToolName(ref.Name) != tool {
			continue
		}
		a, err := r.loadAction(ctx, agent.Namespace, ref.Name)
		if errors.Is(err, errNotUsable) {
			break
		}
		return a, err
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownTool, tool)
}

// observe counts the call and appends it to the session's events.
func (r *Runner) observe(ctx context.Context, s *v1alpha1.OmpSession, tool string, out outcome) {
	metrics.ActionCallsTotal.WithLabelValues(s.Namespace, s.Spec.AgentRef.Name, tool, out.result).Inc()
	if r.Store == nil {
		return
	}
	text := tool
	if out.target != "" {
		text += " " + out.target
	}
	text += ": " + oneLine(out.text)
	ev := store.Event{At: r.now().UTC(), Kind: "action", Text: truncate(text, maxEventText)}
	if err := r.Store.AppendEvents(string(s.UID), []store.Event{ev}); err != nil {
		logf.FromContext(ctx).Error(err, "appending action event", "session", client.ObjectKeyFromObject(s))
	}
}

// recordAction appends the call to status.actions.
func (r *Runner) recordAction(ctx context.Context, s *v1alpha1.OmpSession, tool string, out outcome) error {
	rec := v1alpha1.ActionRecord{
		Action: tool,
		Target: truncate(out.target, maxRecordTarget),
		OK:     !out.isErr,
		At:     metav1.NewTime(r.now()),
		Result: truncate(out.text, maxRecordResult),
	}
	return r.patchSessionStatus(ctx, client.ObjectKeyFromObject(s), func(cur *v1alpha1.OmpSession) error {
		cur.Status.Actions = append(cur.Status.Actions, rec)
		if n := len(cur.Status.Actions); n > maxActionRecords {
			cur.Status.Actions = slices.Clone(cur.Status.Actions[n-maxActionRecords:])
		}
		return nil
	})
}

// patchSessionStatus reads the session fresh, applies mutate and patches the status with
// an optimistic lock, retrying on conflict. An error from mutate aborts without a write.
func (r *Runner) patchSessionStatus(ctx context.Context, key client.ObjectKey, mutate func(*v1alpha1.OmpSession) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1alpha1.OmpSession
		if err := r.APIReader.Get(ctx, key, &cur); err != nil {
			return err
		}
		base := cur.DeepCopy()
		if err := mutate(&cur); err != nil {
			return err
		}
		return r.Client.Status().Patch(ctx, &cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// secretValue reads one Secret key in namespace.
func (r *Runner) secretValue(ctx context.Context, namespace string, ref v1alpha1.SecretKeyRef) (string, error) {
	var sec corev1.Secret
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &sec); err != nil {
		return "", fmt.Errorf("secret %s: %w", ref.Name, err)
	}
	v, ok := sec.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("secret %s has no key %q", ref.Name, ref.Key)
	}
	return string(v), nil
}

// waitContext bounds a wait by waitTimeout and, when it is still ahead, the session's
// deadline (startTime plus the agent's session timeout).
func (r *Runner) waitContext(ctx context.Context, c callCtx, waitTimeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(waitTimeout)
	if st := c.session.Status.StartTime; st != nil {
		limit := durationOr(c.agent.Spec.Limits.SessionTimeout, defaultSessionLimit)
		if end := st.Add(limit); end.After(time.Now()) && end.Before(deadline) {
			deadline = end
		}
	}
	return context.WithDeadline(ctx, deadline)
}

// pollUntil calls check every poll interval until it reports done or ctx ends. It reports
// whether ctx ended first; a check error aborts.
func (r *Runner) pollUntil(ctx context.Context, check func(context.Context) (bool, error)) (timedOut bool, err error) {
	t := time.NewTicker(r.pollInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return true, nil
		case <-t.C:
		}
		done, err := check(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return true, nil
			}
			return false, err
		}
		if done {
			return false, nil
		}
	}
}

// httpClient returns a client that times out after 30 s and never follows redirects.
func (r *Runner) httpClient() *http.Client {
	var c http.Client
	if r.HTTP != nil {
		c = *r.HTTP
	}
	if c.Timeout <= 0 || c.Timeout > httpTimeout {
		c.Timeout = httpTimeout
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// decodeArgs strictly decodes tool arguments: unknown fields are errors.
func decodeArgs(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if dec.More() {
		return errors.New("invalid arguments: trailing data")
	}
	return nil
}

// checkText validates a required text argument of 1..max characters.
func checkText(field, v string, limit int) error {
	n := utf8.RuneCountInString(strings.TrimSpace(v))
	if n < 1 || utf8.RuneCountInString(v) > limit {
		return fmt.Errorf("%s must be 1-%d characters", field, limit)
	}
	return nil
}

// truncate cuts s to at most n characters, marking the cut with an ellipsis.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	rs := []rune(s)
	return string(rs[:n-1]) + "…"
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
