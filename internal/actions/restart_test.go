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
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

func restartAction(mutate func(*v1alpha1.OmpActionSpec)) *v1alpha1.OmpAction {
	return newAction("restart-app", func(s *v1alpha1.OmpActionSpec) {
		s.KubernetesRolloutRestart = &v1alpha1.KubernetesRolloutRestartAction{
			WaitTimeout: metav1.Duration{Duration: 200 * time.Millisecond},
		}
		if mutate != nil {
			mutate(s)
		}
	})
}

func deployment(namespace, name string, ready bool) *appsv1.Deployment {
	one := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
	}
	if ready {
		d.Status = appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	}
	return d
}

type restartReq struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}

func TestRestartHappyPath(t *testing.T) {
	r, c := newRunner(t, nil, newSession(testSession), restartAction(nil), deployment(testNS, "web", true))
	res := call(t, r, newAgent("restart-app"), "restart_app", restartReq{"Deployment", testNS, "web", "stuck"})
	if res.IsError || !strings.HasPrefix(res.Text, "restarted Deployment apps/web; ready 1/1 after ") {
		t.Fatalf("result = %+v", res)
	}
	var d appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "web"}, &d); err != nil {
		t.Fatal(err)
	}
	if got := d.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"]; got != fixedNow.Format(time.RFC3339) {
		t.Fatalf("restartedAt = %q", got)
	}
	recs := getSession(t, c).Status.Actions
	if len(recs) != 1 || recs[0].Action != "restart_app" || !recs[0].OK || recs[0].Target != "Deployment/apps/web" {
		t.Fatalf("status.actions = %+v", recs)
	}
	events, err := r.Store.ReadEvents(testUID, 10)
	if err != nil || len(events) != 1 || events[0].Kind != "action" || !strings.Contains(events[0].Text, "restart_app") {
		t.Fatalf("events = %+v, %v", events, err)
	}
}

func TestRestartNotReadyAfterWait(t *testing.T) {
	r, _ := newRunner(t, nil, newSession(testSession), restartAction(nil), deployment(testNS, "web", false))
	res := call(t, r, newAgent("restart-app"), "restart_app", restartReq{"Deployment", testNS, "web", "stuck"})
	if res.IsError || !strings.HasPrefix(res.Text, "restart issued; not ready after 200ms: 0/1") {
		t.Fatalf("result = %+v", res)
	}
}

func TestRestartStatefulSetAndDaemonSetComplete(t *testing.T) {
	one := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "db"},
		Spec:       appsv1.StatefulSetSpec{Replicas: &one},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1, CurrentRevision: "r1", UpdateRevision: "r1"},
	}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "agent"},
		Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, UpdatedNumberScheduled: 2, NumberAvailable: 2, NumberReady: 2},
	}
	act := restartAction(func(s *v1alpha1.OmpActionSpec) {
		s.KubernetesRolloutRestart.Kinds = []v1alpha1.WorkloadKind{v1alpha1.WorkloadStatefulSet, v1alpha1.WorkloadDaemonSet}
	})
	r, _ := newRunner(t, nil, newSession(testSession), act, sts, ds)
	agent := newAgent("restart-app")
	if res := call(t, r, agent, "restart_app", restartReq{"StatefulSet", testNS, "db", "x"}); res.IsError || !strings.HasPrefix(res.Text, "restarted StatefulSet apps/db; ready 1/1") {
		t.Fatalf("statefulset result = %+v", res)
	}
	if res := call(t, r, agent, "restart_app", restartReq{"DaemonSet", testNS, "agent", "x"}); res.IsError || !strings.HasPrefix(res.Text, "restarted DaemonSet apps/agent; ready 2/2") {
		t.Fatalf("daemonset result = %+v", res)
	}
}

func TestRestartRefusals(t *testing.T) {
	tests := []struct {
		name    string
		targets []string
		mutate  func(*v1alpha1.OmpActionSpec)
		req     restartReq
		want    string
	}{
		{"kind not allowed", nil, nil, restartReq{"StatefulSet", testNS, "web", "x"}, `kind "StatefulSet" is not allowed`},
		{"other namespace", nil, nil, restartReq{"Deployment", "other", "web", "x"}, `namespace "other" is not allowed`},
		{"star without the flag", nil, func(s *v1alpha1.OmpActionSpec) {
			s.KubernetesRolloutRestart.Namespaces.Allow = []string{"*"}
		}, restartReq{"Deployment", "other", "web", "x"}, `namespace "other" is not allowed`},
		{"operator namespace even with star flag", []string{"*"}, func(s *v1alpha1.OmpActionSpec) {
			s.KubernetesRolloutRestart.Namespaces.Allow = []string{"*"}
		}, restartReq{"Deployment", testOperNS, "web", "x"}, `namespace "omp-system" is not allowed`},
		{"denied", nil, func(s *v1alpha1.OmpActionSpec) {
			s.KubernetesRolloutRestart.Namespaces.Deny = []string{testNS}
		}, restartReq{"Deployment", testNS, "web", "x"}, "is not allowed"},
		{"empty reason", nil, nil, restartReq{"Deployment", testNS, "web", ""}, "reason must be"},
		{"empty name", nil, nil, restartReq{"Deployment", testNS, "", "x"}, "name is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep := deployment(tt.req.Namespace, "web", true)
			r, c := newRunner(t, tt.targets, newSession(testSession), restartAction(tt.mutate), dep)
			res := call(t, r, newAgent("restart-app"), "restart_app", tt.req)
			if !res.IsError || !strings.HasPrefix(res.Text, "refused: ") || !strings.Contains(res.Text, tt.want) {
				t.Fatalf("result = %+v, want refusal containing %q", res, tt.want)
			}
			var d appsv1.Deployment
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &d); err == nil && d.Spec.Template.Annotations != nil {
				t.Fatalf("refused call patched the deployment: %v", d.Spec.Template.Annotations)
			}
		})
	}
}

func TestRestartAllowedOtherNamespaceWithFlag(t *testing.T) {
	act := restartAction(func(s *v1alpha1.OmpActionSpec) {
		s.KubernetesRolloutRestart.Namespaces.Allow = []string{"other"}
	})
	r, _ := newRunner(t, []string{"other"}, newSession(testSession), act, deployment("other", "web", true))
	res := call(t, r, newAgent("restart-app"), "restart_app", restartReq{"Deployment", "other", "web", "x"})
	if res.IsError || !strings.HasPrefix(res.Text, "restarted Deployment other/web") {
		t.Fatalf("result = %+v", res)
	}
}

func TestRestartCooldown(t *testing.T) {
	rec := func(at time.Time, ok bool, target string) v1alpha1.ActionRecord {
		return v1alpha1.ActionRecord{Action: "restart_app", Target: target, OK: ok, At: metav1.NewTime(at)}
	}
	tests := []struct {
		name    string
		record  v1alpha1.ActionRecord
		blocked bool
	}{
		{"recent ok restart of the same target", rec(fixedNow.Add(-time.Hour), true, "Deployment/apps/web"), true},
		{"old ok restart", rec(fixedNow.Add(-7*time.Hour), true, "Deployment/apps/web"), false},
		{"recent failed restart", rec(fixedNow.Add(-time.Hour), false, "Deployment/apps/web"), false},
		{"recent restart of another target", rec(fixedNow.Add(-time.Hour), true, "Deployment/apps/api"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The earlier restart is in another session of the same agent.
			earlier := newSession("earlier", tt.record)
			r, _ := newRunner(t, nil, newSession(testSession), earlier, restartAction(nil), deployment(testNS, "web", true))
			res := call(t, r, newAgent("restart-app"), "restart_app", restartReq{"Deployment", testNS, "web", "x"})
			if tt.blocked {
				if !res.IsError || !strings.Contains(res.Text, "cooldown is 6h0m0s") {
					t.Fatalf("result = %+v, want cooldown refusal", res)
				}
			} else if res.IsError {
				t.Fatalf("result = %+v, want success", res)
			}
		})
	}
}

func TestMaxPerSession(t *testing.T) {
	act := restartAction(func(s *v1alpha1.OmpActionSpec) {
		s.MaxPerSession = 2
		s.KubernetesRolloutRestart.Cooldown = metav1.Duration{Duration: time.Nanosecond}
	})
	r, c := newRunner(t, nil, newSession(testSession), act, deployment(testNS, "web", true), deployment(testNS, "api", true), deployment(testNS, "db", true))
	agent := newAgent("restart-app")
	for _, n := range []string{"web", "api"} {
		if res := call(t, r, agent, "restart_app", restartReq{"Deployment", testNS, n, "x"}); res.IsError {
			t.Fatalf("call %s refused: %+v", n, res)
		}
	}
	res := call(t, r, agent, "restart_app", restartReq{"Deployment", testNS, "db", "x"})
	if !res.IsError || !strings.Contains(res.Text, "call limit reached") {
		t.Fatalf("third call = %+v, want call limit refusal", res)
	}
	if n := len(getSession(t, c).Status.Actions); n != 2 {
		t.Fatalf("recorded %d calls, want 2 (the limit refusal is not recorded)", n)
	}
	var d appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "db"}, &d); err != nil || d.Spec.Template.Annotations != nil {
		t.Fatalf("db was restarted: %v %v", err, d.Spec.Template.Annotations)
	}
}

func TestStatusActionsCapped(t *testing.T) {
	recs := make([]v1alpha1.ActionRecord, 0, maxActionRecords)
	for range maxActionRecords {
		recs = append(recs, v1alpha1.ActionRecord{Action: "other", OK: true, At: metav1.NewTime(fixedNow)})
	}
	r, _ := newRunner(t, nil, newSession(testSession, recs...), restartAction(nil), deployment(testNS, "web", true))
	res := call(t, r, newAgent("restart-app"), "restart_app", restartReq{"Deployment", testNS, "web", "x"})
	if !res.IsError || !strings.Contains(res.Text, "already made 20 action calls") {
		t.Fatalf("result = %+v", res)
	}
}

func TestCallUnknownTool(t *testing.T) {
	r, _ := newRunner(t, nil, newSession(testSession), restartAction(nil))
	for _, tool := range []string{"nope", "restart-app", "bash"} {
		_, err := r.Call(context.Background(), newAgent("restart-app"), newSession(testSession), tool, []byte(`{}`))
		if !errors.Is(err, ErrUnknownTool) {
			t.Fatalf("Call(%q) error = %v, want ErrUnknownTool", tool, err)
		}
	}
}

func TestCallNotReadyActionIsUnknown(t *testing.T) {
	act := restartAction(nil)
	act.Status.Conditions[0].Status = metav1.ConditionFalse
	r, _ := newRunner(t, nil, newSession(testSession), act)
	_, err := r.Call(context.Background(), newAgent("restart-app"), newSession(testSession), "restart_app", []byte(`{}`))
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("error = %v, want ErrUnknownTool", err)
	}
}

func TestEventsRecordRefusals(t *testing.T) {
	r, _ := newRunner(t, nil, newSession(testSession), restartAction(nil))
	call(t, r, newAgent("restart-app"), "restart_app", restartReq{"Deployment", "other", "web", "x"})
	events, err := r.Store.ReadEvents(testUID, 10)
	if err != nil || len(events) != 1 || !strings.Contains(events[0].Text, "refused") {
		t.Fatalf("events = %+v, %v", events, err)
	}
}
