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
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentspec"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/gateway"
	"github.com/TechDufus/omp-operator/internal/store"
)

// agentRequeue is how often an agent is re-checked, since its Secrets, ConfigMaps,
// Flux sources and actions are not watched.
const agentRequeue = 5 * time.Minute

// Ready reasons set by the reconciler itself; spec problems carry agentspec's reasons.
const (
	agentReasonReady               = "Ready"
	agentReasonNamespaceNotAllowed = "NamespaceNotAllowed"
	agentReasonNoImage             = "NoImage"
	agentReasonMissingSecret       = "MissingSecret"
	agentReasonMissingConfigMap    = "MissingConfigMap"
	agentReasonMissingSource       = "MissingSource"
	agentReasonActionNotReady      = "ActionNotReady"
	agentReasonNoGatewayToken      = "NoGatewayToken"
)

// OmpAgentReconciler reconciles an OmpAgent: it decides Ready, keeps the managed
// gateway token and the gateway clients Secret current, and cleans up stored data
// when the agent is deleted.
//
// It writes status.observedGeneration and the Ready condition and nothing else;
// status.toolchain, ToolchainReady and status.notices belong to other writers.
type OmpAgentReconciler struct {
	// Client is the manager's (cached) client; it performs every write.
	client.Client
	// Scheme must know the omp types, core/v1 and Flux source-controller v1.
	Scheme *runtime.Scheme
	// APIReader reads Secrets, ConfigMaps, Flux sources, OmpActions and the fresh agent
	// without the cache. Defaults to Client.
	APIReader client.Reader
	// Options are the operator-wide settings.
	Options *config.Options
	// Store holds the agent's toolchains and continuity, removed on deletion.
	Store *store.Store
	// Gateway owns managed gateway tokens and the clients Secret. May be nil only when
	// Options.GatewayManaged() is false.
	Gateway *gateway.Manager
}

// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompagents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompagents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompagents/finalizers,verbs=update

func (r *OmpAgentReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// Reconcile evaluates the agent and records Ready.
func (r *OmpAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var agent ompv1alpha1.OmpAgent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			// Gone without a finalizer pass (it never got one): make sure no clients entry lingers.
			return ctrl.Result{}, r.syncGateway(ctx)
		}
		return ctrl.Result{}, err
	}
	if !agent.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &agent)
	}

	allowed := r.Options.NamespaceAllowed(agent.Namespace)
	if allowed && !controllerutil.ContainsFinalizer(&agent, ompv1alpha1.FinalizerCleanup) {
		if err := r.setFinalizer(ctx, req.NamespacedName, true); err != nil {
			return ctrl.Result{}, err
		}
	}

	p, err := r.check(ctx, &agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	cond := metav1.Condition{
		Type: ompv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: agentReasonReady,
		Message: "agent is ready", ObservedGeneration: agent.Generation,
	}
	if p != nil {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, p.Reason, p.Message
		log.V(1).Info("agent not ready", "reason", p.Reason, "message", p.Message)
	}
	if err := r.writeStatus(ctx, req.NamespacedName, agent.Generation, cond); err != nil {
		return ctrl.Result{}, err
	}
	if allowed {
		if err := r.syncGateway(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: agentRequeue}, nil
}

func (r *OmpAgentReconciler) syncGateway(ctx context.Context) error {
	if !r.Options.GatewayManaged() {
		return nil
	}
	if r.Gateway == nil {
		return errors.New("managed gateway tokens are enabled but the reconciler has no gateway manager")
	}
	return r.Gateway.Sync(ctx)
}

// finalize removes what the operator holds for a deleted agent, then the finalizer.
func (r *OmpAgentReconciler) finalize(ctx context.Context, agent *ompv1alpha1.OmpAgent) error {
	if !controllerutil.ContainsFinalizer(agent, ompv1alpha1.FinalizerCleanup) {
		return nil
	}
	// Sync leaves terminating agents out of the clients file.
	if err := r.syncGateway(ctx); err != nil {
		return err
	}
	if r.Store != nil {
		if err := r.Store.RemoveAgent(agent.Namespace, agent.Name); err != nil {
			return fmt.Errorf("remove stored data: %w", err)
		}
	}
	return r.setFinalizer(ctx, client.ObjectKeyFromObject(agent), false)
}

// setFinalizer adds or removes the cleanup finalizer on a fresh copy, with an optimistic lock.
func (r *OmpAgentReconciler) setFinalizer(ctx context.Context, key types.NamespacedName, add bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur ompv1alpha1.OmpAgent
		if err := r.reader().Get(ctx, key, &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		orig := cur.DeepCopy()
		var changed bool
		if add {
			changed = controllerutil.AddFinalizer(&cur, ompv1alpha1.FinalizerCleanup)
		} else {
			changed = controllerutil.RemoveFinalizer(&cur, ompv1alpha1.FinalizerCleanup)
		}
		if !changed {
			return nil
		}
		return client.IgnoreNotFound(r.Patch(ctx, &cur, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})))
	})
}

// writeStatus records observedGeneration and the Ready condition on a fresh copy.
func (r *OmpAgentReconciler) writeStatus(ctx context.Context, key types.NamespacedName, generation int64, cond metav1.Condition) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur ompv1alpha1.OmpAgent
		if err := r.reader().Get(ctx, key, &cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		orig := cur.DeepCopy()
		changed := meta.SetStatusCondition(&cur.Status.Conditions, cond)
		if cur.Status.ObservedGeneration != generation {
			cur.Status.ObservedGeneration = generation
			changed = true
		}
		if !changed {
			return nil
		}
		return client.IgnoreNotFound(r.Status().Patch(ctx, &cur, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})))
	})
}

// check returns why the agent is not Ready, or nil. The error is for failed reads.
func (r *OmpAgentReconciler) check(ctx context.Context, a *ompv1alpha1.OmpAgent) (*agentspec.Problem, error) {
	if !r.Options.NamespaceAllowed(a.Namespace) {
		return &agentspec.Problem{Reason: agentReasonNamespaceNotAllowed,
			Message: fmt.Sprintf("namespace %q is not served by this operator (--agent-namespaces)", a.Namespace)}, nil
	}
	if a.Spec.Image == "" && r.Options.DefaultRuntimeImage == "" {
		return &agentspec.Problem{Reason: agentReasonNoImage,
			Message: "spec.image is empty and the operator has no --default-runtime-image"}, nil
	}
	if p := agentspec.Validate(a); p != nil {
		return p, nil
	}
	for _, step := range []func(context.Context, *ompv1alpha1.OmpAgent) (*agentspec.Problem, error){
		r.checkSecrets, r.checkConfigMaps, r.checkSources, r.checkActions, r.checkGatewayToken,
	} {
		if p, err := step(ctx, a); p != nil || err != nil {
			return p, err
		}
	}
	return nil, nil
}

// checkSecrets requires every referenced Secret to exist and hold the referenced key.
func (r *OmpAgentReconciler) checkSecrets(ctx context.Context, a *ompv1alpha1.OmpAgent) (*agentspec.Problem, error) {
	secrets := map[string]*corev1.Secret{}
	for _, use := range agentspec.SecretRefs(&a.Spec) {
		s, seen := secrets[use.Ref.Name]
		if !seen {
			var got corev1.Secret
			err := r.reader().Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: use.Ref.Name}, &got)
			switch {
			case apierrors.IsNotFound(err):
			case err != nil:
				return nil, fmt.Errorf("read Secret %q: %w", use.Ref.Name, err)
			default:
				s = &got
			}
			secrets[use.Ref.Name] = s
		}
		if s == nil {
			return &agentspec.Problem{Reason: agentReasonMissingSecret,
				Message: fmt.Sprintf("%s: Secret %q not found", use.What, use.Ref.Name)}, nil
		}
		if _, ok := s.Data[use.Ref.Key]; !ok {
			return &agentspec.Problem{Reason: agentReasonMissingSecret,
				Message: fmt.Sprintf("%s: Secret %q has no key %q", use.What, use.Ref.Name, use.Ref.Key)}, nil
		}
	}
	return nil, nil
}

// checkConfigMaps requires the ConfigMaps of ConfigMap skill sources to exist.
func (r *OmpAgentReconciler) checkConfigMaps(ctx context.Context, a *ompv1alpha1.OmpAgent) (*agentspec.Problem, error) {
	checked := map[string]bool{}
	for i, s := range a.Spec.Skills {
		if s.ConfigMap == nil || checked[s.ConfigMap.Name] {
			continue
		}
		checked[s.ConfigMap.Name] = true
		var cm corev1.ConfigMap
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: s.ConfigMap.Name}, &cm)
		if apierrors.IsNotFound(err) {
			return &agentspec.Problem{Reason: agentReasonMissingConfigMap,
				Message: fmt.Sprintf("skills[%d]: ConfigMap %q not found", i, s.ConfigMap.Name)}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read ConfigMap %q: %w", s.ConfigMap.Name, err)
		}
	}
	return nil, nil
}

// checkSources requires the Flux sources of skills and the workspace to exist.
func (r *OmpAgentReconciler) checkSources(ctx context.Context, a *ompv1alpha1.OmpAgent) (*agentspec.Problem, error) {
	var refs []ompv1alpha1.FluxSourceRef
	for _, s := range a.Spec.Skills {
		if s.FluxSource != nil {
			refs = append(refs, s.FluxSource.FluxSourceRef)
		}
	}
	if w := a.Spec.Workspace; w != nil && w.FluxSource != nil {
		refs = append(refs, *w.FluxSource)
	}
	checked := map[ompv1alpha1.FluxSourceRef]bool{}
	for _, ref := range refs {
		if checked[ref] {
			continue
		}
		checked[ref] = true
		var obj client.Object = &sourcev1.GitRepository{}
		if ref.Kind == ompv1alpha1.FluxSourceOCIRepository {
			obj = &sourcev1.OCIRepository{}
		}
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, obj)
		switch {
		case apierrors.IsNotFound(err):
			return &agentspec.Problem{Reason: agentReasonMissingSource,
				Message: fmt.Sprintf("Flux %s %s/%s not found", ref.Kind, ref.Namespace, ref.Name)}, nil
		case meta.IsNoMatchError(err):
			return &agentspec.Problem{Reason: agentReasonMissingSource,
				Message: fmt.Sprintf("Flux %s is not installed in the cluster", ref.Kind)}, nil
		case err != nil:
			return nil, fmt.Errorf("read Flux %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
		}
	}
	return nil, nil
}

// checkActions requires every listed OmpAction to exist and be Ready.
func (r *OmpAgentReconciler) checkActions(ctx context.Context, a *ompv1alpha1.OmpAgent) (*agentspec.Problem, error) {
	for _, ref := range a.Spec.Actions {
		var act ompv1alpha1.OmpAction
		err := r.reader().Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: ref.Name}, &act)
		if apierrors.IsNotFound(err) {
			return &agentspec.Problem{Reason: agentReasonActionNotReady,
				Message: fmt.Sprintf("action %q not found", ref.Name)}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read OmpAction %q: %w", ref.Name, err)
		}
		c := meta.FindStatusCondition(act.Status.Conditions, ompv1alpha1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionTrue {
			msg := "has no Ready condition yet"
			if c != nil {
				msg = fmt.Sprintf("is not Ready: %s: %s", c.Reason, c.Message)
			}
			return &agentspec.Problem{Reason: agentReasonActionNotReady,
				Message: fmt.Sprintf("action %q %s", ref.Name, msg)}, nil
		}
	}
	return nil, nil
}

// checkGatewayToken makes sure a gateway has a token: the user's Secret (already
// checked by checkSecrets) or, with managed tokens enabled, the operator's.
func (r *OmpAgentReconciler) checkGatewayToken(ctx context.Context, a *ompv1alpha1.OmpAgent) (*agentspec.Problem, error) {
	gw := a.Spec.Model.Gateway
	if gw == nil || gw.TokenSecretRef != nil {
		return nil, nil
	}
	if !r.Options.GatewayManaged() {
		return &agentspec.Problem{Reason: agentReasonNoGatewayToken,
			Message: "model.gateway.tokenSecretRef is not set and managed gateway tokens are disabled (--gateway-clients-secret)"}, nil
	}
	if r.Gateway == nil {
		return nil, errors.New("managed gateway tokens are enabled but the reconciler has no gateway manager")
	}
	if err := r.Gateway.EnsureToken(ctx, a); err != nil {
		return &agentspec.Problem{Reason: agentReasonNoGatewayToken,
			Message: fmt.Sprintf("cannot create the managed gateway token: %v", err)}, nil
	}
	return nil, nil
}

// agentsForAction maps an OmpAction to the agents in its namespace that list it.
func (r *OmpAgentReconciler) agentsForAction(ctx context.Context, obj client.Object) []reconcile.Request {
	var agents ompv1alpha1.OmpAgentList
	if err := r.List(ctx, &agents, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "list agents for action", "action", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range agents.Items {
		for _, ref := range agents.Items[i].Spec.Actions {
			if ref.Name == obj.GetName() {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&agents.Items[i])})
				break
			}
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *OmpAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ompv1alpha1.OmpAgent{}).
		Watches(&ompv1alpha1.OmpAction{}, handler.EnqueueRequestsFromMapFunc(r.agentsForAction)).
		Named("ompagent").
		Complete(r)
}
