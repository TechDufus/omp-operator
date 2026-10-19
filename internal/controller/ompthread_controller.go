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
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/cloudspec"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/metrics"
	"github.com/TechDufus/omp-operator/internal/store"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	// AnnotationTurnsFailed marks a Failed sitting whose open turns were already failed.
	AnnotationTurnsFailed = "omp.techdufus.com/turns-failed"

	defaultThreadWorkingRequeue = 30 * time.Second
	defaultThreadIdleRecheck    = 5 * time.Minute
	defaultThreadAgentRecheck   = 5 * time.Minute

	// maxSittingNameTries is how many sitting numbers are tried when names are taken.
	maxSittingNameTries = 5

	maxSummaryTitle = 80
	maxSummaryReply = 3000
	maxSummaryError = 1000
	// maxSummaryQuestion is the length of the question text kept in the thread status.
	maxSummaryQuestion = 1000
)

// ThreadNotifier tells an agent's sinks that a thread turn finished. *notify.Dispatcher
// satisfies it.
type ThreadNotifier interface {
	TurnDone(ctx context.Context, agent *ompv1alpha1.OmpAgent, thread *ompv1alpha1.OmpThread, turn threads.Turn) error
}

// OmpThreadReconciler serves OmpThreads: it starts a sitting (an OmpSession) whenever a turn is
// queued and no sitting is active, requeues or fails the turns of an ended sitting, notifies
// finished turns, mirrors the turn log into status and deletes idle threads after the agent's
// retention.
//
// The turn log itself lives in the store (threads.Log); the agent API and the cloud API write
// it and signal this reconciler through Threads.Events().
type OmpThreadReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads uncached: threads, agents and sittings.
	APIReader client.Reader
	Options   *config.Options
	Store     *store.Store
	Threads   *threads.Log
	// Notifier delivers turn results; nil disables notifications.
	Notifier ThreadNotifier
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	// Timings; zero means the default named in parentheses.
	WorkingRequeue time.Duration // recheck of a Working thread (30s)
	IdleRecheck    time.Duration // longest wait between checks of an Idle thread (5m)
	AgentRecheck   time.Duration // wait for a missing agent (5m)
}

// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompthreads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompthreads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompthreads/finalizers,verbs=update

func (r *OmpThreadReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *OmpThreadReconciler) scheme() *runtime.Scheme {
	if r.Scheme != nil {
		return r.Scheme
	}
	return r.Client.Scheme()
}

// Reconcile advances one thread.
func (r *OmpThreadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	th := &ompv1alpha1.OmpThread{}
	if err := r.APIReader.Get(ctx, req.NamespacedName, th); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !r.Options.NamespaceAllowed(th.Namespace) {
		return ctrl.Result{}, nil
	}
	defer r.updateThreadsGauge(ctx, th.Namespace, th.Spec.AgentRef.Name)

	// 1. deleting
	if !th.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, th)
	}
	if !controllerutil.ContainsFinalizer(th, ompv1alpha1.FinalizerCleanup) {
		base := th.DeepCopy()
		controllerutil.AddFinalizer(th, ompv1alpha1.FinalizerCleanup)
		if err := r.Patch(ctx, th, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	// 2. agent
	agent := &ompv1alpha1.OmpAgent{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: th.Namespace, Name: th.Spec.AgentRef.Name}, agent); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		cond := metav1.Condition{
			Type: ompv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "AgentNotFound",
			Message: fmt.Sprintf("agent %s does not exist", th.Spec.AgentRef.Name),
		}
		if err := r.patchStatus(ctx, th, func(cur *ompv1alpha1.OmpThread) bool {
			cond.ObservedGeneration = cur.Generation
			return meta.SetStatusCondition(&cur.Status.Conditions, cond)
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: durOr(r.AgentRecheck, defaultThreadAgentRecheck)}, nil
	}

	// 3. sittings and turns
	sittings, err := r.sittings(ctx, th)
	if err != nil {
		return ctrl.Result{}, err
	}
	active := activeSitting(sittings)

	// 4. no active sitting: settle the last one, start the next
	activeName := ""
	if active != nil {
		activeName = active.Name
	} else if activeName, err = r.settleAndStart(ctx, th, sittings); err != nil {
		return ctrl.Result{}, err
	}

	// 5. notify finished turns
	turns, err := r.notifyTurns(ctx, agent, th)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 6. status
	now := r.now()
	st := threadStatus(th, turns, activeName)
	if err := r.patchStatus(ctx, th, func(cur *ompv1alpha1.OmpThread) bool {
		return applyThreadStatus(cur, st)
	}); err != nil {
		return ctrl.Result{}, err
	}

	// 7. retention
	retention := cloudspec.ThreadRetention(cloudOf(agent))
	if st.phase == ompv1alpha1.ThreadPhaseIdle && now.Sub(st.lastActivity) > retention {
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, th, client.PropagationPolicy(metav1.DeletePropagationBackground)))
	}

	// 8. requeue
	if st.phase != ompv1alpha1.ThreadPhaseIdle {
		return ctrl.Result{RequeueAfter: durOr(r.WorkingRequeue, defaultThreadWorkingRequeue)}, nil
	}
	left := st.lastActivity.Add(retention).Sub(now)
	return ctrl.Result{RequeueAfter: max(time.Second, min(durOr(r.IdleRecheck, defaultThreadIdleRecheck), left))}, nil
}

// cloudOf returns the agent's cloud settings, or an empty spec (all defaults) when it has none.
func cloudOf(agent *ompv1alpha1.OmpAgent) *ompv1alpha1.CloudSpec {
	if agent.Spec.Cloud != nil {
		return agent.Spec.Cloud
	}
	return &ompv1alpha1.CloudSpec{}
}

// finalize removes what the thread stored: its files and its conversation tarball.
func (r *OmpThreadReconciler) finalize(ctx context.Context, th *ompv1alpha1.OmpThread) error {
	if !controllerutil.ContainsFinalizer(th, ompv1alpha1.FinalizerCleanup) {
		return nil
	}
	if r.Store != nil {
		if err := r.Store.RemoveThread(th.Namespace, th.Name); err != nil {
			return fmt.Errorf("remove thread files: %w", err)
		}
		id := store.ContinuityID(cloudspec.ContinuityKey(th.Namespace, th.Name))
		if err := r.Store.RemoveContinuity(th.Namespace, th.Spec.AgentRef.Name, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("remove continuity: %w", err)
		}
	}
	base := th.DeepCopy()
	controllerutil.RemoveFinalizer(th, ompv1alpha1.FinalizerCleanup)
	return client.IgnoreNotFound(r.Patch(ctx, th, client.MergeFrom(base)))
}

// sittings lists the thread's sessions, oldest first.
func (r *OmpThreadReconciler) sittings(ctx context.Context, th *ompv1alpha1.OmpThread) ([]ompv1alpha1.OmpSession, error) {
	var list ompv1alpha1.OmpSessionList
	if err := r.APIReader.List(ctx, &list, client.InNamespace(th.Namespace),
		client.MatchingLabels{ompv1alpha1.LabelThread: th.Name}); err != nil {
		return nil, err
	}
	out := list.Items
	slices.SortFunc(out, func(a, b ompv1alpha1.OmpSession) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		if na, nb := sittingNumber(th.Name, a.Name), sittingNumber(th.Name, b.Name); na != nb {
			return na - nb
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// sittingNumber is n of a sitting named <thread>-<n>, or 0.
func sittingNumber(thread, name string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(name, thread+"-"))
	if err != nil {
		return 0
	}
	return n
}

// activeSitting is the newest sitting that has not ended, or nil.
func activeSitting(sittings []ompv1alpha1.OmpSession) *ompv1alpha1.OmpSession {
	for i := len(sittings) - 1; i >= 0; i-- {
		switch sittings[i].Status.Phase {
		case ompv1alpha1.SessionPhaseSucceeded, ompv1alpha1.SessionPhaseFailed:
		default:
			return &sittings[i]
		}
	}
	return nil
}

// settleAndStart handles a thread without an active sitting: the turns the last sitting left
// behind are failed (it failed for good) or put back in the queue (it ended otherwise), then a
// new sitting starts when a turn is queued. It returns the name of the new sitting, or "".
func (r *OmpThreadReconciler) settleAndStart(ctx context.Context, th *ompv1alpha1.OmpThread, sittings []ompv1alpha1.OmpSession) (string, error) {
	var newest *ompv1alpha1.OmpSession
	if len(sittings) > 0 {
		newest = &sittings[len(sittings)-1]
	}
	if newest != nil && newest.Status.Phase == ompv1alpha1.SessionPhaseFailed && newest.Annotations[AnnotationTurnsFailed] != "true" {
		if _, err := r.Threads.FailOpen(th.Namespace, th.Name, "cloud session failed: "+newest.Status.Error); err != nil {
			return "", fmt.Errorf("fail open turns: %w", err)
		}
		base := newest.DeepCopy()
		if newest.Annotations == nil {
			newest.Annotations = map[string]string{}
		}
		newest.Annotations[AnnotationTurnsFailed] = "true"
		if err := r.Patch(ctx, newest, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("annotate failed sitting: %w", err)
		}
		return "", nil
	}
	if newest != nil {
		if _, err := r.Threads.Requeue(th.Namespace, th.Name, newest.Name); err != nil {
			return "", fmt.Errorf("requeue turns: %w", err)
		}
	}
	turns, err := r.Threads.Turns(th.Namespace, th.Name)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(turns, func(t threads.Turn) bool { return t.State == ompv1alpha1.TurnStateQueued })
	if i < 0 {
		return "", nil
	}
	return r.createSitting(ctx, th, len(sittings)+1, turns[i].Text)
}

// createSitting creates OmpSession <thread>-<n>, trying the following numbers when a name is
// taken, with the oldest queued turn's text as its prompt.
func (r *OmpThreadReconciler) createSitting(ctx context.Context, th *ompv1alpha1.OmpThread, n int, text string) (string, error) {
	key := cloudspec.ContinuityKey(th.Namespace, th.Name)
	for range maxSittingNameTries {
		s := &ompv1alpha1.OmpSession{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: th.Namespace,
				Name:      th.Name + "-" + strconv.Itoa(n),
				Labels: map[string]string{
					ompv1alpha1.LabelAgent:      th.Spec.AgentRef.Name,
					ompv1alpha1.LabelKind:       string(ompv1alpha1.SessionKindCloud),
					ompv1alpha1.LabelThread:     th.Name,
					ompv1alpha1.LabelContinuity: store.ContinuityID(key),
				},
			},
			Spec: ompv1alpha1.OmpSessionSpec{
				AgentRef:      ompv1alpha1.LocalObjectRef{Name: th.Spec.AgentRef.Name},
				ThreadRef:     &ompv1alpha1.LocalObjectRef{Name: th.Name},
				Prompt:        text,
				Priority:      new(int32(1)),
				ContinuityKey: key,
			},
		}
		if err := controllerutil.SetControllerReference(th, s, r.scheme()); err != nil {
			return "", err
		}
		agent := &ompv1alpha1.OmpAgent{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: th.Namespace, Name: th.Spec.AgentRef.Name}, agent); err != nil {
			return "", err
		}
		if err := controllerutil.SetOwnerReference(agent, s, r.scheme()); err != nil {
			return "", err
		}
		err := r.Create(ctx, s)
		switch {
		case err == nil:
			return s.Name, nil
		case apierrors.IsAlreadyExists(err):
			n++
		default:
			return "", fmt.Errorf("create sitting: %w", err)
		}
	}
	return "", fmt.Errorf("create sitting: names %s-N are taken", th.Name)
}

// notifyTurns tells the sinks about each finished turn once and records it. A delivery failure is
// logged, not retried, so sinks that succeeded are not told twice. It returns the fresh turns.
func (r *OmpThreadReconciler) notifyTurns(ctx context.Context, agent *ompv1alpha1.OmpAgent, th *ompv1alpha1.OmpThread) ([]threads.Turn, error) {
	turns, err := r.Threads.Turns(th.Namespace, th.Name)
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, t := range turns {
		if !t.Finished() || t.NotifiedAt != nil {
			continue
		}
		ids = append(ids, t.ID)
		if t.State == ompv1alpha1.TurnStateCancelled || r.Notifier == nil {
			continue
		}
		if err := r.Notifier.TurnDone(ctx, agent, th, t); err != nil {
			logf.FromContext(ctx).Error(err, "notify thread turn", "thread", client.ObjectKeyFromObject(th), "turn", t.ID)
		}
	}
	if len(ids) == 0 {
		return turns, nil
	}
	if err := r.Threads.MarkNotified(th.Namespace, th.Name, ids); err != nil {
		return nil, fmt.Errorf("mark turns notified: %w", err)
	}
	for _, t := range turns {
		if slices.Contains(ids, t.ID) {
			metrics.ThreadTurnsTotal.WithLabelValues(th.Namespace, th.Spec.AgentRef.Name, string(t.State)).Inc()
		}
	}
	return r.Threads.Turns(th.Namespace, th.Name)
}

// computedStatus is the part of the thread status derived from the turn log.
type computedStatus struct {
	phase        ompv1alpha1.ThreadPhase
	turns        int32
	queued       int32
	active       string
	latest       *ompv1alpha1.ThreadTurnSummary
	result       *ompv1alpha1.ThreadResult
	lastActivity time.Time
	question     *ompv1alpha1.ThreadQuestion
}

func threadStatus(th *ompv1alpha1.OmpThread, turns []threads.Turn, active string) computedStatus {
	st := computedStatus{
		phase:        ompv1alpha1.ThreadPhaseIdle,
		turns:        int32(len(turns)),
		active:       active,
		lastActivity: th.CreationTimestamp.Time,
	}
	bump := func(t *time.Time) {
		if t != nil && t.After(st.lastActivity) {
			st.lastActivity = *t
		}
	}
	for _, t := range turns {
		switch t.State {
		case ompv1alpha1.TurnStateQueued:
			st.queued++
			st.phase = ompv1alpha1.ThreadPhaseWorking
		case ompv1alpha1.TurnStateRunning:
			st.phase = ompv1alpha1.ThreadPhaseWorking
		}
		q := t.QueuedAt
		bump(&q)
		bump(t.StartedAt)
		bump(t.CompletedAt)
		if t.State == ompv1alpha1.TurnStateDone || t.State == ompv1alpha1.TurnStateFailed {
			st.latest = &ompv1alpha1.ThreadTurnSummary{
				ID:    int32(t.ID),
				State: t.State,
				Title: truncate(t.Title, maxSummaryTitle),
				Reply: truncate(t.Reply, maxSummaryReply),
				Error: truncate(t.Error, maxSummaryError),
			}
			if t.CompletedAt != nil {
				ct := secondTime(*t.CompletedAt)
				st.latest.CompletedAt = &ct
			}
		}
		if t.Head != "" {
			res := &ompv1alpha1.ThreadResult{
				Branch:       cloudspec.Branch(th.Name),
				Head:         t.Head,
				Base:         th.Spec.Repo.BaseCommit,
				FilesChanged: int32(t.FilesChanged),
				Insertions:   int32(t.Insertions),
				Deletions:    int32(t.Deletions),
				Pushed:       t.Pushed,
				PushError:    truncate(t.PushError, 300),
				UpdatedAt:    secondTime(t.QueuedAt),
			}
			if t.CompletedAt != nil {
				res.UpdatedAt = secondTime(*t.CompletedAt)
			}
			st.result = res
		}
	}
	if p := threads.PendingQuestion(turns); p != nil {
		st.phase = ompv1alpha1.ThreadPhaseWaiting
		st.question = &ompv1alpha1.ThreadQuestion{
			Turn: int32(p.Turn), ID: int32(p.Question.ID), Text: truncate(p.Question.Text, maxSummaryQuestion),
			AskedAt: secondTime(p.Question.AskedAt),
		}
	}
	return st
}

// applyThreadStatus writes st into cur and reports whether anything changed.
func applyThreadStatus(cur *ompv1alpha1.OmpThread, st computedStatus) bool {
	before := cur.Status.DeepCopy()
	s := &cur.Status
	s.ObservedGeneration = cur.Generation
	s.Phase, s.Turns, s.Queued, s.ActiveSession = st.phase, st.turns, st.queued, st.active
	s.Latest, s.Result, s.Question = st.latest, st.result, st.question
	la := secondTime(st.lastActivity)
	s.LastActivityAt = &la
	meta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type: ompv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: cur.Generation,
	})
	return !equalThreadStatus(before, s)
}

// secondTime is t at the precision the API server stores.
func secondTime(t time.Time) metav1.Time { return metav1.NewTime(t.UTC().Truncate(time.Second)) }

func equalThreadStatus(a, b *ompv1alpha1.OmpThreadStatus) bool {
	return a.ObservedGeneration == b.ObservedGeneration && a.Phase == b.Phase && a.Turns == b.Turns &&
		a.Queued == b.Queued && a.ActiveSession == b.ActiveSession &&
		equalPtr(a.Latest, b.Latest, func(x, y *ompv1alpha1.ThreadTurnSummary) bool {
			return x.ID == y.ID && x.State == y.State && x.Title == y.Title && x.Reply == y.Reply &&
				x.Error == y.Error && equalTimePtr(x.CompletedAt, y.CompletedAt)
		}) &&
		equalPtr(a.Result, b.Result, func(x, y *ompv1alpha1.ThreadResult) bool {
			return x.Branch == y.Branch && x.Head == y.Head && x.Base == y.Base && x.FilesChanged == y.FilesChanged &&
				x.Insertions == y.Insertions && x.Deletions == y.Deletions && x.Pushed == y.Pushed &&
				x.PushError == y.PushError && x.UpdatedAt.Equal(&y.UpdatedAt)
		}) &&
		equalTimePtr(a.LastActivityAt, b.LastActivityAt) &&
		equalPtr(a.Question, b.Question, equalThreadQuestion) &&
		len(a.Conditions) == len(b.Conditions) &&
		!slices.ContainsFunc(b.Conditions, func(c metav1.Condition) bool {
			o := meta.FindStatusCondition(a.Conditions, c.Type)
			return o == nil || o.Status != c.Status || o.Reason != c.Reason || o.Message != c.Message ||
				o.ObservedGeneration != c.ObservedGeneration
		})
}

func equalThreadQuestion(x, y *ompv1alpha1.ThreadQuestion) bool {
	return x.Turn == y.Turn && x.ID == y.ID && x.Text == y.Text && x.AskedAt.Equal(&y.AskedAt)
}

func equalPtr[T any](a, b *T, eq func(x, y *T) bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return eq(a, b)
}

func equalTimePtr(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}

// patchStatus applies mutate to a fresh copy of the thread and patches its status with an
// optimistic lock, retrying on conflict; th is replaced by the result. mutate returns false when
// nothing changed.
func (r *OmpThreadReconciler) patchStatus(ctx context.Context, th *ompv1alpha1.OmpThread, mutate func(*ompv1alpha1.OmpThread) bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &ompv1alpha1.OmpThread{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(th), cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		base := cur.DeepCopy()
		if !mutate(cur) {
			*th = *cur
			return nil
		}
		if err := r.Status().Patch(ctx, cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		*th = *cur
		return nil
	})
}

// updateThreadsGauge republishes the Working, Waiting and Idle thread counts of the agent.
func (r *OmpThreadReconciler) updateThreadsGauge(ctx context.Context, ns, agent string) {
	var list ompv1alpha1.OmpThreadList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return
	}
	var working, waiting, idle float64
	for _, t := range list.Items {
		if t.Spec.AgentRef.Name != agent || !t.DeletionTimestamp.IsZero() {
			continue
		}
		switch t.Status.Phase {
		case ompv1alpha1.ThreadPhaseWorking:
			working++
		case ompv1alpha1.ThreadPhaseWaiting:
			waiting++
		default:
			idle++
		}
	}
	metrics.ThreadsActive.WithLabelValues(ns, agent, string(ompv1alpha1.ThreadPhaseWorking)).Set(working)
	metrics.ThreadsActive.WithLabelValues(ns, agent, string(ompv1alpha1.ThreadPhaseWaiting)).Set(waiting)
	metrics.ThreadsActive.WithLabelValues(ns, agent, string(ompv1alpha1.ThreadPhaseIdle)).Set(idle)
}

// SetupWithManager sets up the controller with the Manager.
func (r *OmpThreadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&ompv1alpha1.OmpThread{}).
		Owns(&ompv1alpha1.OmpSession{}).
		Named("ompthread")
	if r.Threads != nil {
		b = b.WatchesRawSource(source.Channel(r.Threads.Events(), &handler.EnqueueRequestForObject{}))
	}
	return b.Complete(r)
}
