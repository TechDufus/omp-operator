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

//nolint:goconst // test fixtures repeat literals on purpose
package actions

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

func fluxAction(mutate func(*v1alpha1.OmpActionSpec)) *v1alpha1.OmpAction {
	return newAction("flux-sync", func(s *v1alpha1.OmpActionSpec) {
		s.FluxReconcile = &v1alpha1.FluxReconcileAction{
			WaitTimeout: metav1.Duration{Duration: 200 * time.Millisecond},
		}
		if mutate != nil {
			mutate(s)
		}
	})
}

// fluxObject builds a Flux object. handled is the lastHandledReconcileAt Flux has already
// recorded; tests pre-set it to the fixed request time, standing in for the controller.
func fluxObject(ref fluxRef, spec map[string]any, handled bool, ready, message string) *unstructured.Unstructured {
	u := ref.object()
	if spec == nil {
		spec = map[string]any{}
	}
	status := map[string]any{
		"conditions": []any{map[string]any{"type": "Ready", "status": ready, "message": message}},
	}
	if handled {
		status["lastHandledReconcileAt"] = fixedNow.Format(time.RFC3339Nano)
	}
	u.Object["spec"] = spec
	u.Object["status"] = status
	return u
}

type fluxReq struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	WithSource bool   `json:"with_source,omitempty"`
	Reason     string `json:"reason"`
}

func annotation(t *testing.T, c client.Client, ref fluxRef) string {
	t.Helper()
	u := ref.object()
	if err := c.Get(context.Background(), ref.key(), u); err != nil {
		t.Fatal(err)
	}
	return u.GetAnnotations()[reconcileRequestedAtAnnotation]
}

func TestFluxReconcile(t *testing.T) {
	ks := fluxRef{v1alpha1.FluxKustomization, testNS, "apps"}
	r, c := newRunner(t, nil, newSession(testSession), fluxAction(nil),
		fluxObject(ks, nil, true, "True", "Applied revision main@sha1:abc"))
	res := call(t, r, newAgent("flux-sync"), "flux_sync", fluxReq{Kind: "Kustomization", Namespace: testNS, Name: "apps", Reason: "drift"})
	want := "Kustomization/apps/apps reconciled; Ready=True: Applied revision main@sha1:abc"
	if res.IsError || res.Text != want {
		t.Fatalf("result = %+v, want %q", res, want)
	}
	if got := annotation(t, c, ks); got != fixedNow.Format(time.RFC3339Nano) {
		t.Fatalf("requestedAt = %q", got)
	}
	recs := getSession(t, c).Status.Actions
	if len(recs) != 1 || recs[0].Target != "Kustomization/apps/apps" || !recs[0].OK {
		t.Fatalf("status.actions = %+v", recs)
	}
}

func TestFluxReconcileRefusesSuspended(t *testing.T) {
	ks := fluxRef{v1alpha1.FluxKustomization, testNS, "apps"}
	r, c := newRunner(t, nil, newSession(testSession), fluxAction(nil),
		fluxObject(ks, map[string]any{"suspend": true}, false, "True", ""))
	res := call(t, r, newAgent("flux-sync"), "flux_sync", fluxReq{Kind: "Kustomization", Namespace: testNS, Name: "apps", Reason: "drift"})
	if !res.IsError || !strings.Contains(res.Text, "suspended; resuming is not yours") {
		t.Fatalf("result = %+v", res)
	}
	if got := annotation(t, c, ks); got != "" {
		t.Fatalf("suspended object was annotated: %q", got)
	}
}

func TestFluxReconcileNotHandled(t *testing.T) {
	ks := fluxRef{v1alpha1.FluxKustomization, testNS, "apps"}
	r, _ := newRunner(t, nil, newSession(testSession), fluxAction(nil), fluxObject(ks, nil, false, "True", ""))
	res := call(t, r, newAgent("flux-sync"), "flux_sync", fluxReq{Kind: "Kustomization", Namespace: testNS, Name: "apps", Reason: "drift"})
	if res.IsError || !strings.Contains(res.Text, "requested; not handled after 200ms") {
		t.Fatalf("result = %+v", res)
	}
}

func TestFluxReconcileRefusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1alpha1.OmpActionSpec)
		req    fluxReq
		want   string
	}{
		{"kind not allowed by default", nil, fluxReq{Kind: "HelmRepository", Namespace: testNS, Name: "x", Reason: "r"}, `kind "HelmRepository" is not allowed`},
		{"namespace not allowed", nil, fluxReq{Kind: "Kustomization", Namespace: "other", Name: "x", Reason: "r"}, `namespace "other" is not allowed`},
		{"operator namespace", func(s *v1alpha1.OmpActionSpec) {
			s.FluxReconcile.Namespaces.Allow = []string{"*"}
		}, fluxReq{Kind: "Kustomization", Namespace: testOperNS, Name: "x", Reason: "r"}, "is not allowed"},
		{"missing reason", nil, fluxReq{Kind: "Kustomization", Namespace: testNS, Name: "x"}, "reason must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newRunner(t, []string{"*"}, newSession(testSession), fluxAction(tt.mutate))
			res := call(t, r, newAgent("flux-sync"), "flux_sync", tt.req)
			if !res.IsError || !strings.HasPrefix(res.Text, "refused: ") || !strings.Contains(res.Text, tt.want) {
				t.Fatalf("result = %+v, want refusal containing %q", res, tt.want)
			}
		})
	}
}

func TestFluxReconcileWithSource(t *testing.T) {
	ks := fluxRef{v1alpha1.FluxKustomization, testNS, "apps"}
	src := fluxRef{v1alpha1.FluxGitRepository, testNS, "flux-system"}
	hr := fluxRef{v1alpha1.FluxHelmRelease, testNS, "web"}
	oci := fluxRef{v1alpha1.FluxOCIRepository, testNS, "chart"}
	r, c := newRunner(t, nil, newSession(testSession), fluxAction(nil),
		fluxObject(ks, map[string]any{"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"}}, true, "True", "ok"),
		fluxObject(src, nil, true, "True", "stored artifact"),
		fluxObject(hr, map[string]any{"chartRef": map[string]any{"kind": "OCIRepository", "name": "chart"}}, true, "False", "install failed"),
		fluxObject(oci, nil, true, "True", "fetched"),
	)
	agent := newAgent("flux-sync")

	res := call(t, r, agent, "flux_sync", fluxReq{Kind: "Kustomization", Namespace: testNS, Name: "apps", WithSource: true, Reason: "r"})
	want := "source GitRepository/apps/flux-system reconciled; Ready=True: stored artifact; Kustomization/apps/apps reconciled; Ready=True: ok"
	if res.IsError || res.Text != want {
		t.Fatalf("result = %q, want %q", res.Text, want)
	}
	if annotation(t, c, src) == "" {
		t.Fatal("source was not annotated")
	}

	res = call(t, r, agent, "flux_sync", fluxReq{Kind: "HelmRelease", Namespace: testNS, Name: "web", WithSource: true, Reason: "r"})
	if res.IsError || !strings.Contains(res.Text, "source OCIRepository/apps/chart reconciled") || !strings.HasSuffix(res.Text, "Ready=False: install failed") {
		t.Fatalf("helmrelease result = %q", res.Text)
	}
}

func TestFluxReconcileSourceNotAllowed(t *testing.T) {
	ks := fluxRef{v1alpha1.FluxKustomization, testNS, "apps"}
	src := fluxRef{v1alpha1.FluxGitRepository, "other", "shared"}
	act := fluxAction(func(s *v1alpha1.OmpActionSpec) {
		s.FluxReconcile.Kinds = []v1alpha1.FluxKind{v1alpha1.FluxKustomization, v1alpha1.FluxGitRepository}
	})
	r, c := newRunner(t, nil, newSession(testSession), act,
		fluxObject(ks, map[string]any{"sourceRef": map[string]any{"kind": "GitRepository", "name": "shared", "namespace": "other"}}, true, "True", "ok"),
		fluxObject(src, nil, true, "True", ""),
	)
	res := call(t, r, newAgent("flux-sync"), "flux_sync", fluxReq{Kind: "Kustomization", Namespace: testNS, Name: "apps", WithSource: true, Reason: "r"})
	if res.IsError || !strings.HasPrefix(res.Text, "source GitRepository/other/shared not reconciled: namespace not allowed") {
		t.Fatalf("result = %q", res.Text)
	}
	if annotation(t, c, src) != "" {
		t.Fatal("source in a namespace the rule denies was annotated")
	}
}

func TestSourceRefDefaults(t *testing.T) {
	ks := fluxObject(fluxRef{v1alpha1.FluxKustomization, "a", "k"}, map[string]any{
		"sourceRef": map[string]any{"kind": "Bucket", "name": "b"},
	}, false, "True", "")
	if _, ok := sourceRef(ks); ok {
		t.Fatal("a Bucket source is not reconcilable by this action")
	}
	hr := fluxObject(fluxRef{v1alpha1.FluxHelmRelease, "a", "h"}, map[string]any{
		"chart": map[string]any{"spec": map[string]any{"sourceRef": map[string]any{"kind": "HelmRepository", "name": "repo"}}},
	}, false, "True", "")
	got, ok := sourceRef(hr)
	if !ok || got != (fluxRef{v1alpha1.FluxHelmRepository, "a", "repo"}) {
		t.Fatalf("sourceRef = %+v, %v", got, ok)
	}
}
