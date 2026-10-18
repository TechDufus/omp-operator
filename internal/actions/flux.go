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
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

const (
	// reconcileRequestedAtAnnotation is what `flux reconcile` sets to trigger a reconcile.
	reconcileRequestedAtAnnotation = "reconcile.fluxcd.io/requestedAt"

	suspendedRefusal = "suspended; resuming is not yours"
)

// sourceGroup is the API group of the Flux source kinds.
const sourceGroup = "source.toolkit.fluxcd.io"

var fluxGVKs = map[v1alpha1.FluxKind]schema.GroupVersionKind{
	v1alpha1.FluxKustomization:  {Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"},
	v1alpha1.FluxHelmRelease:    {Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"},
	v1alpha1.FluxGitRepository:  {Group: sourceGroup, Version: "v1", Kind: "GitRepository"},
	v1alpha1.FluxOCIRepository:  {Group: sourceGroup, Version: "v1", Kind: "OCIRepository"},
	v1alpha1.FluxHelmRepository: {Group: sourceGroup, Version: "v1", Kind: "HelmRepository"},
}

type fluxArgs struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	WithSource bool   `json:"with_source"`
	Reason     string `json:"reason"`
}

// fluxRef names a Flux object.
type fluxRef struct {
	kind      v1alpha1.FluxKind
	namespace string
	name      string
}

func (f fluxRef) String() string { return fmt.Sprintf("%s/%s/%s", f.kind, f.namespace, f.name) }

func (f fluxRef) object() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(fluxGVKs[f.kind])
	u.SetNamespace(f.namespace)
	u.SetName(f.name)
	return u
}

func (f fluxRef) key() client.ObjectKey {
	return client.ObjectKey{Namespace: f.namespace, Name: f.name}
}

// fluxReconcile asks Flux to reconcile an object, and optionally its source first.
func (r *Runner) fluxReconcile(ctx context.Context, c callCtx, spec *v1alpha1.FluxReconcileAction, raw json.RawMessage) outcome {
	var a fluxArgs
	if err := decodeArgs(raw, &a); err != nil {
		return refusal("", "%v", err)
	}
	if err := checkText("reason", a.Reason, reasonMax); err != nil {
		return refusal("", "%v", err)
	}
	ref := fluxRef{kind: v1alpha1.FluxKind(a.Kind), namespace: a.Namespace, name: a.Name}
	target := ref.String()
	if !slices.Contains(fluxKinds(spec), ref.kind) {
		return refusal(target, "kind %q is not allowed; allowed kinds: %v", a.Kind, fluxKinds(spec))
	}
	if a.Name == "" || len(a.Name) > 253 {
		return refusal(target, "name is required")
	}
	if !TargetNamespaceAllowed(r.Options, c.action.Namespace, spec.Namespaces, a.Namespace) {
		return refusal(target, "namespace %q is not allowed for this action", a.Namespace)
	}

	obj := ref.object()
	if err := r.APIReader.Get(ctx, ref.key(), obj); err != nil {
		return failure(target, "reading %s: %v", target, err)
	}
	if isSuspended(obj) {
		return refusal(target, "%s is %s", target, suspendedRefusal)
	}

	wait := durationOr(spec.WaitTimeout, defaultWaitTimeout)
	wctx, cancel := r.waitContext(ctx, c, wait)
	defer cancel()

	var notes []string
	if a.WithSource {
		notes = append(notes, r.reconcileSource(wctx, c, spec, obj, wait))
	}
	res, err := r.reconcileObject(wctx, ref, obj)
	if err != nil {
		return failure(target, "reconciling %s: %v", target, err)
	}
	text := res.text(target, wait)
	if len(notes) > 0 && notes[0] != "" {
		text = notes[0] + "; " + text
	}
	return okOutcome(target, "%s", text)
}

// reconcileSource follows the object's source reference and reconciles the source when the
// action allows its kind and namespace. It returns a note for the agent.
func (r *Runner) reconcileSource(ctx context.Context, c callCtx, spec *v1alpha1.FluxReconcileAction, obj *unstructured.Unstructured, wait time.Duration) string {
	src, ok := sourceRef(obj)
	if !ok {
		return "no source reference to reconcile"
	}
	if !slices.Contains(fluxKinds(spec), src.kind) {
		return fmt.Sprintf("source %s not reconciled: kind not allowed by this action", src)
	}
	if !TargetNamespaceAllowed(r.Options, c.action.Namespace, spec.Namespaces, src.namespace) {
		return fmt.Sprintf("source %s not reconciled: namespace not allowed by this action", src)
	}
	sobj := src.object()
	if err := r.APIReader.Get(ctx, src.key(), sobj); err != nil {
		return fmt.Sprintf("source %s not reconciled: %v", src, err)
	}
	if isSuspended(sobj) {
		return fmt.Sprintf("source %s not reconciled: %s", src, suspendedRefusal)
	}
	res, err := r.reconcileObject(ctx, src, sobj)
	if err != nil {
		return fmt.Sprintf("source %s not reconciled: %v", src, err)
	}
	return "source " + res.text(src.String(), wait)
}

// reconcileResult is the outcome of one reconcile request.
type reconcileResult struct {
	handled bool
	status  string
	message string
}

func (rr reconcileResult) text(target string, wait time.Duration) string {
	if !rr.handled {
		return fmt.Sprintf("reconcile of %s requested; not handled after %s", target, wait)
	}
	s := fmt.Sprintf("%s reconciled; Ready=%s", target, rr.status)
	if rr.message != "" {
		s += ": " + rr.message
	}
	return s
}

// reconcileObject sets the reconcile annotation and polls until Flux handled it.
func (r *Runner) reconcileObject(ctx context.Context, ref fluxRef, obj *unstructured.Unstructured) (reconcileResult, error) {
	requestedAt := r.now().UTC().Format(time.RFC3339Nano)
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, reconcileRequestedAtAnnotation, requestedAt)
	if err := r.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		return reconcileResult{}, err
	}
	var last *unstructured.Unstructured
	timedOut, err := r.pollUntil(ctx, func(ctx context.Context) (bool, error) {
		cur := ref.object()
		if err := r.APIReader.Get(ctx, ref.key(), cur); err != nil {
			return false, err
		}
		last = cur
		handled, _, _ := unstructured.NestedString(cur.Object, "status", "lastHandledReconcileAt")
		return handled == requestedAt, nil
	})
	if err != nil {
		return reconcileResult{}, err
	}
	if timedOut || last == nil {
		return reconcileResult{}, nil
	}
	res := reconcileResult{handled: true, status: "Unknown"}
	conds, _, _ := unstructured.NestedSlice(last.Object, "status", "conditions")
	for _, ci := range conds {
		cm, ok := ci.(map[string]any)
		if !ok || cm["type"] != "Ready" {
			continue
		}
		if s, ok := cm["status"].(string); ok {
			res.status = s
		}
		res.message, _ = cm["message"].(string)
	}
	return res, nil
}

func isSuspended(obj *unstructured.Unstructured) bool {
	suspended, _, _ := unstructured.NestedBool(obj.Object, "spec", "suspend")
	return suspended
}

// sourceRef returns the source a Kustomization (spec.sourceRef) or HelmRelease
// (spec.chart.spec.sourceRef or spec.chartRef) points at, when it is a Flux source kind.
func sourceRef(obj *unstructured.Unstructured) (fluxRef, bool) {
	var m map[string]any
	switch obj.GetKind() {
	case "Kustomization":
		m, _, _ = unstructured.NestedMap(obj.Object, "spec", "sourceRef")
	case "HelmRelease":
		m, _, _ = unstructured.NestedMap(obj.Object, "spec", "chart", "spec", "sourceRef")
		if m == nil {
			m, _, _ = unstructured.NestedMap(obj.Object, "spec", "chartRef")
		}
	}
	if m == nil {
		return fluxRef{}, false
	}
	kind, _ := m["kind"].(string)
	name, _ := m["name"].(string)
	ns, _ := m["namespace"].(string)
	if ns == "" {
		ns = obj.GetNamespace()
	}
	fk := v1alpha1.FluxKind(kind)
	gvk, ok := fluxGVKs[fk]
	if !ok || name == "" || gvk.Group != sourceGroup {
		return fluxRef{}, false
	}
	return fluxRef{kind: fk, namespace: ns, name: name}, true
}
