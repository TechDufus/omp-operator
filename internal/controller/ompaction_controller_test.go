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
package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
)

func actionTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := ompv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func actionTestObject(name string, mutate func(*ompv1alpha1.OmpActionSpec)) *ompv1alpha1.OmpAction {
	a := &ompv1alpha1.OmpAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: name, Generation: 3},
		Spec: ompv1alpha1.OmpActionSpec{
			Description:              "restarts things",
			KubernetesRolloutRestart: &ompv1alpha1.KubernetesRolloutRestartAction{},
		},
	}
	if mutate != nil {
		mutate(&a.Spec)
	}
	return a
}

func reconcileAction(t *testing.T, opts *config.Options, objs ...client.Object) (ctrl.Result, *ompv1alpha1.OmpAction, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(actionTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&ompv1alpha1.OmpAction{}).
		Build()
	r := &OmpActionReconciler{Client: c, Scheme: c.Scheme(), Options: opts}
	key := client.ObjectKeyFromObject(objs[0])
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got ompv1alpha1.OmpAction
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	return res, &got, c
}

func TestOmpActionReady(t *testing.T) {
	val := "x"
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "api"},
		Data:       map[string][]byte{"token": []byte("t")},
	}
	httpSpec := func(s *ompv1alpha1.OmpActionSpec) {
		s.KubernetesRolloutRestart = nil
		s.HTTPRequest = &ompv1alpha1.HTTPRequestAction{
			Method: ompv1alpha1.MethodGet, URL: "https://a.example/x/{{.Args.id}}", AllowedURLPrefix: "https://a.example/x",
			Parameters: apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"id":{"type":"integer"}}}`)},
			Headers:    []ompv1alpha1.ValueOrSecret{{Name: "X-Literal", Value: &val}},
		}
	}
	tests := []struct {
		name       string
		action     *ompv1alpha1.OmpAction
		extra      []client.Object
		wantStatus metav1.ConditionStatus
		wantReason string
		wantType   ompv1alpha1.ActionType
	}{
		{
			name:       "rollout restart is ready",
			action:     actionTestObject("restart-web", nil),
			wantStatus: metav1.ConditionTrue, wantReason: "Ready", wantType: ompv1alpha1.ActionKubernetesRolloutRestart,
		},
		{
			name:       "http request is ready",
			action:     actionTestObject("call-api", httpSpec),
			wantStatus: metav1.ConditionTrue, wantReason: "Ready", wantType: ompv1alpha1.ActionHTTPRequest,
		},
		{
			name: "http request with an existing secret header is ready",
			action: actionTestObject("call-api", func(s *ompv1alpha1.OmpActionSpec) {
				httpSpec(s)
				s.HTTPRequest.Headers = append(s.HTTPRequest.Headers, ompv1alpha1.ValueOrSecret{
					Name: "X-Token", SecretRef: &ompv1alpha1.SecretKeyRef{Name: "api", Key: "token"},
				})
			}),
			extra:      []client.Object{sec},
			wantStatus: metav1.ConditionTrue, wantReason: "Ready", wantType: ompv1alpha1.ActionHTTPRequest,
		},
		{
			name:       "invalid name",
			action:     actionTestObject("Restart_Web", nil),
			wantStatus: metav1.ConditionFalse, wantReason: "InvalidName", wantType: ompv1alpha1.ActionKubernetesRolloutRestart,
		},
		{
			name:       "reserved name",
			action:     actionTestObject("bash", nil),
			wantStatus: metav1.ConditionFalse, wantReason: "ReservedName", wantType: ompv1alpha1.ActionKubernetesRolloutRestart,
		},
		{
			name:       "reserved name after dash conversion",
			action:     actionTestObject("web-search", nil),
			wantStatus: metav1.ConditionFalse, wantReason: "ReservedName", wantType: ompv1alpha1.ActionKubernetesRolloutRestart,
		},
		{
			name:       "finish is reserved",
			action:     actionTestObject("finish", nil),
			wantStatus: metav1.ConditionFalse, wantReason: "ReservedName", wantType: ompv1alpha1.ActionKubernetesRolloutRestart,
		},
		{
			name: "no type set",
			action: actionTestObject("empty", func(s *ompv1alpha1.OmpActionSpec) {
				s.KubernetesRolloutRestart = nil
			}),
			wantStatus: metav1.ConditionFalse, wantReason: "InvalidSpec", wantType: "",
		},
		{
			name: "two types set",
			action: actionTestObject("both", func(s *ompv1alpha1.OmpActionSpec) {
				s.FluxReconcile = &ompv1alpha1.FluxReconcileAction{}
			}),
			wantStatus: metav1.ConditionFalse, wantReason: "InvalidSpec", wantType: "",
		},
		{
			name: "silence url with userinfo",
			action: actionTestObject("silence", func(s *ompv1alpha1.OmpActionSpec) {
				s.KubernetesRolloutRestart = nil
				s.AlertmanagerSilence = &ompv1alpha1.AlertmanagerSilenceAction{URL: "http://u:p@am:9093"}
			}),
			wantStatus: metav1.ConditionFalse, wantReason: "InvalidSpec", wantType: ompv1alpha1.ActionAlertmanagerSilence,
		},
		{
			name: "http url template does not parse",
			action: actionTestObject("call-api", func(s *ompv1alpha1.OmpActionSpec) {
				httpSpec(s)
				s.HTTPRequest.URL = "https://a.example/{{.Args.id"
			}),
			wantStatus: metav1.ConditionFalse, wantReason: "InvalidSpec", wantType: ompv1alpha1.ActionHTTPRequest,
		},
		{
			name: "http parameters schema does not compile",
			action: actionTestObject("call-api", func(s *ompv1alpha1.OmpActionSpec) {
				httpSpec(s)
				s.HTTPRequest.Parameters = apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"id":{"type":"bogus"}}}`)}
			}),
			wantStatus: metav1.ConditionFalse, wantReason: "InvalidSpec", wantType: ompv1alpha1.ActionHTTPRequest,
		},
		{
			name: "http allowedURLPrefix with a query",
			action: actionTestObject("call-api", func(s *ompv1alpha1.OmpActionSpec) {
				httpSpec(s)
				s.HTTPRequest.AllowedURLPrefix = "https://a.example/x?y=1"
			}),
			wantStatus: metav1.ConditionFalse, wantReason: "InvalidSpec", wantType: ompv1alpha1.ActionHTTPRequest,
		},
		{
			name: "secret missing",
			action: actionTestObject("call-api", func(s *ompv1alpha1.OmpActionSpec) {
				httpSpec(s)
				s.HTTPRequest.Headers = []ompv1alpha1.ValueOrSecret{{Name: "X-Token", SecretRef: &ompv1alpha1.SecretKeyRef{Name: "api", Key: "token"}}}
			}),
			wantStatus: metav1.ConditionFalse, wantReason: "MissingSecret", wantType: ompv1alpha1.ActionHTTPRequest,
		},
		{
			name: "secret key missing",
			action: actionTestObject("call-api", func(s *ompv1alpha1.OmpActionSpec) {
				httpSpec(s)
				s.HTTPRequest.Headers = []ompv1alpha1.ValueOrSecret{{Name: "X-Token", SecretRef: &ompv1alpha1.SecretKeyRef{Name: "api", Key: "nope"}}}
			}),
			extra:      []client.Object{sec},
			wantStatus: metav1.ConditionFalse, wantReason: "MissingSecret", wantType: ompv1alpha1.ActionHTTPRequest,
		},
		{
			name: "silence auth secret missing",
			action: actionTestObject("silence", func(s *ompv1alpha1.OmpActionSpec) {
				s.KubernetesRolloutRestart = nil
				s.AlertmanagerSilence = &ompv1alpha1.AlertmanagerSilenceAction{
					URL: "http://am:9093", AuthSecretRef: &ompv1alpha1.SecretKeyRef{Name: "am", Key: "token"},
				}
			}),
			wantStatus: metav1.ConditionFalse, wantReason: "MissingSecret", wantType: ompv1alpha1.ActionAlertmanagerSilence,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, got, _ := reconcileAction(t, &config.Options{OperatorNamespace: "omp-system"}, append([]client.Object{tt.action}, tt.extra...)...)
			if res.RequeueAfter != 5*time.Minute {
				t.Fatalf("RequeueAfter = %s, want 5m", res.RequeueAfter)
			}
			ready := meta.FindStatusCondition(got.Status.Conditions, ompv1alpha1.ConditionReady)
			if ready == nil || ready.Status != tt.wantStatus || ready.Reason != tt.wantReason {
				t.Fatalf("Ready = %+v, want %s/%s", ready, tt.wantStatus, tt.wantReason)
			}
			if ready.ObservedGeneration != 3 {
				t.Fatalf("observedGeneration = %d, want 3", ready.ObservedGeneration)
			}
			if got.Status.Type != tt.wantType {
				t.Fatalf("status.type = %q, want %q", got.Status.Type, tt.wantType)
			}
		})
	}
}

func TestOmpActionRecoversWhenSecretAppears(t *testing.T) {
	action := actionTestObject("silence", func(s *ompv1alpha1.OmpActionSpec) {
		s.KubernetesRolloutRestart = nil
		s.AlertmanagerSilence = &ompv1alpha1.AlertmanagerSilenceAction{
			URL: "http://am:9093", AuthSecretRef: &ompv1alpha1.SecretKeyRef{Name: "am", Key: "token"},
		}
	})
	opts := &config.Options{OperatorNamespace: "omp-system"}
	_, got, c := reconcileAction(t, opts, action)
	if ready := meta.FindStatusCondition(got.Status.Conditions, ompv1alpha1.ConditionReady); ready == nil || ready.Reason != "MissingSecret" {
		t.Fatalf("Ready = %+v", ready)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "am"}, Data: map[string][]byte{"token": []byte("t")}}
	if err := c.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	r := &OmpActionReconciler{Client: c, Scheme: c.Scheme(), Options: opts}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(action)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(action), got); err != nil {
		t.Fatal(err)
	}
	if ready := meta.FindStatusCondition(got.Status.Conditions, ompv1alpha1.ConditionReady); ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v after the Secret was created", ready)
	}
}

func TestOmpActionSkipsDisallowedNamespace(t *testing.T) {
	res, got, _ := reconcileAction(t, &config.Options{OperatorNamespace: "omp-system", AgentNamespaces: []string{"other"}}, actionTestObject("restart-web", nil))
	if res.RequeueAfter != 0 || len(got.Status.Conditions) != 0 || got.Status.Type != "" {
		t.Fatalf("an action in a namespace the operator does not serve was reconciled: %+v %+v", res, got.Status)
	}
}

func TestOmpActionMissingObject(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(actionTestScheme(t)).WithStatusSubresource(&ompv1alpha1.OmpAction{}).Build()
	r := &OmpActionReconciler{Client: c, Scheme: c.Scheme(), Options: &config.Options{}}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "apps", Name: "gone"}})
	if err != nil || res != (ctrl.Result{}) {
		t.Fatalf("Reconcile of a deleted action = %+v, %v", res, err)
	}
}

func TestOmpActionStatusWrittenOnce(t *testing.T) {
	opts := &config.Options{OperatorNamespace: "omp-system"}
	_, got, c := reconcileAction(t, opts, actionTestObject("restart-web", nil))
	first := meta.FindStatusCondition(got.Status.Conditions, ompv1alpha1.ConditionReady).LastTransitionTime
	rv := got.ResourceVersion
	r := &OmpActionReconciler{Client: c, Scheme: c.Scheme(), Options: opts}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(got)}); err != nil {
		t.Fatal(err)
	}
	var again ompv1alpha1.OmpAction
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(got), &again); err != nil {
		t.Fatal(err)
	}
	if again.ResourceVersion != rv {
		t.Fatalf("an unchanged status was written again (resourceVersion %s -> %s)", rv, again.ResourceVersion)
	}
	if !meta.FindStatusCondition(again.Status.Conditions, ompv1alpha1.ConditionReady).LastTransitionTime.Equal(&first) {
		t.Fatal("lastTransitionTime changed without a status change")
	}
}
