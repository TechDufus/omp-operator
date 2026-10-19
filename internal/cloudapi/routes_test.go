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

package cloudapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/store"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const threadAbc = "coder-abc123"

func secretCreateFails() interceptor.Funcs {
	return interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*corev1.Secret); ok {
			return errors.New("secret store is down")
		}
		return c.Create(ctx, obj, opts...)
	}}
}

// threadDeleteCounter counts OmpThread deletes.
func threadDeleteCounter(n *int) interceptor.Funcs {
	return interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*v1alpha1.OmpThread); ok {
			*n++
		}
		return c.Delete(ctx, obj, opts...)
	}}
}

// threadCreateCollides makes the first n OmpThread creates answer AlreadyExists.
func threadCreateCollides(calls *int, n int) interceptor.Funcs {
	return interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*v1alpha1.OmpThread); ok {
			*calls++
			if *calls <= n {
				return apierrors.NewAlreadyExists(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "ompthreads"}, obj.GetName())
			}
		}
		return c.Create(ctx, obj, opts...)
	}}
}

func threadObject(name string, age time.Duration, mutate func(*v1alpha1.OmpThread)) *v1alpha1.OmpThread {
	t := &v1alpha1.OmpThread{
		ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: name, CreationTimestamp: metav1.NewTime(epoch.Add(-age))},
		Spec: v1alpha1.OmpThreadSpec{
			AgentRef: v1alpha1.LocalObjectRef{Name: agentCoder}, Owner: "token:ops", Title: "title of " + name,
			Repo:  v1alpha1.ThreadRepo{Name: "app", RemoteURL: remoteURL, BaseRef: "main", BaseCommit: baseSHA, HandoffCommit: handoffSHA, LocalBranch: "main"},
			Paths: v1alpha1.ThreadPaths{Home: "/home/ada", Workspace: "/home/ada/app"},
		},
	}
	if mutate != nil {
		mutate(t)
	}
	return t
}

func TestListThreads(t *testing.T) {
	objs := make([]client.Object, 0, 56)
	for i := range 55 {
		objs = append(objs, threadObject(fmt.Sprintf("coder-t%05d", i), time.Duration(i)*time.Minute, nil))
	}
	other := threadObject("coder-other1", 0, func(th *v1alpha1.OmpThread) { th.Namespace = nsOther })
	h := newHarness(t, nil, append(objs, other)...)

	for _, tok := range []string{opsToken, viewerToken} {
		w := h.do(http.MethodGet, threadsPath, tok, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		got := decode[struct{ Threads []ThreadSummary }](t, w).Threads
		if len(got) != 50 || got[0].Name != "coder-t00000" || got[49].Name != "coder-t00049" {
			t.Fatalf("%s: %d threads, first %s last %s", tok, len(got), got[0].Name, got[len(got)-1].Name)
		}
		for _, s := range got {
			if s.Namespace != nsProd {
				t.Fatalf("thread of namespace %s listed", s.Namespace)
			}
		}
	}
	w := h.do(http.MethodGet, threadsPath, opsToken, nil)
	if !strings.Contains(w.Body.String(), `"url":"https://omp.example.com/threads/prod/coder-t00000"`) || !strings.Contains(w.Body.String(), `"turns":0`) {
		t.Errorf("summary shape: %.400s", w.Body)
	}

	empty := newHarness(t, nil)
	if w := empty.do(http.MethodGet, threadsPath, opsToken, nil); strings.TrimSpace(w.Body.String()) != `{"threads":[]}` {
		t.Errorf("empty list = %s", w.Body)
	}
}

func TestGetThread(t *testing.T) {
	long := strings.Repeat("é", 1500) // 3000 bytes
	th := threadObject(threadAbc, time.Hour, func(th *v1alpha1.OmpThread) {
		th.Status = v1alpha1.OmpThreadStatus{
			Phase: v1alpha1.ThreadPhaseWorking, Turns: 2, Queued: 1, ActiveSession: threadAbc + "-1",
			Latest: &v1alpha1.ThreadTurnSummary{ID: 1, State: v1alpha1.TurnStateDone, Title: "done"},
		}
	})
	h := newHarness(t, nil, th)
	if _, err := h.log.Enqueue(nsProd, threadAbc, long, "token:ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.log.Next(t.Context(), nsProd, threadAbc, threadAbc+"-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := h.log.Complete(nsProd, threadAbc, threadAbc+"-1", 1, threads.Result{
		Outcome: threads.OutcomeCompleted, Reply: "# Done\nbody", Usage: threads.Usage{InputTokens: 5}, Head: handoffSHA, FilesChanged: 2, Insertions: 3, Deletions: 1, Pushed: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.log.Enqueue(nsProd, threadAbc, "second", "token:viewer"); err != nil {
		t.Fatal(err)
	}

	for _, tok := range []string{opsToken, viewerToken} {
		checkDetail(t, h, tok, long)
	}
	if w := h.do(http.MethodGet, threadsPath+"/coder-nope00", opsToken, nil); w.Code != http.StatusNotFound || errorOf(t, w) != "not found" {
		t.Errorf("unknown thread: %d %s", w.Code, w.Body)
	}
	// A thread of another namespace is not reachable through this one.
	if w := h.do(http.MethodGet, "/api/v1/namespaces/other/threads/"+threadAbc, opsToken, nil); w.Code != http.StatusNotFound {
		t.Errorf("other namespace: %d", w.Code)
	}
}

func checkDetail(t *testing.T, h *harness, token, longText string) {
	t.Helper()
	w := h.do(http.MethodGet, threadsPath+"/"+threadAbc, token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	raw := decode[map[string]any](t, w)
	if turns, ok := raw["turns"].([]any); !ok || len(turns) != 2 {
		t.Fatalf("turns = %#v", raw["turns"])
	}
	for _, k := range []string{"repo", "paths", "activeSession", "latest", "url", "createdAt"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("detail lacks %q", k)
		}
	}
	d := decode[ThreadDetail](t, w)
	if d.Repo.BaseCommit != baseSHA || d.Paths.Workspace != "/home/ada/app" || d.ActiveSession != threadAbc+"-1" || d.Phase != v1alpha1.ThreadPhaseWorking {
		t.Errorf("detail = %+v", d)
	}
	t1, t2 := d.Turns[0], d.Turns[1]
	if t1.ID != 1 || t1.State != v1alpha1.TurnStateDone || len(t1.Text) > 2000 || !strings.HasPrefix(longText, t1.Text) || t1.Reply != "# Done\nbody" ||
		t1.Title != "Done" || t1.Usage == nil || t1.Usage.InputTokens != 5 || t1.Head != handoffSHA || t1.FilesChanged != 2 || !t1.Pushed || t1.By != "token:ops" {
		t.Errorf("turn 1 = %+v", t1)
	}
	if t2.ID != 2 || t2.State != v1alpha1.TurnStateQueued || t2.Text != "second" || t2.By != "token:viewer" {
		t.Errorf("turn 2 = %+v", t2)
	}
}

func TestEvents(t *testing.T) {
	sitting := func(n int, age time.Duration, uid string, phase v1alpha1.SessionPhase, thread string) *v1alpha1.OmpSession {
		return &v1alpha1.OmpSession{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: nsProd, Name: fmt.Sprintf("%s-%d", thread, n), UID: types.UID(uid), CreationTimestamp: metav1.NewTime(epoch.Add(-age)),
				Labels: map[string]string{v1alpha1.LabelThread: thread},
			},
			Spec:   v1alpha1.OmpSessionSpec{AgentRef: v1alpha1.LocalObjectRef{Name: agentCoder}, Prompt: "p", ThreadRef: &v1alpha1.LocalObjectRef{Name: thread}},
			Status: v1alpha1.OmpSessionStatus{Phase: phase},
		}
	}
	h := newHarness(t, nil,
		threadObject(threadAbc, time.Hour, nil),
		threadObject("coder-empty1", time.Hour, nil),
		sitting(1, 3*time.Hour, "uid-old", v1alpha1.SessionPhaseSucceeded, threadAbc),
		sitting(2, 2*time.Hour, "uid-new", v1alpha1.SessionPhaseRunning, threadAbc),
		sitting(1, time.Hour, "uid-foreign", v1alpha1.SessionPhaseRunning, "coder-foreign"),
	)
	newEvents := make([]store.Event, 0, 600)
	for i := range 600 {
		newEvents = append(newEvents, store.Event{At: epoch.Add(time.Duration(i) * time.Second), Kind: "tool", Text: fmt.Sprintf("e%d", i)})
	}
	for uid, evs := range map[string][]store.Event{
		"uid-old":     {{At: epoch.Add(-time.Hour), Kind: "notice", Text: "old sitting"}},
		"uid-new":     newEvents,
		"uid-foreign": {{At: epoch, Kind: "notice", Text: "foreign"}},
	} {
		if err := h.st.AppendEvents(uid, evs); err != nil {
			t.Fatal(err)
		}
	}
	type resp struct {
		Session string  `json:"session"`
		Events  []Event `json:"events"`
	}
	get := func(tok, thread, query string) *resp {
		t.Helper()
		w := h.do(http.MethodGet, threadsPath+"/"+thread+"/events"+query, tok, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body)
		}
		r := decode[resp](t, w)
		return &r
	}

	r := get(viewerToken, threadAbc, "")
	if r.Session != threadAbc+"-2" || len(r.Events) != 500 || r.Events[0].Text != "e100" || r.Events[499].Text != "e599" {
		t.Fatalf("no since: session %q, %d events (%v..)", r.Session, len(r.Events), r.Events[0])
	}
	cursor := epoch.Add(590 * time.Second).Format(time.RFC3339Nano)
	r = get(opsToken, threadAbc, "?since="+cursor)
	if len(r.Events) != 9 || r.Events[0].Text != "e591" {
		t.Errorf("since %s: %d events, first %q", cursor, len(r.Events), r.Events[0].Text)
	}
	if r = get(opsToken, threadAbc, "?since="+epoch.Add(time.Hour).Format(time.RFC3339Nano)); len(r.Events) != 0 || r.Session != threadAbc+"-2" {
		t.Errorf("since in the future: %+v", r)
	}

	// Once no sitting is active the newest one's events are served.
	var s2 v1alpha1.OmpSession
	if err := h.c.Get(t.Context(), types.NamespacedName{Namespace: nsProd, Name: threadAbc + "-2"}, &s2); err != nil {
		t.Fatal(err)
	}
	s2.Status.Phase = v1alpha1.SessionPhaseSucceeded
	if err := h.c.Status().Update(t.Context(), &s2); err != nil {
		t.Fatal(err)
	}
	if r = get(opsToken, threadAbc, "?since="+cursor); r.Session != threadAbc+"-2" || len(r.Events) != 9 {
		t.Errorf("newest finished sitting: %q %d", r.Session, len(r.Events))
	}

	w := h.do(http.MethodGet, threadsPath+"/coder-empty1/events", opsToken, nil)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"session":"","events":[]}` {
		t.Errorf("thread without sittings: %d %s", w.Code, w.Body)
	}
	if w := h.do(http.MethodGet, threadsPath+"/"+threadAbc+"/events?since=yesterday", opsToken, nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad since: %d", w.Code)
	}
}

func TestMessages(t *testing.T) {
	post := func(h *harness, name string, body any) *httpResult {
		w := h.do(http.MethodPost, threadsPath+"/"+name+"/messages", opsToken, jsonBody(t, body))
		return &httpResult{w.Code, w.Body.String()}
	}

	t.Run("queues a turn and signals the thread", func(t *testing.T) {
		h := newHarness(t, nil, threadObject(threadAbc, time.Hour, nil))
		res := post(h, threadAbc, map[string]string{"text": "and then?"})
		if res.code != http.StatusCreated || !strings.Contains(res.body, `"turn":{"id":1,"state":"queued","text":"and then?","by":"token:ops"`) {
			t.Fatalf("%d %s", res.code, res.body)
		}
		turns, _ := h.log.Turns(nsProd, threadAbc)
		if len(turns) != 1 || turns[0].Text != "and then?" {
			t.Errorf("turns = %+v", turns)
		}
		select {
		case ev := <-h.log.Events():
			if ev.Object.GetNamespace() != nsProd || ev.Object.GetName() != threadAbc {
				t.Errorf("signalled %s/%s", ev.Object.GetNamespace(), ev.Object.GetName())
			}
		case <-time.After(time.Second):
			t.Error("the thread was not signalled")
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		h := newHarness(t, nil, threadObject(threadAbc, time.Hour, nil))
		for name, tc := range map[string]struct {
			body any
			code int
		}{
			"empty text":     {map[string]string{"text": ""}, 400},
			"missing text":   {map[string]string{}, 400},
			"too long":       {map[string]string{"text": strings.Repeat("x", threads.MaxTextBytes+1)}, 400},
			"body too large": {map[string]string{"text": strings.Repeat("x", 2*threads.MaxTextBytes)}, 413},
		} {
			if res := post(h, threadAbc, tc.body); res.code != tc.code {
				t.Errorf("%s: %d %s", name, res.code, res.body)
			}
		}
		w := h.do(http.MethodPost, threadsPath+"/"+threadAbc+"/messages", opsToken, strings.NewReader("{nope"))
		if w.Code != http.StatusBadRequest {
			t.Errorf("bad JSON: %d", w.Code)
		}
		if turns, _ := h.log.Turns(nsProd, threadAbc); len(turns) != 0 {
			t.Errorf("rejected messages were queued: %+v", turns)
		}
	})

	t.Run("a full thread", func(t *testing.T) {
		h := newHarness(t, nil, threadObject(threadAbc, time.Hour, nil))
		for range threads.MaxTurns {
			if _, err := h.log.Enqueue(nsProd, threadAbc, "x", "token:ops"); err != nil {
				t.Fatal(err)
			}
		}
		res := post(h, threadAbc, map[string]string{"text": "one more"})
		if res.code != http.StatusConflict || !strings.Contains(res.body, "thread has 200 turns; start a new one") {
			t.Errorf("%d %s", res.code, res.body)
		}
	})

	t.Run("a deleting thread", func(t *testing.T) {
		now := metav1.NewTime(epoch)
		h := newHarness(t, nil, threadObject(threadAbc, time.Hour, func(th *v1alpha1.OmpThread) {
			th.DeletionTimestamp, th.Finalizers = &now, []string{v1alpha1.FinalizerCleanup}
		}))
		res := post(h, threadAbc, map[string]string{"text": "hi"})
		if res.code != http.StatusConflict || !strings.Contains(res.body, "thread is being deleted") {
			t.Errorf("%d %s", res.code, res.body)
		}
	})

	t.Run("unknown thread", func(t *testing.T) {
		h := newHarness(t, nil)
		if res := post(h, "coder-nope00", map[string]string{"text": "hi"}); res.code != http.StatusNotFound {
			t.Errorf("%d %s", res.code, res.body)
		}
	})
}

type httpResult struct {
	code int
	body string
}

func TestStop(t *testing.T) {
	h := newHarness(t, nil, threadObject(threadAbc, time.Hour, nil))
	for _, text := range []string{"one", "two", "three"} {
		if _, err := h.log.Enqueue(nsProd, threadAbc, text, "token:ops"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.log.Next(t.Context(), nsProd, threadAbc, "s-1", 0); err != nil {
		t.Fatal(err)
	}
	w := h.do(http.MethodPost, threadsPath+"/"+threadAbc+"/stop", opsToken, nil)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"cancelled":2,"stoppedRunning":true}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	turns, _ := h.log.Turns(nsProd, threadAbc)
	if turns[0].State != v1alpha1.TurnStateRunning || !turns[0].StopRequested || turns[0].StoppedBy != "token:ops" ||
		turns[1].State != v1alpha1.TurnStateCancelled || turns[2].State != v1alpha1.TurnStateCancelled {
		t.Errorf("turns = %+v", turns)
	}
	select {
	case <-h.log.Events():
	case <-time.After(time.Second):
		t.Error("the thread was not signalled")
	}
	w = h.do(http.MethodPost, threadsPath+"/"+threadAbc+"/stop", opsToken, nil)
	if w.Code != http.StatusOK {
		t.Errorf("second stop: %d", w.Code)
	}
}

func TestResultAndConversation(t *testing.T) {
	th := threadObject(threadAbc, time.Hour, func(th *v1alpha1.OmpThread) {
		th.Status.Result = &v1alpha1.ThreadResult{Branch: "omp-cloud/" + threadAbc, Head: handoffSHA, Base: baseSHA, UpdatedAt: metav1.NewTime(epoch)}
	})
	h := newHarness(t, nil, th)
	for _, route := range []struct{ path, msg string }{{"/result", "no result yet"}, {"/conversation", "no conversation yet"}} {
		w := h.do(http.MethodGet, threadsPath+"/"+threadAbc+route.path, viewerToken, nil)
		if w.Code != http.StatusNotFound || errorOf(t, w) != route.msg {
			t.Errorf("%s: %d %s", route.path, w.Code, w.Body)
		}
	}

	if err := h.st.PutThreadFile(nsProd, threadAbc, "result.bundle", strings.NewReader("BUNDLE-BYTES"), store.MaxResultBytes); err != nil {
		t.Fatal(err)
	}
	w := h.do(http.MethodGet, threadsPath+"/"+threadAbc+"/result", viewerToken, nil)
	if w.Code != http.StatusOK || w.Body.String() != "BUNDLE-BYTES" || w.Header().Get("X-Omp-Branch") != "omp-cloud/"+threadAbc ||
		w.Header().Get("X-Omp-Head") != handoffSHA || w.Header().Get("X-Omp-Base") != baseSHA ||
		w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("result: %d %q %v", w.Code, w.Body, w.Header())
	}
	if w := h.do(http.MethodGet, threadsPath+"/"+threadAbc+"/result", viewerToken, nil, "Range", "bytes=0-5"); w.Code != http.StatusPartialContent || w.Body.String() != "BUNDLE" {
		t.Errorf("range: %d %q", w.Code, w.Body)
	}

	var buf strings.Builder
	if err := writeTarGz(&buf, "a.jsonl", strings.NewReader("{}\n"), 3); err != nil {
		t.Fatal(err)
	}
	id := store.ContinuityID("thread/prod/" + threadAbc)
	if err := h.st.PutContinuity(nsProd, agentCoder, id, strings.NewReader(buf.String())); err != nil {
		t.Fatal(err)
	}
	w = h.do(http.MethodGet, threadsPath+"/"+threadAbc+"/conversation", viewerToken, nil)
	if w.Code != http.StatusOK || w.Body.String() != buf.String() || w.Header().Get("Content-Type") != "application/gzip" {
		t.Errorf("conversation: %d %v", w.Code, w.Header())
	}
}

func TestDeleteThread(t *testing.T) {
	h := newHarness(t, nil, threadObject(threadAbc, time.Hour, nil))
	w := h.do(http.MethodDelete, threadsPath+"/"+threadAbc, opsToken, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got v1alpha1.OmpThread
	if err := h.c.Get(t.Context(), types.NamespacedName{Namespace: nsProd, Name: threadAbc}, &got); !apierrors.IsNotFound(err) {
		t.Errorf("thread after delete: %v", err)
	}
	if w := h.do(http.MethodDelete, threadsPath+"/"+threadAbc, opsToken, nil); w.Code != http.StatusNotFound {
		t.Errorf("second delete: %d", w.Code)
	}
}
