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
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
)

// ---- sink server ----

// etPost is one request the sink server received.
type etPost struct {
	Path string
	Body []byte
}

// JSON decodes the body into a map.
func (p etPost) JSON() map[string]any {
	GinkgoHelper()
	var m map[string]any
	Expect(json.Unmarshal(p.Body, &m)).To(Succeed())
	return m
}

// etSinkServer records the POSTs of agents' webhook notification sinks. Every agent's sink
// Secret points at its own path, so specs read only their own posts.
type etSinkServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	posts []etPost
}

func newEtSinkServer() *etSinkServer {
	s := &etSinkServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.posts = append(s.posts, etPost{Path: r.URL.Path, Body: body})
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	return s
}

func (s *etSinkServer) Posts(path string) []etPost {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []etPost
	for _, p := range s.posts {
		if p.Path == path {
			out = append(out, p)
		}
	}
	return out
}

// etSinkPath is the sink path of an agent.
func etSinkPath(ns, agent string) string { return "/" + ns + "/" + agent }

// etSinkFor creates the Secret of the agent's webhook sink and returns the sink.
func etSinkFor(ns, agent string) ompv1alpha1.NotifySink {
	GinkgoHelper()
	etCreate(etSecret(ns, agent+"-sink", map[string]string{"url": etSink.srv.URL + etSinkPath(ns, agent)}))
	return ompv1alpha1.NotifySink{
		Type:    ompv1alpha1.SinkWebhook,
		Webhook: &ompv1alpha1.WebhookSink{URLSecretRef: ompv1alpha1.SecretKeyRef{Name: agent + "-sink", Key: "url"}},
	}
}

// ---- generic object helpers ----

var (
	etNameSeq atomic.Int64
	etPokeSeq atomic.Int64
)

// etName returns a unique lower-case object name.
func etName(prefix string) string {
	return fmt.Sprintf("%s-%s-%d", prefix, strings.ToLower(rand.Text()[:5]), etNameSeq.Add(1))
}

func etKey(o client.Object) types.NamespacedName { return client.ObjectKeyFromObject(o) }

func etCreate(o client.Object) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, o)).To(Succeed())
}

// etCleanup deletes the objects when the spec ends and waits until they are gone. Finalizers
// are removed by the running reconcilers.
func etCleanup(objs ...client.Object) {
	GinkgoHelper()
	type ref struct {
		obj client.Object
		key types.NamespacedName
	}
	refs := make([]ref, 0, len(objs))
	for _, o := range objs {
		refs = append(refs, ref{obj: o, key: etKey(o)})
	}
	DeferCleanup(func() {
		for _, r := range refs {
			stub := r.obj.DeepCopyObject().(client.Object)
			stub.SetNamespace(r.key.Namespace)
			stub.SetName(r.key.Name)
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, stub))).To(Succeed())
		}
		for _, r := range refs {
			obj := r.obj.DeepCopyObject().(client.Object)
			Eventually(func() bool { return etNotFound(k8sClient.Get(ctx, r.key, obj)) }).Should(BeTrue(),
				"%T %s should be gone", r.obj, r.key)
		}
	})
}

// etPoke touches an object's annotations so its reconciler runs again, e.g. after the fake
// clock moved or a Secret it reads appeared. o only needs a namespace and a name.
func etPoke(o client.Object) {
	GinkgoHelper()
	stub := o.DeepCopyObject().(client.Object)
	stub.SetNamespace(o.GetNamespace())
	stub.SetName(o.GetName())
	patch := fmt.Sprintf(`{"metadata":{"annotations":{"envtest/poke":"%d"}}}`, etPokeSeq.Add(1))
	Expect(k8sClient.Patch(ctx, stub, client.RawPatch(types.MergePatchType, []byte(patch)))).To(Succeed())
}

func etSecret(ns, name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, StringData: data}
	return s
}

func etCond(conds []metav1.Condition, typ string) *metav1.Condition {
	return meta.FindStatusCondition(conds, typ)
}

// ---- agents ----

// etAgent returns an agent that is Ready once its references exist: an explicit image, one
// model, retry settings short enough to stay under the fake clock's steps.
func etAgent(ns, name string, mutate func(*ompv1alpha1.OmpAgentSpec)) *ompv1alpha1.OmpAgent {
	a := &ompv1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: ompv1alpha1.OmpAgentSpec{
			Image: etRuntimeImage,
			Model: ompv1alpha1.ModelSpec{Default: "anthropic/claude-sonnet-5-5"},
			Limits: ompv1alpha1.LimitsSpec{
				MaxAttempts: 3,
				RetryDelay:  metav1.Duration{Duration: 30 * time.Second},
			},
		},
	}
	if mutate != nil {
		mutate(&a.Spec)
	}
	return a
}

// etAgentCondition waits for the agent's condition typ to have the given status and reason.
func etAgentCondition(key types.NamespacedName, typ string, status metav1.ConditionStatus, reason string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var a ompv1alpha1.OmpAgent
		g.Expect(k8sClient.Get(ctx, key, &a)).To(Succeed())
		c := etCond(a.Status.Conditions, typ)
		g.Expect(c).NotTo(BeNil(), "no %s condition yet", typ)
		g.Expect(c.Status).To(Equal(status))
		g.Expect(c.Reason).To(Equal(reason), c.Message)
	}).Should(Succeed())
}

// etReadyAgent creates an agent with a webhook sink and waits until it is Ready.
func etReadyAgent(ns string, mutate func(*ompv1alpha1.OmpAgentSpec)) *ompv1alpha1.OmpAgent {
	GinkgoHelper()
	name := etName("agent")
	sink := etSinkFor(ns, name)
	a := etAgent(ns, name, func(s *ompv1alpha1.OmpAgentSpec) {
		s.Notify = []ompv1alpha1.NotifySink{sink}
		if mutate != nil {
			mutate(s)
		}
	})
	etCreate(a)
	etCleanup(a)
	etAgentCondition(etKey(a), ompv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready")
	return a
}

// etBrokenAgent creates an agent that stays not Ready (it needs a Secret that is missing), so
// its sessions are never started.
func etBrokenAgent(ns string) *ompv1alpha1.OmpAgent {
	GinkgoHelper()
	a := etAgent(ns, etName("broken"), func(s *ompv1alpha1.OmpAgentSpec) {
		s.Credentials = []ompv1alpha1.Credential{{
			Name: "absent", Description: "a Secret that does not exist", Env: "ABSENT",
			SecretRef: ompv1alpha1.SecretKeyRef{Name: "does-not-exist", Key: "k"},
		}}
	})
	etCreate(a)
	etCleanup(a)
	etAgentCondition(etKey(a), ompv1alpha1.ConditionReady, metav1.ConditionFalse, "MissingSecret")
	return a
}

// ---- sessions ----

// etSession returns a manual session of the agent.
func etSession(ns, agent string, mutate func(*ompv1alpha1.OmpSession)) *ompv1alpha1.OmpSession {
	s := &ompv1alpha1.OmpSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: etName("sess")},
		Spec: ompv1alpha1.OmpSessionSpec{
			AgentRef: ompv1alpha1.LocalObjectRef{Name: agent},
			Prompt:   "check the cluster",
		},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

// etCreateSession creates a session and arranges for its removal.
func etCreateSession(ns, agent string, mutate func(*ompv1alpha1.OmpSession)) *ompv1alpha1.OmpSession {
	GinkgoHelper()
	s := etSession(ns, agent, mutate)
	etCreate(s)
	etCleanup(s)
	return s
}

func etGetSession(key types.NamespacedName) *ompv1alpha1.OmpSession {
	GinkgoHelper()
	s := &ompv1alpha1.OmpSession{}
	Expect(k8sClient.Get(ctx, key, s)).To(Succeed())
	return s
}

// etSessionPhase waits until the session is in the given phase.
func etSessionPhase(key types.NamespacedName, phase ompv1alpha1.SessionPhase) *ompv1alpha1.OmpSession {
	GinkgoHelper()
	var got ompv1alpha1.OmpSession
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		g.Expect(got.Status.Phase).To(Equal(phase), "error: %q", got.Status.Error)
	}).Should(Succeed())
	return &got
}

// etAwaitPod waits until the session runs its attempt-th pod and returns the pod's key.
func etAwaitPod(key types.NamespacedName, attempt int32) types.NamespacedName {
	GinkgoHelper()
	var pod types.NamespacedName
	Eventually(func(g Gomega) {
		var s ompv1alpha1.OmpSession
		g.Expect(k8sClient.Get(ctx, key, &s)).To(Succeed())
		g.Expect(s.Status.Phase).To(Equal(ompv1alpha1.SessionPhaseRunning))
		g.Expect(s.Status.Attempts).To(Equal(attempt))
		g.Expect(s.Status.PodName).NotTo(BeEmpty())
		pod = types.NamespacedName{Namespace: key.Namespace, Name: s.Status.PodName}
		g.Expect(k8sClient.Get(ctx, pod, &corev1.Pod{})).To(Succeed())
	}).Should(Succeed())
	return pod
}

// etSetPod plays the kubelet: it rewrites the pod's status.
func etSetPod(key types.NamespacedName, mutate func(*corev1.PodStatus)) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var p corev1.Pod
		g.Expect(k8sClient.Get(ctx, key, &p)).To(Succeed())
		mutate(&p.Status)
		g.Expect(k8sClient.Status().Update(ctx, &p)).To(Succeed())
	}).Should(Succeed())
}

// etPodSucceeded marks the pod as having exited 0.
func etPodSucceeded(s *corev1.PodStatus) { s.Phase = corev1.PodSucceeded }

// etReport plays the agent API: it merge-patches fields of the session's status.
func etReport(key types.NamespacedName, status map[string]any) {
	GinkgoHelper()
	body, err := json.Marshal(map[string]any{"status": status})
	Expect(err).NotTo(HaveOccurred())
	stub := &ompv1alpha1.OmpSession{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Status().Patch(ctx, stub, client.RawPatch(types.MergePatchType, body))).To(Succeed())
	}).Should(Succeed())
}

// etFinish ends the session's current attempt the way a successful driver does: the agent API
// records verdict ok, then the pod exits 0.
func etFinish(key types.NamespacedName, pod types.NamespacedName) {
	GinkgoHelper()
	etReport(key, map[string]any{
		"outcome": "completed", "verdict": "ok", "title": "finished", "summary": "all done",
	})
	etSetPod(pod, etPodSucceeded)
	etSessionPhase(key, ompv1alpha1.SessionPhaseSucceeded)
}

// etAgentPods lists the agent's operator-created pods, uncached.
func etAgentPods(ns, agent string, role ompv1alpha1.PodRole) []corev1.Pod {
	GinkgoHelper()
	var l corev1.PodList
	sel := client.MatchingLabels{
		ompv1alpha1.LabelManagedBy: ompv1alpha1.ManagedByValue,
		ompv1alpha1.LabelAgent:     agent,
	}
	if role != "" {
		sel[ompv1alpha1.LabelRole] = string(role)
	}
	Expect(k8sClient.List(ctx, &l, client.InNamespace(ns), sel)).To(Succeed())
	return l.Items
}

// etAPICall calls the real agent API handler and returns the status code and body.
func etAPICall(method, path, token string, body any) (int, []byte) {
	GinkgoHelper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		Expect(err).NotTo(HaveOccurred())
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, etAPI.URL+path, rdr)
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := etAPI.Client().Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return resp.StatusCode, out
}

// etToken reads the API token of a token Secret.
func etToken(ns, secret string) string {
	GinkgoHelper()
	var s corev1.Secret
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secret}, &s)).To(Succeed())
		g.Expect(s.Data["token"]).NotTo(BeEmpty())
	}).Should(Succeed())
	return string(s.Data["token"])
}
