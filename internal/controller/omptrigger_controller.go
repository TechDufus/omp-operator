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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/adapters"
	"github.com/TechDufus/omp-operator/internal/celx"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/hooks"
	"github.com/TechDufus/omp-operator/internal/metrics"
	"github.com/TechDufus/omp-operator/internal/prompt"
	"github.com/TechDufus/omp-operator/internal/schedule"
	"github.com/TechDufus/omp-operator/internal/store"
)

// Ready condition reasons of an OmpTrigger.
const (
	triggerReasonReady             = "Ready"
	triggerReasonAgentNotFound     = "AgentNotFound"
	triggerReasonInvalidSpec       = "InvalidSpec"
	triggerReasonInvalidExpression = "InvalidExpression"
	triggerReasonInvalidPrompt     = "InvalidPrompt"
	triggerReasonMissingSecret     = "MissingSecret"
	triggerReasonInvalidSchedule   = "InvalidSchedule"

	// DefaultSchedulePriority is the priority of a schedule session when the trigger sets none.
	DefaultSchedulePriority int32 = 3

	// TriggerRecheckInterval is how often a trigger is re-evaluated when nothing changes.
	TriggerRecheckInterval = 5 * time.Minute
)

// OmpTriggerReconciler reconciles an OmpTrigger: it reports Ready, sets
// status.type and status.hookPath, and fires schedule triggers.
//
// Webhook triggers are served by internal/hooks, which shares CEL (the compiled
// program cache) with this reconciler.
type OmpTriggerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads Secrets uncached; SetupWithManager defaults it to the manager's.
	APIReader client.Reader
	// Options holds the operator-wide settings (allowed namespaces).
	Options *config.Options
	// CEL caches compiled programs; share it with the hooks Handler.
	CEL *celx.Cache
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=omp.techdufus.com,resources=omptriggers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=omp.techdufus.com,resources=omptriggers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=omp.techdufus.com,resources=omptriggers/finalizers,verbs=update

func (r *OmpTriggerReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *OmpTriggerReconciler) namespaceAllowed(ns string) bool {
	return r.Options == nil || r.Options.NamespaceAllowed(ns)
}

func (r *OmpTriggerReconciler) compiler(t *ompv1alpha1.OmpTrigger) celx.Compiler {
	if r.CEL == nil {
		return celx.Compile
	}
	return r.CEL.Compiler(t.Namespace, t.Name, t.Generation)
}

// triggerEvaluation is the outcome of the Ready checks.
type triggerEvaluation struct {
	agent    *ompv1alpha1.OmpAgent
	schedule *schedule.Schedule
	reason   string
	message  string
}

func (e triggerEvaluation) ready() bool { return e.reason == "" }

// Reconcile checks the trigger, publishes its status and fires due schedules.
func (r *OmpTriggerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	t := &ompv1alpha1.OmpTrigger{}
	if err := r.Get(ctx, req.NamespacedName, t); err != nil {
		if apierrors.IsNotFound(err) {
			if r.CEL != nil {
				r.CEL.Forget(req.Namespace, req.Name)
			}
			metrics.TriggerGroupsActive.DeleteLabelValues(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !t.DeletionTimestamp.IsZero() || !r.namespaceAllowed(t.Namespace) {
		return ctrl.Result{}, nil
	}

	ev, err := r.evaluate(ctx, t)
	if err != nil {
		return ctrl.Result{}, err
	}

	now := r.now()
	requeue := TriggerRecheckInterval
	var firedSlot *time.Time
	var next *time.Time
	if ev.ready() && ev.schedule != nil && hooks.Enabled(t) {
		slot, err := r.fireDue(ctx, t, ev, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !slot.IsZero() {
			firedSlot = &slot
		}
		if n := ev.schedule.Next(now); !n.IsZero() {
			next = &n
			requeue = min(requeue, n.Sub(now))
		}
	}

	generation := t.Generation
	updated, err := r.updateStatus(ctx, client.ObjectKeyFromObject(t), func(cur *ompv1alpha1.OmpTrigger) {
		applyTriggerStatus(cur, ev, generation, firedSlot, next)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if updated != nil && t.Spec.Webhook != nil {
		metrics.TriggerGroupsActive.WithLabelValues(t.Namespace, t.Name).Set(float64(hooks.ActiveGroups(updated.Status.Groups)))
	} else {
		metrics.TriggerGroupsActive.DeleteLabelValues(t.Namespace, t.Name)
	}
	if !ev.ready() {
		logf.FromContext(ctx).V(1).Info("trigger not ready", "reason", ev.reason, "message", ev.message)
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// applyTriggerStatus writes the reconciler-owned status fields of cur.
func applyTriggerStatus(cur *ompv1alpha1.OmpTrigger, ev triggerEvaluation, generation int64, firedSlot, next *time.Time) {
	cur.Status.Type = ""
	cur.Status.HookPath = ""
	switch {
	case cur.Spec.Webhook != nil && cur.Spec.Schedule == nil:
		cur.Status.Type = ompv1alpha1.TriggerWebhook
		cur.Status.HookPath = hooks.PathPrefix + cur.Namespace + "/" + cur.Name
	case cur.Spec.Schedule != nil && cur.Spec.Webhook == nil:
		cur.Status.Type = ompv1alpha1.TriggerSchedule
	}
	cond := metav1.Condition{
		Type:               ompv1alpha1.ConditionReady,
		ObservedGeneration: generation,
	}
	if ev.ready() {
		cond.Status, cond.Reason = metav1.ConditionTrue, triggerReasonReady
	} else {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, ev.reason, ev.message
	}
	meta.SetStatusCondition(&cur.Status.Conditions, cond)
	if firedSlot != nil {
		cur.Status.LastFireAt = &metav1.Time{Time: *firedSlot}
	}
	cur.Status.NextFireAt = nil
	if next != nil {
		cur.Status.NextFireAt = &metav1.Time{Time: *next}
	}
}

// updateStatus reads the trigger fresh, applies mutate and patches the status
// with an optimistic lock, retrying on conflict. It returns the stored object,
// or nil when the trigger is gone.
func (r *OmpTriggerReconciler) updateStatus(ctx context.Context, key client.ObjectKey, mutate func(*ompv1alpha1.OmpTrigger)) (*ompv1alpha1.OmpTrigger, error) {
	var out *ompv1alpha1.OmpTrigger
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &ompv1alpha1.OmpTrigger{}
		if err := r.reader().Get(ctx, key, cur); err != nil {
			return err
		}
		orig := cur.DeepCopy()
		mutate(cur)
		out = cur
		if apiequality.Semantic.DeepEqual(orig.Status, cur.Status) {
			return nil
		}
		return r.Status().Patch(ctx, cur, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
	})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return out, err
}

func (r *OmpTriggerReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// evaluate runs the Ready checks in order. A non-nil error is a transient
// read failure, not a spec problem.
func (r *OmpTriggerReconciler) evaluate(ctx context.Context, t *ompv1alpha1.OmpTrigger) (triggerEvaluation, error) {
	fail := func(reason, format string, args ...any) (triggerEvaluation, error) {
		return triggerEvaluation{reason: reason, message: fmt.Sprintf(format, args...)}, nil
	}

	agent := &ompv1alpha1.OmpAgent{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Spec.AgentRef.Name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return fail(triggerReasonAgentNotFound, "OmpAgent %q not found", t.Spec.AgentRef.Name)
		}
		return triggerEvaluation{}, err
	}
	ev := triggerEvaluation{agent: agent}

	spec := &t.Spec
	if (spec.Webhook == nil) == (spec.Schedule == nil) {
		return fail(triggerReasonInvalidSpec, "exactly one of webhook or schedule must be set")
	}

	if w := spec.Webhook; w != nil {
		set := 0
		for _, b := range []bool{w.Auth.Bearer != nil, w.Auth.Basic != nil, w.Auth.HMAC != nil} {
			if b {
				set++
			}
		}
		if set != 1 {
			return fail(triggerReasonInvalidSpec, "webhook.auth: exactly one of bearer, basic or hmac must be set")
		}
		if w.Adapter == ompv1alpha1.AdapterGeneric && (w.Mapping == nil || w.Mapping.Key == "") {
			return fail(triggerReasonInvalidSpec, "webhook.mapping.key is required for the generic adapter")
		}
		if _, err := adapters.Resolve(string(w.Adapter), hooks.ExpressionsOf(t), r.compiler(t)); err != nil {
			if errors.Is(err, adapters.ErrConfig) {
				return fail(triggerReasonInvalidSpec, "%v", err)
			}
			return fail(triggerReasonInvalidExpression, "%v", err)
		}
	}

	if err := triggerCheckPrompt(t); err != nil {
		return fail(triggerReasonInvalidPrompt, "spec.prompt: %v", err)
	}

	if spec.Webhook != nil {
		reason, msg, err := r.checkAuthSecret(ctx, t)
		if err != nil {
			return triggerEvaluation{}, err
		}
		if reason != "" {
			return fail(reason, "%s", msg)
		}
	}

	if s := spec.Schedule; s != nil {
		parsed, err := schedule.Parse(s.Cron, s.TimeZone)
		if err != nil {
			return fail(triggerReasonInvalidSchedule, "spec.schedule: %v", err)
		}
		ev.schedule = parsed
	}
	if spec.Schedule != nil && spec.Session.Resume == ompv1alpha1.ResumeEpisode {
		return fail(triggerReasonInvalidSpec, "session.resume: episode applies to webhook triggers only")
	}
	return ev, nil
}

// triggerCheckPrompt parses the template and renders it with sample data for
// the trigger's type, so a reference to a field that does not exist is caught
// before an event arrives. A reference to a label or other map key that is
// absent from the sample is accepted: whether it exists depends on the event.
func triggerCheckPrompt(t *ompv1alpha1.OmpTrigger) error {
	var data any
	if t.Spec.Webhook != nil {
		data = prompt.WebhookData{
			Agent:   t.Spec.AgentRef.Name,
			Trigger: t.Name,
			Session: t.Name + "-sample",
			Event: prompt.Event{
				Name: "SampleAlert/default", Key: "sample", Severity: "warning", Status: "firing",
				Labels: map[string]string{"alertname": "SampleAlert", "namespace": "default", "severity": "warning"},
			},
			Reason:     "new episode",
			AlertCount: 1,
			Previous:   prompt.NoPrevious,
			Alerts:     prompt.Fence("alerts", "[]"),
			Payload:    prompt.Fence("payload", "{}"),
		}
	} else {
		data = prompt.ScheduleData{
			Agent:   t.Spec.AgentRef.Name,
			Trigger: t.Name,
			Session: t.Name + "-sample",
			Input:   t.Spec.Schedule.Input,
			Now:     time.Now().UTC().Format(time.RFC3339),
		}
	}
	_, err := prompt.Render(t.Spec.Prompt, data)
	if err != nil && strings.Contains(err.Error(), "map has no entry for key") {
		return nil
	}
	return err
}

// checkAuthSecret verifies that the webhook's auth Secret and keys exist.
func (r *OmpTriggerReconciler) checkAuthSecret(ctx context.Context, t *ompv1alpha1.OmpTrigger) (reason, message string, err error) {
	a := t.Spec.Webhook.Auth
	var name string
	var keys []string
	switch {
	case a.Bearer != nil:
		name, keys = a.Bearer.SecretRef.Name, []string{a.Bearer.SecretRef.Key}
	case a.Basic != nil:
		name, keys = a.Basic.SecretRef.Name, []string{"username", "password"}
	case a.HMAC != nil:
		name, keys = a.HMAC.SecretRef.Name, []string{a.HMAC.SecretRef.Key}
	default:
		return triggerReasonInvalidSpec, "webhook.auth: exactly one of bearer, basic or hmac must be set", nil
	}
	s := &corev1.Secret{}
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return triggerReasonMissingSecret, fmt.Sprintf("Secret %q not found", name), nil
		}
		return "", "", err
	}
	for _, k := range keys {
		if len(s.Data[k]) == 0 {
			return triggerReasonMissingSecret, fmt.Sprintf("Secret %q has no value for key %q", name, k), nil
		}
	}
	return "", "", nil
}

// fireDue creates the session of the latest due slot, if any, and returns the
// slot (zero when nothing was due). A session that already exists counts as done.
func (r *OmpTriggerReconciler) fireDue(ctx context.Context, t *ompv1alpha1.OmpTrigger, ev triggerEvaluation, now time.Time) (time.Time, error) {
	base := t.CreationTimestamp.Time
	if t.Status.LastFireAt != nil {
		base = t.Status.LastFireAt.Time
	}
	slot := ev.schedule.Due(base, now)
	if slot.IsZero() {
		return time.Time{}, nil
	}
	name := t.Name + "-" + slot.UTC().Format("20060102-1504")
	text, err := prompt.Render(t.Spec.Prompt, prompt.ScheduleData{
		Agent:   ev.agent.Name,
		Trigger: t.Name,
		Session: name,
		Input:   t.Spec.Schedule.Input,
		Now:     now.In(ev.schedule.Location()).Format(time.RFC3339),
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("render prompt: %w", err)
	}
	priority := DefaultSchedulePriority
	if t.Spec.Priority != nil {
		priority = *t.Spec.Priority
	}
	labels := map[string]string{
		ompv1alpha1.LabelAgent:   ev.agent.Name,
		ompv1alpha1.LabelTrigger: t.Name,
		ompv1alpha1.LabelKind:    string(ompv1alpha1.SessionKindSchedule),
	}
	continuityKey := ""
	if t.Spec.Session.Resume == ompv1alpha1.ResumeKey {
		continuityKey = "schedule/" + t.Name
		labels[ompv1alpha1.LabelContinuity] = store.ContinuityID(continuityKey)
	}
	session := &ompv1alpha1.OmpSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: t.Namespace, Name: name, Labels: labels},
		Spec: ompv1alpha1.OmpSessionSpec{
			AgentRef:      ompv1alpha1.LocalObjectRef{Name: ev.agent.Name},
			TriggerRef:    &ompv1alpha1.LocalObjectRef{Name: t.Name},
			Prompt:        text,
			Priority:      &priority,
			ContinuityKey: continuityKey,
		},
	}
	if err := controllerutil.SetOwnerReference(ev.agent, session, r.Scheme); err != nil {
		return time.Time{}, err
	}
	if err := controllerutil.SetOwnerReference(t, session, r.Scheme); err != nil {
		return time.Time{}, err
	}
	if err := r.Create(ctx, session); err != nil && !apierrors.IsAlreadyExists(err) {
		return time.Time{}, err
	}
	return slot, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OmpTriggerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&ompv1alpha1.OmpTrigger{}).
		Watches(&ompv1alpha1.OmpAgent{}, handler.EnqueueRequestsFromMapFunc(r.triggersOfAgent)).
		Named("omptrigger").
		Complete(r)
}

// triggersOfAgent maps an agent event to the triggers that reference it.
func (r *OmpTriggerReconciler) triggersOfAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &ompv1alpha1.OmpTriggerList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "listing triggers of agent", "agent", obj.GetName())
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.AgentRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return out
}
