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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/actions"
	"github.com/TechDufus/omp-operator/internal/config"
)

// actionRecheckInterval is how often an OmpAction is re-validated, so that a Secret created
// or removed later is noticed.
const actionRecheckInterval = 5 * time.Minute

// OmpActionReconciler reconciles a OmpAction object. Options may be nil in tests; then
// every namespace is served. Secrets are read through Client, which the manager configures
// to bypass its cache for Secrets.
type OmpActionReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Options *config.Options
}

// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompactions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompactions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=omp.techdufus.com,resources=ompactions/finalizers,verbs=update
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories;ocirepositories;helmrepositories,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;patch
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile validates an OmpAction and reports the result as its Ready condition and
// status.type. The reasons are InvalidName, ReservedName, InvalidSpec and MissingSecret.
func (r *OmpActionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var act ompv1alpha1.OmpAction
	if err := r.Get(ctx, req.NamespacedName, &act); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !act.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if r.Options != nil && !r.Options.NamespaceAllowed(act.Namespace) {
		return ctrl.Result{}, nil
	}

	typ, _ := actions.Type(&act.Spec)
	status, reason, message, err := r.evaluate(ctx, &act)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.writeStatus(ctx, req.NamespacedName, typ, status, reason, message); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{RequeueAfter: actionRecheckInterval}, nil
}

// evaluate returns the Ready condition of an action. err is set only for failures that say
// nothing about the action (a Secret read failing for a reason other than not-found).
func (r *OmpActionReconciler) evaluate(ctx context.Context, act *ompv1alpha1.OmpAction) (metav1.ConditionStatus, string, string, error) {
	if reason, ok := actions.ValidateName(act.Name); !ok {
		if reason == actions.ReasonInvalidName {
			return metav1.ConditionFalse, reason, fmt.Sprintf("name %q must match ^[a-z][a-z0-9-]{0,62}$", act.Name), nil
		}
		return metav1.ConditionFalse, reason, fmt.Sprintf("tool name %q is reserved by omp or the operator", actions.ToolName(act.Name)), nil
	}
	if err := actions.ValidateSpec(&act.Spec); err != nil {
		return metav1.ConditionFalse, actions.ReasonInvalidSpec, err.Error(), nil
	}
	refs := secretRefs(&act.Spec)
	for _, ref := range refs {
		var sec corev1.Secret
		err := r.Get(ctx, client.ObjectKey{Namespace: act.Namespace, Name: ref.Name}, &sec)
		switch {
		case apierrors.IsNotFound(err):
			return metav1.ConditionFalse, actions.ReasonMissingSecret, fmt.Sprintf("secret %q not found", ref.Name), nil
		case err != nil:
			return "", "", "", err
		}
		if _, ok := sec.Data[ref.Key]; !ok {
			return metav1.ConditionFalse, actions.ReasonMissingSecret, fmt.Sprintf("secret %q has no key %q", ref.Name, ref.Key), nil
		}
	}
	return metav1.ConditionTrue, "Ready", "action is ready", nil
}

// secretRefs lists the Secret keys an action reads at run time.
func secretRefs(s *ompv1alpha1.OmpActionSpec) []ompv1alpha1.SecretKeyRef {
	var refs []ompv1alpha1.SecretKeyRef
	if s.AlertmanagerSilence != nil && s.AlertmanagerSilence.AuthSecretRef != nil {
		refs = append(refs, *s.AlertmanagerSilence.AuthSecretRef)
	}
	if s.HTTPRequest != nil {
		for _, h := range s.HTTPRequest.Headers {
			if h.SecretRef != nil {
				refs = append(refs, *h.SecretRef)
			}
		}
	}
	return refs
}

// writeStatus reads the action fresh and patches status.type and the Ready condition.
func (r *OmpActionReconciler) writeStatus(ctx context.Context, key client.ObjectKey, typ ompv1alpha1.ActionType, status metav1.ConditionStatus, reason, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur ompv1alpha1.OmpAction
		if err := r.Get(ctx, key, &cur); err != nil {
			return err
		}
		base := cur.DeepCopy()
		cur.Status.Type = typ
		meta.SetStatusCondition(&cur.Status.Conditions, metav1.Condition{
			Type:               ompv1alpha1.ConditionReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: cur.Generation,
		})
		if equalStatus(&base.Status, &cur.Status) {
			return nil
		}
		return r.Status().Patch(ctx, &cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

func equalStatus(a, b *ompv1alpha1.OmpActionStatus) bool {
	if a.Type != b.Type || len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		if a.Conditions[i] != b.Conditions[i] {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *OmpActionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ompv1alpha1.OmpAction{}).
		Named("ompaction").
		Complete(r)
}
