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
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

func TestToolDefs(t *testing.T) {
	notReady := restartAction(nil)
	notReady.Name = "not-ready"
	notReady.Status.Conditions[0].Status = metav1.ConditionFalse
	invalid := newAction("invalid", func(s *v1alpha1.OmpActionSpec) {}) // no type set

	ha := httpAction("http://127.0.0.1:1", nil)
	r, _ := newRunner(t, nil, newSession(testSession), restartAction(nil), fluxAction(nil), ha, notReady, invalid,
		newAction("silence-it", func(s *v1alpha1.OmpActionSpec) {
			s.AlertmanagerSilence = &v1alpha1.AlertmanagerSilenceAction{URL: "http://am:9093"}
		}))
	agent := newAgent("restart-app", "flux-sync", "http-call", "not-ready", "invalid", "missing", "silence-it", "restart-app")

	defs, acts, err := r.ToolDefs(context.Background(), agent)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, ","); got != "restart_app,flux_sync,http_call,silence_it,finish" {
		t.Fatalf("tools = %s", got)
	}
	if len(acts) != 4 {
		t.Fatalf("resolved actions = %d, want 4", len(acts))
	}
	for _, d := range defs {
		var schema map[string]any
		if err := json.Unmarshal(d.Parameters, &schema); err != nil || schema["type"] != "object" {
			t.Fatalf("%s parameters = %s (%v)", d.Name, d.Parameters, err)
		}
		if d.Description == "" {
			t.Fatalf("%s has no description", d.Name)
		}
	}
	// The http tool exposes the action's own schema verbatim.
	var got, want any
	if err := json.Unmarshal(defs[2].Parameters, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(itemSchema), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("http parameters = %s", defs[2].Parameters)
	}
}

func TestToolDefsOnlyFinish(t *testing.T) {
	r, _ := newRunner(t, nil)
	defs, acts, err := r.ToolDefs(context.Background(), newAgent())
	if err != nil || len(defs) != 1 || defs[0].Name != "finish" || len(acts) != 0 {
		t.Fatalf("defs = %+v, acts = %d, err = %v", defs, len(acts), err)
	}
}

func TestRestartSchemaEnumFollowsKinds(t *testing.T) {
	spec := &restartAction(func(s *v1alpha1.OmpActionSpec) {
		s.KubernetesRolloutRestart.Kinds = []v1alpha1.WorkloadKind{"Deployment", "DaemonSet"}
	}).Spec
	raw, err := parametersFor(spec)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties struct {
			Kind struct {
				Enum []string `json:"enum"`
			} `json:"kind"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil || strings.Join(s.Properties.Kind.Enum, ",") != "Deployment,DaemonSet" {
		t.Fatalf("schema = %s (%v)", raw, err)
	}
}

func TestValidateSpecUnions(t *testing.T) {
	if err := ValidateSpec(&v1alpha1.OmpActionSpec{Description: "d"}); err == nil {
		t.Fatal("no type must be invalid")
	}
	both := &v1alpha1.OmpActionSpec{
		Description:              "d",
		KubernetesRolloutRestart: &v1alpha1.KubernetesRolloutRestartAction{},
		FluxReconcile:            &v1alpha1.FluxReconcileAction{},
	}
	if err := ValidateSpec(both); err == nil {
		t.Fatal("two types must be invalid")
	}
	if err := ValidateSpec(&v1alpha1.OmpActionSpec{Description: " ", FluxReconcile: &v1alpha1.FluxReconcileAction{}}); err == nil {
		t.Fatal("a blank description must be invalid")
	}
	bad := []*v1alpha1.OmpActionSpec{
		{Description: "d", KubernetesRolloutRestart: &v1alpha1.KubernetesRolloutRestartAction{Kinds: []v1alpha1.WorkloadKind{"Job"}}},
		{Description: "d", FluxReconcile: &v1alpha1.FluxReconcileAction{Kinds: []v1alpha1.FluxKind{"Bucket"}}},
		{Description: "d", AlertmanagerSilence: &v1alpha1.AlertmanagerSilenceAction{URL: "am:9093"}},
		{Description: "d", AlertmanagerSilence: &v1alpha1.AlertmanagerSilenceAction{URL: "http://u:p@am:9093"}},
		{Description: "d", AlertmanagerSilence: &v1alpha1.AlertmanagerSilenceAction{URL: "http://am:9093/?x=1"}},
	}
	for i, s := range bad {
		if err := ValidateSpec(s); err == nil {
			t.Errorf("bad spec %d accepted", i)
		}
	}
	for _, s := range []*v1alpha1.OmpActionSpec{
		{Description: "d", KubernetesRolloutRestart: &v1alpha1.KubernetesRolloutRestartAction{}},
		{Description: "d", FluxReconcile: &v1alpha1.FluxReconcileAction{}},
		{Description: "d", AlertmanagerSilence: &v1alpha1.AlertmanagerSilenceAction{URL: "https://am.example:9093/prefix"}},
	} {
		if err := ValidateSpec(s); err != nil {
			t.Errorf("good spec rejected: %v", err)
		}
	}
}

func TestType(t *testing.T) {
	typ, ok := Type(&v1alpha1.OmpActionSpec{AlertmanagerSilence: &v1alpha1.AlertmanagerSilenceAction{}})
	if !ok || typ != v1alpha1.ActionAlertmanagerSilence {
		t.Fatalf("Type = %q, %v", typ, ok)
	}
	if _, ok := Type(&v1alpha1.OmpActionSpec{}); ok {
		t.Fatal("an empty spec has no type")
	}
}
