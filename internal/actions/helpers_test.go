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
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	testNS      = "apps"
	testOperNS  = "omp-system"
	testAgent   = "triage"
	testSession = "s1"
	testUID     = "uid-s1"
)

var fixedNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// newRunner builds a Runner over a fake client holding objs. The operator runs in omp-system
// and lets actions target any other namespace only when targets is given.
func newRunner(t *testing.T, targets []string, objs ...client.Object) (*Runner, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.OmpSession{}, &v1alpha1.OmpAction{}).
		Build()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{
		Client:       c,
		APIReader:    c,
		Store:        st,
		Options:      &config.Options{OperatorNamespace: testOperNS, ActionTargetNamespaces: targets},
		Now:          func() time.Time { return fixedNow },
		PollInterval: 5 * time.Millisecond,
	}, c
}

func newAgent(actionNames ...string) *v1alpha1.OmpAgent {
	a := &v1alpha1.OmpAgent{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testAgent}}
	for _, n := range actionNames {
		a.Spec.Actions = append(a.Spec.Actions, v1alpha1.LocalObjectRef{Name: n})
	}
	return a
}

func newSession(name string, records ...v1alpha1.ActionRecord) *v1alpha1.OmpSession {
	return &v1alpha1.OmpSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNS, Name: name, UID: types.UID("uid-" + name),
			Labels: map[string]string{v1alpha1.LabelAgent: testAgent},
		},
		Spec:   v1alpha1.OmpSessionSpec{AgentRef: v1alpha1.LocalObjectRef{Name: testAgent}, Prompt: "go"},
		Status: v1alpha1.OmpSessionStatus{Phase: v1alpha1.SessionPhaseRunning, Actions: records},
	}
}

// newAction returns a Ready OmpAction in the test namespace.
func newAction(name string, mutate func(*v1alpha1.OmpActionSpec)) *v1alpha1.OmpAction {
	a := &v1alpha1.OmpAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name},
		Spec:       v1alpha1.OmpActionSpec{Description: "does a thing"},
		Status: v1alpha1.OmpActionStatus{Conditions: []metav1.Condition{{
			Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now(),
		}}},
	}
	mutate(&a.Spec)
	return a
}

func call(t *testing.T, r *Runner, agent *v1alpha1.OmpAgent, tool string, args any) agentapi.ActionResult {
	t.Helper()
	res, err := r.Call(context.Background(), agent, newSession(testSession), tool, mustMarshal(t, args))
	if err != nil {
		t.Fatalf("Call(%s): %v", tool, err)
	}
	return res
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func getSession(t *testing.T, c client.Client) *v1alpha1.OmpSession {
	t.Helper()
	var s v1alpha1.OmpSession
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: testSession}, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func jsonParams(schema string) apiextensionsv1.JSON {
	return apiextensionsv1.JSON{Raw: []byte(schema)}
}
