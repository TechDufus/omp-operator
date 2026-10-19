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
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/gateway"
	"github.com/TechDufus/omp-operator/internal/metrics"
	"github.com/TechDufus/omp-operator/internal/podspec"
	"github.com/TechDufus/omp-operator/internal/store"
)

// Default timings of OmpSessionReconciler; each is overridable by a reconciler field.
const (
	defaultNotReadyLimit    = 30 * time.Minute
	defaultPodStartLimit    = 10 * time.Minute
	defaultWorkspaceLimit   = 30 * time.Minute
	defaultNotReadyRequeue  = 60 * time.Second
	defaultQueueRequeue     = 10 * time.Second
	defaultToolchainRequeue = 10 * time.Second
	defaultGatewayHold      = 2 * time.Minute
	defaultToolchainRetry   = 6 * time.Hour
	defaultNoticeEvery      = 6 * time.Hour
	defaultRetentionRecheck = time.Hour

	noticeAuth      = "auth"
	noticeWorkspace = "workspace"
	noticeToolchain = "toolchain"

	maxErrorLen = 1000
)

var (
	// errStale aborts a status transition whose precondition no longer holds on the fresh object.
	errStale = errors.New("session changed concurrently")
	// errNoChange makes a status update a no-op.
	errNoChange = errors.New("no change")
)

// SessionNotifier sends session results and rate-limited notices to an agent's sinks.
// *notify.Dispatcher satisfies it.
type SessionNotifier interface {
	SessionDone(ctx context.Context, agent *ompv1alpha1.OmpAgent, s *ompv1alpha1.OmpSession) error
	Notice(ctx context.Context, agent *ompv1alpha1.OmpAgent, key string, every time.Duration, title, text string) (sent bool, err error)
}

// ClientsWriteClock reports when the gateway clients Secret was last written by this
// process. *gateway.Manager satisfies it.
type ClientsWriteClock interface {
	LastClientsWrite() time.Time
}

// OmpSessionReconciler runs OmpSessions: it gates them one at a time per agent, starts
// session and toolsync pods, and turns how the pod ended into a terminal phase.
//
// Status ownership: this reconciler owns phase, attempts, retryAt, podName, startTime,
// completionTime and the conditions; the agent API owns outcome, verdict, title, summary,
// proposedCommands, actions, usage, error, workspaceRevision and continuity.
type OmpSessionReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads uncached: sessions, agents, pods, Secrets and Flux sources.
	APIReader client.Reader
	Options   *config.Options
	Store     *store.Store
	// Notifier delivers results and notices; nil disables notifications.
	Notifier SessionNotifier
	// Gateway gates sessions of managed-gateway agents on the clients Secret write age; nil
	// disables the gate.
	Gateway ClientsWriteClock
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	// Timings; zero means the default named in parentheses.
	NotReadyLimit    time.Duration // agent not Ready before the session fails (30m)
	PodStartLimit    time.Duration // pod not Running before the attempt is retryable (10m)
	WorkspaceLimit   time.Duration // Flux artifact missing before the session fails (30m)
	NotReadyRequeue  time.Duration // wait for a Ready agent or Flux artifact (60s)
	QueueRequeue     time.Duration // wait for the agent to be free or this session's turn (10s)
	ToolchainRequeue time.Duration // wait for a toolsync pod (10s)
	GatewayHold      time.Duration // minimum age of the clients Secret write (2m)
	ToolchainRetry   time.Duration // minimum gap between toolsync attempts (6h)
	NoticeEvery      time.Duration // minimum gap between equal notices (6h)
	RetentionRecheck time.Duration // longest wait between retention checks (1h)
}

// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompsessions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompsessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompsessions/finalizers,verbs=update

func (r *OmpSessionReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func durOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (r *OmpSessionReconciler) scheme() *runtime.Scheme {
	if r.Scheme != nil {
		return r.Scheme
	}
	return r.Client.Scheme()
}

// Reconcile advances one session by at most one lifecycle step.
func (r *OmpSessionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	s := &ompv1alpha1.OmpSession{}
	if err := r.APIReader.Get(ctx, req.NamespacedName, s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !r.Options.NamespaceAllowed(s.Namespace) {
		return ctrl.Result{}, nil
	}
	defer r.updateActiveGauge(ctx, s.Namespace, s.Spec.AgentRef.Name)

	if !s.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, s)
	}
	if err := r.ensureMetadata(ctx, s); err != nil {
		return ctrl.Result{}, err
	}
	if s.Status.Phase == "" {
		err := r.updateStatus(ctx, s, func(cur *ompv1alpha1.OmpSession) error {
			if cur.Status.Phase != "" {
				return errNoChange
			}
			cur.Status.Phase = ompv1alpha1.SessionPhasePending
			return nil
		})
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	switch s.Status.Phase {
	case ompv1alpha1.SessionPhaseSucceeded, ompv1alpha1.SessionPhaseFailed:
		res, err = r.reconcileTerminal(ctx, s)
	case ompv1alpha1.SessionPhaseRunning:
		res, err = r.reconcileRunning(ctx, s)
	default:
		res, err = r.reconcilePending(ctx, s)
	}
	if errors.Is(err, errStale) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return res, err
}

// ---- metadata, finalizer ----

// ensureMetadata adds the finalizer, the informational labels and the owner references a
// creator may have left out (manual sessions are created by users).
func (r *OmpSessionReconciler) ensureMetadata(ctx context.Context, s *ompv1alpha1.OmpSession) error {
	base := s.DeepCopy()
	controllerutil.AddFinalizer(s, ompv1alpha1.FinalizerCleanup)

	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	setLabel := func(k, v string) {
		if v == "" || validation.IsValidLabelValue(v) != nil {
			return
		}
		if _, ok := s.Labels[k]; !ok {
			s.Labels[k] = v
		}
	}
	kind := ompv1alpha1.SessionKindManual
	switch {
	case s.Spec.ThreadRef != nil:
		kind = ompv1alpha1.SessionKindCloud
	case s.Spec.Event != nil:
		kind = ompv1alpha1.SessionKindWebhook
	case s.Spec.TriggerRef != nil:
		kind = ompv1alpha1.SessionKindSchedule
	}
	setLabel(ompv1alpha1.LabelAgent, s.Spec.AgentRef.Name)
	setLabel(ompv1alpha1.LabelKind, string(kind))
	if s.Spec.ThreadRef != nil {
		setLabel(ompv1alpha1.LabelThread, s.Spec.ThreadRef.Name)
	}
	if s.Spec.TriggerRef != nil {
		setLabel(ompv1alpha1.LabelTrigger, s.Spec.TriggerRef.Name)
	}
	if ev := s.Spec.Event; ev != nil {
		setLabel(ompv1alpha1.LabelGroup, ev.GroupID)
		setLabel(ompv1alpha1.LabelSeverity, ev.Severity)
	}
	if s.Spec.ContinuityKey != "" {
		setLabel(ompv1alpha1.LabelContinuity, store.ContinuityID(s.Spec.ContinuityKey))
	}

	owner := func(o client.Object) error {
		if slices.ContainsFunc(s.OwnerReferences, func(ref metav1.OwnerReference) bool { return ref.UID == o.GetUID() }) {
			return nil
		}
		return controllerutil.SetOwnerReference(o, s, r.scheme())
	}
	agent := &ompv1alpha1.OmpAgent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Spec.AgentRef.Name}, agent); err == nil {
		if err := owner(agent); err != nil {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if t := s.Spec.ThreadRef; t != nil {
		thread := &ompv1alpha1.OmpThread{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: t.Name}, thread); err == nil {
			// The thread reconciler's sittings already carry it as their controller.
			if err := owner(thread); err != nil {
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	if t := s.Spec.TriggerRef; t != nil {
		trig := &ompv1alpha1.OmpTrigger{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: t.Name}, trig); err == nil {
			if err := owner(trig); err != nil {
				return err
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}

	if equalMeta(base, s) {
		return nil
	}
	return r.Patch(ctx, s, client.MergeFrom(base))
}

func equalMeta(a, b *ompv1alpha1.OmpSession) bool {
	return slices.Equal(a.Finalizers, b.Finalizers) &&
		len(a.Labels) == len(b.Labels) &&
		len(a.OwnerReferences) == len(b.OwnerReferences)
}

// finalize removes the session's stored files, and the continuity tarball when no other
// session of the agent holds the same continuity label. The sittings of a thread share the
// thread's tarball, which the thread's own finalizer removes.
func (r *OmpSessionReconciler) finalize(ctx context.Context, s *ompv1alpha1.OmpSession) error {
	if !controllerutil.ContainsFinalizer(s, ompv1alpha1.FinalizerCleanup) {
		return nil
	}
	if r.Store != nil {
		if err := r.Store.RemoveSession(string(s.UID)); err != nil {
			return fmt.Errorf("remove session data: %w", err)
		}
		if id := s.Labels[ompv1alpha1.LabelContinuity]; id != "" && s.Spec.ThreadRef == nil {
			shared, err := r.continuityShared(ctx, s, id)
			if err != nil {
				return err
			}
			if !shared {
				if err := r.Store.RemoveContinuity(s.Namespace, s.Spec.AgentRef.Name, id); err != nil {
					return fmt.Errorf("remove continuity: %w", err)
				}
			}
		}
	}
	base := s.DeepCopy()
	controllerutil.RemoveFinalizer(s, ompv1alpha1.FinalizerCleanup)
	if err := r.Patch(ctx, s, client.MergeFrom(base)); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

func (r *OmpSessionReconciler) continuityShared(ctx context.Context, s *ompv1alpha1.OmpSession, id string) (bool, error) {
	var list ompv1alpha1.OmpSessionList
	if err := r.APIReader.List(ctx, &list, client.InNamespace(s.Namespace),
		client.MatchingLabels{ompv1alpha1.LabelContinuity: id}); err != nil {
		return false, err
	}
	return slices.ContainsFunc(list.Items, func(o ompv1alpha1.OmpSession) bool {
		return o.UID != s.UID && o.Spec.AgentRef.Name == s.Spec.AgentRef.Name && o.DeletionTimestamp.IsZero()
	}), nil
}

// ---- terminal ----

// reconcileTerminal notifies a finished session once and deletes it when its retention is over.
func (r *OmpSessionReconciler) reconcileTerminal(ctx context.Context, s *ompv1alpha1.OmpSession) (ctrl.Result, error) {
	agent, err := r.getAgent(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	if s.Status.CompletionTime == nil {
		now := metav1.NewTime(r.now())
		if err := r.updateStatus(ctx, s, func(cur *ompv1alpha1.OmpSession) error {
			if cur.Status.CompletionTime != nil {
				return errNoChange
			}
			cur.Status.CompletionTime = &now
			return nil
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	if meta.FindStatusCondition(s.Status.Conditions, ompv1alpha1.ConditionNotified) == nil {
		if err := r.notifyDone(ctx, s, agent); err != nil {
			return ctrl.Result{}, err
		}
	}

	retention := podspec.DefaultSessionRetention
	if agent != nil {
		retention = podspec.SessionRetention(agent)
	}
	remaining := s.Status.CompletionTime.Add(retention).Sub(r.now())
	if remaining <= 0 {
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, s))
	}
	return ctrl.Result{RequeueAfter: min(remaining, durOr(r.RetentionRecheck, defaultRetentionRecheck))}, nil
}

// notifyDone sends the result to the agent's sinks and records the Notified condition. A
// delivery failure is recorded, not retried, so sinks that succeeded are not told twice.
func (r *OmpSessionReconciler) notifyDone(ctx context.Context, s *ompv1alpha1.OmpSession, agent *ompv1alpha1.OmpAgent) error {
	cond := metav1.Condition{Type: ompv1alpha1.ConditionNotified, Status: metav1.ConditionTrue, Reason: "Notified"}
	switch {
	case s.Spec.ThreadRef != nil:
		// The sittings of a thread report per turn, through the thread reconciler.
		cond.Reason, cond.Message = "ThreadTurns", "turns are notified by the thread"
	case agent == nil:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "AgentNotFound", "the agent no longer exists"
	case r.Notifier == nil:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "NoNotifier", "notifications are not configured"
	default:
		if err := r.Notifier.SessionDone(ctx, agent, s); err != nil {
			logf.FromContext(ctx).Error(err, "notify session result", "session", client.ObjectKeyFromObject(s))
			cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "NotifyFailed", truncate(err.Error(), 500)
		}
	}
	return r.setCondition(ctx, s, cond)
}

// finish moves the session to a terminal phase, records metrics and notifies the sinks.
// message is the error text for Failed: with override it replaces any error the agent
// reported, otherwise it only fills in for an empty one.
func (r *OmpSessionReconciler) finish(ctx context.Context, s *ompv1alpha1.OmpSession, agent *ompv1alpha1.OmpAgent,
	phase ompv1alpha1.SessionPhase, message string, override bool) (ctrl.Result, error) {
	now := metav1.NewTime(r.now())
	err := r.updateStatus(ctx, s, func(cur *ompv1alpha1.OmpSession) error {
		if cur.Status.Phase == ompv1alpha1.SessionPhaseSucceeded || cur.Status.Phase == ompv1alpha1.SessionPhaseFailed {
			return errStale
		}
		cur.Status.Phase = phase
		cur.Status.CompletionTime = &now
		cur.Status.RetryAt = nil
		if message != "" && (override || cur.Status.Error == "") {
			cur.Status.Error = truncate(message, maxErrorLen)
		}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	verdict := string(s.Status.Verdict)
	if phase == ompv1alpha1.SessionPhaseFailed {
		verdict = "failed"
	} else if verdict == "" {
		verdict = string(ompv1alpha1.NotifyOnOK) // a thread sitting has no verdict
	}
	kind := s.Labels[ompv1alpha1.LabelKind]
	trigger := ""
	if s.Spec.TriggerRef != nil {
		trigger = s.Spec.TriggerRef.Name
	}
	metrics.SessionsTotal.WithLabelValues(s.Namespace, s.Spec.AgentRef.Name, trigger, kind, verdict).Inc()
	if st := s.Status.StartTime; st != nil {
		metrics.SessionDuration.WithLabelValues(s.Namespace, s.Spec.AgentRef.Name).Observe(now.Sub(st.Time).Seconds())
	}

	if err := r.notifyDone(ctx, s, agent); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// ---- pending ----

func (r *OmpSessionReconciler) reconcilePending(ctx context.Context, s *ompv1alpha1.OmpSession) (ctrl.Result, error) {
	agent, err := r.getAgent(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	if agent == nil {
		return r.finish(ctx, s, nil, ompv1alpha1.SessionPhaseFailed, "agent not found", true)
	}
	now := r.now()

	// A start that created the pod but failed to record it: adopt the pod.
	if adopted, err := r.adoptStartedPod(ctx, s); err != nil || adopted {
		return ctrl.Result{}, err
	}

	// 1. retryAt
	if ra := s.Status.RetryAt; ra != nil && ra.After(now) {
		return ctrl.Result{RequeueAfter: ra.Sub(now)}, nil
	}

	// 2. agent Ready
	if ready := meta.FindStatusCondition(agent.Status.Conditions, ompv1alpha1.ConditionReady); ready == nil || ready.Status != metav1.ConditionTrue {
		reason := "no Ready condition yet"
		if ready != nil {
			reason = ready.Reason
		}
		if now.Sub(pendingSince(s)) > durOr(r.NotReadyLimit, defaultNotReadyLimit) {
			return r.finish(ctx, s, agent, ompv1alpha1.SessionPhaseFailed, "agent not ready: "+reason, true)
		}
		return ctrl.Result{RequeueAfter: durOr(r.NotReadyRequeue, defaultNotReadyRequeue)}, nil
	}

	// 3. managed gateway token propagation
	if r.Gateway != nil && gateway.Managed(agent, r.Options) {
		if w := r.Gateway.LastClientsWrite(); !w.IsZero() {
			if wait := durOr(r.GatewayHold, defaultGatewayHold) - now.Sub(w); wait > 0 {
				return ctrl.Result{RequeueAfter: wait}, nil
			}
		}
	}

	// 4. the agent's concurrency limit, in queue order; a toolsync pod and the thread's own
	// running sitting hold the session back
	sessions, pods, err := r.agentWork(ctx, s.Namespace, agent.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if slices.ContainsFunc(pods, func(p corev1.Pod) bool {
		return p.Labels[ompv1alpha1.LabelRole] == string(ompv1alpha1.PodRoleToolsync) && !podFinished(&p)
	}) || slices.ContainsFunc(sessions, func(o ompv1alpha1.OmpSession) bool {
		return o.UID != s.UID && o.Status.Phase == ompv1alpha1.SessionPhaseRunning && sameThread(&o, s)
	}) {
		return ctrl.Result{RequeueAfter: durOr(r.QueueRequeue, defaultQueueRequeue)}, nil
	}
	if !r.mayStart(s, agent, sessions, now) {
		return ctrl.Result{RequeueAfter: durOr(r.QueueRequeue, defaultQueueRequeue)}, nil
	}

	// 5. workspace and skill sources
	if res, done, err := r.workspaceGate(ctx, s, agent); done || err != nil {
		return res, err
	}

	// 6. toolchain (a thread sitting caches its repository's toolchain instead)
	if s.Spec.ThreadRef == nil {
		if res, done, err := r.toolchainGate(ctx, agent, pods); done || err != nil {
			return res, err
		}
	}

	return r.start(ctx, s, agent)
}

// pendingSince is when the session last became eligible to wait: its creation, or its retry time.
func pendingSince(s *ompv1alpha1.OmpSession) time.Time {
	t := s.CreationTimestamp.Time
	if ra := s.Status.RetryAt; ra != nil && ra.After(t) {
		t = ra.Time
	}
	return t
}

// agentWork lists the agent's sessions (by spec, so manual sessions that were never labelled
// count) and its operator-created pods, uncached.
func (r *OmpSessionReconciler) agentWork(ctx context.Context, ns, agent string) ([]ompv1alpha1.OmpSession, []corev1.Pod, error) {
	var sl ompv1alpha1.OmpSessionList
	if err := r.APIReader.List(ctx, &sl, client.InNamespace(ns)); err != nil {
		return nil, nil, err
	}
	sessions := slices.DeleteFunc(sl.Items, func(o ompv1alpha1.OmpSession) bool { return o.Spec.AgentRef.Name != agent })
	var pl corev1.PodList
	if err := r.APIReader.List(ctx, &pl, client.InNamespace(ns), client.MatchingLabels{
		ompv1alpha1.LabelAgent:     agent,
		ompv1alpha1.LabelManagedBy: ompv1alpha1.ManagedByValue,
	}); err != nil {
		return nil, nil, err
	}
	return sessions, pl.Items, nil
}

func sessionPriority(s *ompv1alpha1.OmpSession) int32 {
	if s.Spec.Priority != nil {
		return *s.Spec.Priority
	}
	return 1
}

// maxConcurrent is how many of the agent's sessions may run at once.
func maxConcurrent(agent *ompv1alpha1.OmpAgent) int {
	return max(1, int(agent.Spec.Limits.MaxConcurrentSessions))
}

// sameThread reports whether a and b are sittings of one thread.
func sameThread(a, b *ompv1alpha1.OmpSession) bool {
	return a.Spec.ThreadRef != nil && b.Spec.ThreadRef != nil && a.Spec.ThreadRef.Name == b.Spec.ThreadRef.Name
}

// mayStart applies the agent's concurrency limit to Pending session s. Sessions of one thread
// never run together, and the agent runs at most limits.maxConcurrentSessions sessions at
// once. The Pending sessions that could start now (retryAt passed, no Running session in their
// thread) are ranked by priority number, then age, then name; s starts only when its rank is
// below the number of free slots.
func (r *OmpSessionReconciler) mayStart(s *ompv1alpha1.OmpSession, agent *ompv1alpha1.OmpAgent, sessions []ompv1alpha1.OmpSession, now time.Time) bool {
	running := 0
	for i := range sessions {
		if o := &sessions[i]; o.UID != s.UID && o.Status.Phase == ompv1alpha1.SessionPhaseRunning {
			running++
		}
	}
	free := maxConcurrent(agent) - running
	if free <= 0 {
		return false
	}
	queue := slices.DeleteFunc(slices.Clone(sessions), func(o ompv1alpha1.OmpSession) bool {
		if o.UID == s.UID {
			return false
		}
		if !o.DeletionTimestamp.IsZero() {
			return true
		}
		if o.Status.Phase != "" && o.Status.Phase != ompv1alpha1.SessionPhasePending {
			return true
		}
		if o.Status.RetryAt != nil && o.Status.RetryAt.After(now) {
			return true
		}
		return slices.ContainsFunc(sessions, func(p ompv1alpha1.OmpSession) bool {
			return p.Status.Phase == ompv1alpha1.SessionPhaseRunning && sameThread(&p, &o)
		})
	})
	slices.SortFunc(queue, func(a, b ompv1alpha1.OmpSession) int {
		if pa, pb := sessionPriority(&a), sessionPriority(&b); pa != pb {
			return int(pa - pb)
		}
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	rank := slices.IndexFunc(queue, func(o ompv1alpha1.OmpSession) bool { return o.UID == s.UID })
	return rank >= 0 && rank < free
}

var fluxSourceGV = schema.GroupVersion{Group: "source.toolkit.fluxcd.io", Version: "v1"}

// workspaceGate waits for the Flux artifacts of the workspace and skill sources. done is
// true when the session must not proceed this round.
func (r *OmpSessionReconciler) workspaceGate(ctx context.Context, s *ompv1alpha1.OmpSession, agent *ompv1alpha1.OmpAgent) (ctrl.Result, bool, error) {
	var refs []ompv1alpha1.FluxSourceRef
	if ws := agent.Spec.Workspace; ws != nil && ws.FluxSource != nil && s.Spec.ThreadRef == nil {
		refs = append(refs, *ws.FluxSource) // a thread sitting does not use the agent workspace
	}
	for _, sk := range agent.Spec.Skills {
		if sk.FluxSource != nil {
			refs = append(refs, sk.FluxSource.FluxSourceRef)
		}
	}
	if len(refs) == 0 {
		return ctrl.Result{}, false, nil
	}

	for _, ref := range refs {
		missing, err := r.sourceMissingArtifact(ctx, ref)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if missing == "" {
			continue
		}
		reason := "NoArtifact"
		if strings.HasSuffix(missing, "not found") {
			reason = "SourceNotFound"
		}
		if err := r.setCondition(ctx, s, metav1.Condition{
			Type: ompv1alpha1.ConditionWorkspaceReady, Status: metav1.ConditionFalse, Reason: reason, Message: missing,
		}); err != nil {
			return ctrl.Result{}, true, err
		}
		if r.Notifier != nil {
			if _, err := r.Notifier.Notice(ctx, agent, noticeWorkspace, durOr(r.NoticeEvery, defaultNoticeEvery),
				agent.Name+": workspace source not ready", missing); err != nil {
				logf.FromContext(ctx).Error(err, "send workspace notice")
			}
		}
		if r.now().Sub(pendingSince(s)) > durOr(r.WorkspaceLimit, defaultWorkspaceLimit) {
			res, err := r.finish(ctx, s, agent, ompv1alpha1.SessionPhaseFailed, "workspace not ready: "+missing, true)
			return res, true, err
		}
		return ctrl.Result{RequeueAfter: durOr(r.NotReadyRequeue, defaultNotReadyRequeue)}, true, nil
	}

	if c := meta.FindStatusCondition(s.Status.Conditions, ompv1alpha1.ConditionWorkspaceReady); c == nil || c.Status != metav1.ConditionTrue {
		if err := r.setCondition(ctx, s, metav1.Condition{
			Type: ompv1alpha1.ConditionWorkspaceReady, Status: metav1.ConditionTrue, Reason: "ArtifactAvailable",
		}); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	return ctrl.Result{}, false, nil
}

// sourceMissingArtifact returns why ref has no usable artifact, or "" when it has one.
func (r *OmpSessionReconciler) sourceMissingArtifact(ctx context.Context, ref ompv1alpha1.FluxSourceRef) (string, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(fluxSourceGV.WithKind(string(ref.Kind)))
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("%s %s/%s not found", ref.Kind, ref.Namespace, ref.Name), nil
		}
		return "", err
	}
	if url, _, _ := unstructured.NestedString(u.Object, "status", "artifact", "url"); url == "" {
		return fmt.Sprintf("%s %s/%s has no artifact yet", ref.Kind, ref.Namespace, ref.Name), nil
	}
	return "", nil
}

// toolchainGate runs a toolsync pod when the agent's toolchain is unsynced or stale. A
// failed attempt is recorded and the session then proceeds with whatever toolchain is stored.
func (r *OmpSessionReconciler) toolchainGate(ctx context.Context, agent *ompv1alpha1.OmpAgent, pods []corev1.Pod) (ctrl.Result, bool, error) {
	if agent.Spec.Toolchain == nil {
		return ctrl.Result{}, false, nil
	}
	now := r.now()

	// Finished toolsync pods: record a failure when the toolchain is still unsynced, then clean up.
	var finished []corev1.Pod
	for _, p := range pods {
		if p.Labels[ompv1alpha1.LabelRole] == string(ompv1alpha1.PodRoleToolsync) && podFinished(&p) {
			finished = append(finished, p)
		}
	}
	if len(finished) > 0 {
		fresh := &ompv1alpha1.OmpAgent{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(agent), fresh); err != nil {
			return ctrl.Result{}, true, err
		}
		agent = fresh
		newest := slices.MaxFunc(finished, func(a, b corev1.Pod) int { return a.CreationTimestamp.Compare(b.CreationTimestamp.Time) })
		if toolchainUnsynced(agent) && (agent.Status.Toolchain.LastAttemptAt == nil ||
			newest.CreationTimestamp.After(agent.Status.Toolchain.LastAttemptAt.Time)) {
			detail, _ := podDetail(&newest)
			if detail == "" {
				detail = "toolsync pod ended without reporting a toolchain"
			}
			if err := r.recordToolsyncFailure(ctx, agent, now, detail); err != nil {
				return ctrl.Result{}, true, err
			}
		}
		for i := range finished {
			if err := client.IgnoreNotFound(r.Delete(ctx, &finished[i])); err != nil {
				return ctrl.Result{}, true, err
			}
		}
	}

	if !toolchainUnsynced(agent) {
		return ctrl.Result{}, false, nil
	}
	if la := agent.Status.Toolchain.LastAttemptAt; la != nil && now.Sub(la.Time) <= durOr(r.ToolchainRetry, defaultToolchainRetry) {
		return ctrl.Result{}, false, nil // failed recently: continue with what is stored
	}

	if err := r.ensureTokenSecret(ctx, agent.Namespace, agentapi.ToolsyncTokenSecretName(agent.Name), agent.Name, agent); err != nil {
		return ctrl.Result{}, true, err
	}
	pod := podspec.ToolsyncPod(agent, r.Options, now)
	if err := controllerutil.SetControllerReference(agent, pod, r.scheme()); err != nil {
		return ctrl.Result{}, true, err
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, true, fmt.Errorf("create toolsync pod: %w", err)
	}
	return ctrl.Result{RequeueAfter: durOr(r.ToolchainRequeue, defaultToolchainRequeue)}, true, nil
}

func toolchainUnsynced(agent *ompv1alpha1.OmpAgent) bool {
	return agent.Status.Toolchain.SyncedAt == nil || agent.Status.Toolchain.Stale
}

func (r *OmpSessionReconciler) recordToolsyncFailure(ctx context.Context, agent *ompv1alpha1.OmpAgent, at time.Time, detail string) error {
	t := metav1.NewTime(at)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &ompv1alpha1.OmpAgent{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(agent), cur); err != nil {
			return err
		}
		base := cur.DeepCopy()
		cur.Status.Toolchain.LastAttemptAt = &t
		cur.Status.Toolchain.LastError = truncate(detail, maxErrorLen)
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type: ompv1alpha1.ConditionToolchainReady, Status: metav1.ConditionFalse, Reason: "ToolsyncFailed",
			Message: truncate(detail, 500), ObservedGeneration: cur.Generation,
		})
		if err := r.Status().Patch(ctx, cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		*agent = *cur
		return nil
	})
	if err != nil {
		return fmt.Errorf("record toolsync failure: %w", err)
	}
	if r.Notifier != nil {
		if _, err := r.Notifier.Notice(ctx, agent, noticeToolchain, durOr(r.NoticeEvery, defaultNoticeEvery),
			agent.Name+": toolchain sync failed", detail); err != nil {
			logf.FromContext(ctx).Error(err, "send toolchain notice")
		}
	}
	return nil
}

// ensureTokenSecret creates the API token Secret once; an existing one is kept.
func (r *OmpSessionReconciler) ensureTokenSecret(ctx context.Context, ns, name, agent string, owner client.Object) error {
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &corev1.Secret{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	sec, err := podspec.TokenSecret(ns, name, agent)
	if err != nil {
		return err
	}
	if err := controllerutil.SetOwnerReference(owner, sec, r.scheme()); err != nil {
		return err
	}
	if err := r.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create token secret: %w", err)
	}
	return nil
}

// adoptStartedPod records a pod this session already created for its next attempt, left
// unrecorded by a failed status write.
func (r *OmpSessionReconciler) adoptStartedPod(ctx context.Context, s *ompv1alpha1.OmpSession) (bool, error) {
	pod := &corev1.Pod{}
	name := podspec.SessionPodName(s.Name, s.Status.Attempts+1)
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, pod); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(pod, s) {
		return false, nil
	}
	return true, r.markRunning(ctx, s, name)
}

func (r *OmpSessionReconciler) markRunning(ctx context.Context, s *ompv1alpha1.OmpSession, pod string) error {
	now := metav1.NewTime(r.now())
	return r.updateStatus(ctx, s, func(cur *ompv1alpha1.OmpSession) error {
		if cur.Status.Phase == ompv1alpha1.SessionPhaseRunning || cur.Status.Phase == ompv1alpha1.SessionPhaseSucceeded ||
			cur.Status.Phase == ompv1alpha1.SessionPhaseFailed {
			return errStale
		}
		cur.Status.Phase = ompv1alpha1.SessionPhaseRunning
		cur.Status.Attempts++
		if cur.Status.StartTime == nil {
			cur.Status.StartTime = &now
		}
		cur.Status.PodName = pod
		cur.Status.RetryAt = nil
		cur.Status.Outcome = "" // the new attempt reports its own outcome
		return nil
	})
}

// start creates the session's token Secret and the pod of the next attempt.
func (r *OmpSessionReconciler) start(ctx context.Context, s *ompv1alpha1.OmpSession, agent *ompv1alpha1.OmpAgent) (ctrl.Result, error) {
	if err := r.ensureTokenSecret(ctx, s.Namespace, agentapi.SessionTokenSecretName(s.Name), agent.Name, s); err != nil {
		return ctrl.Result{}, err
	}
	var thread *ompv1alpha1.OmpThread
	if ref := s.Spec.ThreadRef; ref != nil {
		thread = &ompv1alpha1.OmpThread{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: ref.Name}, thread); err != nil {
			if apierrors.IsNotFound(err) {
				return r.finish(ctx, s, agent, ompv1alpha1.SessionPhaseFailed, "thread not found", true)
			}
			return ctrl.Result{}, err
		}
	}
	pod := podspec.SessionPod(agent, s, thread, r.Options, s.Status.Attempts+1)
	if err := controllerutil.SetControllerReference(s, pod, r.scheme()); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("create session pod: %w", err)
	}
	if err := r.markRunning(ctx, s, pod.Name); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: durOr(r.PodStartLimit, defaultPodStartLimit)}, nil
}

// ---- running ----

func (r *OmpSessionReconciler) reconcileRunning(ctx context.Context, s *ompv1alpha1.OmpSession) (ctrl.Result, error) {
	agent, err := r.getAgent(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	if agent == nil {
		r.deletePod(ctx, s.Namespace, s.Status.PodName)
		return r.finish(ctx, s, nil, ompv1alpha1.SessionPhaseFailed, "agent not found", true)
	}

	pod := &corev1.Pod{}
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Status.PodName}, pod)
	if apierrors.IsNotFound(err) {
		return r.attemptEnded(ctx, s, agent, nil)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return r.attemptEnded(ctx, s, agent, pod)
	case corev1.PodRunning:
		return ctrl.Result{}, nil
	}

	// Pending or unknown: the pod must start in time.
	limit := durOr(r.PodStartLimit, defaultPodStartLimit)
	age := r.now().Sub(pod.CreationTimestamp.Time)
	if age <= limit {
		return ctrl.Result{RequeueAfter: limit - age}, nil
	}
	detail, _ := podDetail(pod)
	if detail == "" {
		detail = "pod did not start"
	}
	r.deletePod(ctx, pod.Namespace, pod.Name)
	return r.retryOrFail(ctx, s, agent, fmt.Sprintf("pod not running after %s: %s", limit, detail), false)
}

// attemptEnded applies the result table to a pod that ended or disappeared (pod is nil).
func (r *OmpSessionReconciler) attemptEnded(ctx context.Context, s *ompv1alpha1.OmpSession, agent *ompv1alpha1.OmpAgent, pod *corev1.Pod) (ctrl.Result, error) {
	// The agent API writes outcome and verdict before the pod exits; read them fresh.
	fresh := &ompv1alpha1.OmpSession{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(s), fresh); err != nil {
		return ctrl.Result{}, err
	}
	*s = *fresh

	var detail string
	var deadline bool
	if pod != nil {
		detail, deadline = podDetail(pod)
	} else {
		detail = "pod disappeared"
	}

	switch {
	case s.Spec.ThreadRef != nil && s.Status.Outcome == ompv1alpha1.OutcomeCompleted:
		// A sitting ends by idling out, without a verdict; its turns carry the results.
		return r.finish(ctx, s, agent, ompv1alpha1.SessionPhaseSucceeded, "", false)
	case s.Status.Verdict != "":
		return r.finish(ctx, s, agent, ompv1alpha1.SessionPhaseSucceeded, "", false)
	case s.Status.Outcome == ompv1alpha1.OutcomeCompleted:
		return r.finish(ctx, s, agent, ompv1alpha1.SessionPhaseFailed, "agent ended without calling finish", true)
	case s.Status.Outcome == ompv1alpha1.OutcomeError:
		return r.finish(ctx, s, agent, ompv1alpha1.SessionPhaseFailed, "agent reported an error", false)
	case s.Status.Outcome == ompv1alpha1.OutcomeRetryable:
		return r.retryOrFail(ctx, s, agent, "agent reported a retryable failure", false)
	case s.Status.Outcome == ompv1alpha1.OutcomeAuth:
		return r.retryOrFail(ctx, s, agent, "model authentication was rejected", true)
	case deadline:
		return r.finish(ctx, s, agent, ompv1alpha1.SessionPhaseFailed, "session timed out: pod deadline exceeded", true)
	}
	msg := "pod ended without reporting completion"
	if detail != "" {
		msg += ": " + detail
	}
	return r.retryOrFail(ctx, s, agent, msg, false)
}

// retryOrFail sends the session back to Pending after the agent's retry delay while attempts
// remain, else fails it. auth also notifies the owner that the model login needs attention.
func (r *OmpSessionReconciler) retryOrFail(ctx context.Context, s *ompv1alpha1.OmpSession, agent *ompv1alpha1.OmpAgent, reason string, auth bool) (ctrl.Result, error) {
	if auth {
		metrics.ModelAuthFailuresTotal.WithLabelValues(s.Namespace, agent.Name).Inc()
		if r.Notifier != nil {
			title := agent.Name + ": model login missing or expired"
			if _, err := r.Notifier.Notice(ctx, agent, noticeAuth, durOr(r.NoticeEvery, defaultNoticeEvery), title, r.authNoticeText(agent, title)); err != nil {
				logf.FromContext(ctx).Error(err, "send auth notice")
			}
		}
	}
	if s.Status.Attempts >= podspec.MaxAttempts(agent) {
		if s.Status.Error != "" {
			reason = s.Status.Error // the agent's own account of the last failure
		}
		return r.finish(ctx, s, agent, ompv1alpha1.SessionPhaseFailed,
			fmt.Sprintf("attempts exhausted (%d): %s", s.Status.Attempts, reason), true)
	}
	delay := podspec.RetryDelay(agent)
	at := metav1.NewTime(r.now().Add(delay))
	err := r.updateStatus(ctx, s, func(cur *ompv1alpha1.OmpSession) error {
		if cur.Status.Phase != ompv1alpha1.SessionPhaseRunning {
			return errStale
		}
		cur.Status.Phase = ompv1alpha1.SessionPhasePending
		cur.Status.RetryAt = &at
		if cur.Status.Error == "" {
			cur.Status.Error = truncate(reason, maxErrorLen)
		}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: delay}, nil
}

func (r *OmpSessionReconciler) authNoticeText(agent *ompv1alpha1.OmpAgent, title string) string {
	text := title + "."
	if g := agent.Spec.Model.Gateway; g != nil {
		provider, _, _ := strings.Cut(agent.Spec.Model.Default, "/")
		ns := r.Options.OperatorNamespace
		text += fmt.Sprintf(" Log in again through the auth gateway: "+
			"kubectl -n %[1]s port-forward deploy/omp-auth 54545:54545 & "+
			"kubectl -n %[1]s exec -it deploy/omp-auth -c gateway -- omp auth-broker login %[2]s; "+
			"then kubectl -n %[1]s rollout restart deploy/omp-auth", ns, provider)
	}
	return text
}

func (r *OmpSessionReconciler) deletePod(ctx context.Context, ns, name string) {
	if name == "" {
		return
	}
	pod := &corev1.Pod{}
	pod.Namespace, pod.Name = ns, name
	if err := r.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
		logf.FromContext(ctx).Error(err, "delete pod", "pod", name)
	}
}

// ---- helpers ----

// getAgent returns the session's agent, or nil when it does not exist.
func (r *OmpSessionReconciler) getAgent(ctx context.Context, s *ompv1alpha1.OmpSession) (*ompv1alpha1.OmpAgent, error) {
	agent := &ompv1alpha1.OmpAgent{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Spec.AgentRef.Name}, agent)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return agent, err
}

// updateStatus applies mutate to a fresh copy of the session and patches the status with an
// optimistic lock, retrying on conflict; s is replaced by the result. mutate returns
// errNoChange to skip the write, or errStale when its precondition no longer holds.
func (r *OmpSessionReconciler) updateStatus(ctx context.Context, s *ompv1alpha1.OmpSession, mutate func(*ompv1alpha1.OmpSession) error) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &ompv1alpha1.OmpSession{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(s), cur); err != nil {
			return err
		}
		base := cur.DeepCopy()
		if err := mutate(cur); err != nil {
			if errors.Is(err, errNoChange) {
				*s = *cur
			}
			return err
		}
		if err := r.Status().Patch(ctx, cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		*s = *cur
		return nil
	})
	if errors.Is(err, errNoChange) {
		return nil
	}
	return err
}

// setCondition records c on the session, writing only when it changes.
func (r *OmpSessionReconciler) setCondition(ctx context.Context, s *ompv1alpha1.OmpSession, c metav1.Condition) error {
	return r.updateStatus(ctx, s, func(cur *ompv1alpha1.OmpSession) error {
		c.ObservedGeneration = cur.Generation
		if !meta.SetStatusCondition(&cur.Status.Conditions, c) {
			return errNoChange
		}
		return nil
	})
}

// updateActiveGauge republishes the Pending and Running session counts of the agent.
func (r *OmpSessionReconciler) updateActiveGauge(ctx context.Context, ns, agent string) {
	var sl ompv1alpha1.OmpSessionList
	if err := r.List(ctx, &sl, client.InNamespace(ns)); err != nil {
		return
	}
	var pending, running float64
	for _, o := range sl.Items {
		if o.Spec.AgentRef.Name != agent || !o.DeletionTimestamp.IsZero() {
			continue
		}
		switch o.Status.Phase {
		case "", ompv1alpha1.SessionPhasePending:
			pending++
		case ompv1alpha1.SessionPhaseRunning:
			running++
		}
	}
	metrics.SessionsActive.WithLabelValues(ns, agent, string(ompv1alpha1.SessionPhasePending)).Set(pending)
	metrics.SessionsActive.WithLabelValues(ns, agent, string(ompv1alpha1.SessionPhaseRunning)).Set(running)
}

func podFinished(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// podDetail describes why a pod ended badly and whether its active deadline was exceeded.
func podDetail(p *corev1.Pod) (detail string, deadline bool) {
	deadline = p.Status.Reason == "DeadlineExceeded"
	var parts []string
	if p.Status.Reason != "" {
		parts = append(parts, p.Status.Reason)
	}
	if p.Status.Message != "" {
		parts = append(parts, p.Status.Message)
	}
	statuses := slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses)
	for _, cs := range statuses {
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			msg := fmt.Sprintf("container %s exited %d", cs.Name, t.ExitCode)
			if t.Reason != "" {
				msg += " (" + t.Reason + ")"
			}
			if t.Message != "" {
				msg += ": " + t.Message
			}
			parts = append(parts, msg)
		}
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" && w.Reason != "ContainerCreating" {
			msg := fmt.Sprintf("container %s waiting: %s", cs.Name, w.Reason)
			if w.Message != "" {
				msg += ": " + w.Message
			}
			parts = append(parts, msg)
		}
	}
	return strings.Join(parts, "; "), deadline
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// SetupWithManager sets up the controller with the Manager.
func (r *OmpSessionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ompv1alpha1.OmpSession{}).
		Owns(&corev1.Pod{}).
		Named("ompsession").
		Complete(r)
}
