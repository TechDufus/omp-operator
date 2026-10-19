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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const askSitting = threadAbc + "-1"

// askingThread is a thread with a running turn that asked two questions of one ask
// (a single-select with options, a free-text one).
func askingThread(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, nil, threadObject(threadAbc, time.Hour, func(th *v1alpha1.OmpThread) {
		th.Status.Phase = v1alpha1.ThreadPhaseWorking
	}))
	if _, err := h.log.Enqueue(nsProd, threadAbc, "work", "token:ops"); err != nil {
		t.Fatal(err)
	}
	if got, err := h.log.Next(t.Context(), nsProd, threadAbc, askSitting, 0); err != nil || got == nil {
		t.Fatalf("next: %v %v", got, err)
	}
	rec := 1
	if _, _, err := h.log.Ask(nsProd, threadAbc, askSitting, 1, []threads.QuestionInput{
		{Key: "db", Text: "Which database?", Header: "DB", Options: []threads.QuestionOption{{Label: "sqlite", Description: "embedded"}, {Label: "postgres"}}, Recommended: &rec},
		{Key: "why", Text: "Why?"},
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func answerPath(turn, qid string) string {
	return threadsPath + "/" + threadAbc + "/turns/" + turn + "/questions/" + qid + "/answer"
}

func drain(h *harness) {
	for len(h.log.Events()) > 0 {
		<-h.log.Events()
	}
}

func TestAnswerQuestion(t *testing.T) {
	h := askingThread(t)
	drain(h)

	w := h.do(http.MethodPost, answerPath("1", "1"), opsToken, jsonBody(t, map[string]any{"selected": []string{"sqlite"}, "answer": "  fast  "}))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	q := decode[threads.Question](t, w)
	if q.ID != 1 || q.Ask != 1 || len(q.Selected) != 1 || q.Selected[0] != "sqlite" || q.Answer != "fast" || q.AnsweredBy != "token:ops" ||
		q.AnsweredAt == nil || q.Expired {
		t.Errorf("answered = %+v", q)
	}
	select {
	case ev := <-h.log.Events():
		if ev.Object.GetNamespace() != nsProd || ev.Object.GetName() != threadAbc {
			t.Errorf("signalled %s/%s", ev.Object.GetNamespace(), ev.Object.GetName())
		}
	default:
		t.Error("answering must signal the thread")
	}
	stored, _ := h.log.Turns(nsProd, threadAbc)
	if got := stored[0].Questions[0]; got.AnsweredBy != "token:ops" || got.Selected[0] != "sqlite" {
		t.Errorf("stored = %+v", got)
	}

	tests := []struct {
		name  string
		path  string
		token string
		body  any
		want  int
		msg   string
	}{
		{"already answered", answerPath("1", "1"), opsToken, map[string]any{"answer": "again"}, 409, "question is not waiting for an answer"},
		{"unknown question", answerPath("1", "9"), opsToken, map[string]any{"answer": "x"}, 404, "not found"},
		{"unknown turn", answerPath("5", "1"), opsToken, map[string]any{"answer": "x"}, 404, "not found"},
		{"non-numeric ids", answerPath("one", "two"), opsToken, map[string]any{"answer": "x"}, 404, "not found"},
		{"nothing given", answerPath("1", "2"), opsToken, map[string]any{}, 400, "invalid answer"},
		{"not an option", answerPath("1", "1"), opsToken, map[string]any{"selected": []string{"x"}}, 409, "not waiting"},
		{"too long", answerPath("1", "2"), opsToken, map[string]any{"answer": strings.Repeat("x", threads.MaxQuestionTextBytes+1)}, 400, "longer than"},
		{"viewer", answerPath("1", "2"), viewerToken, map[string]any{"answer": "x"}, 403, "operator role required"},
		{"no grant", answerPath("1", "2"), nobodyToken, map[string]any{"answer": "x"}, 404, "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := h.do(http.MethodPost, tt.path, tt.token, jsonBody(t, tt.body))
			if w.Code != tt.want || !strings.Contains(errorOf(t, w), tt.msg) {
				t.Errorf("%d %s, want %d containing %q", w.Code, w.Body, tt.want, tt.msg)
			}
		})
	}
	if w := h.do(http.MethodPost, answerPath("1", "2"), opsToken, strings.NewReader("{nope")); w.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: %d", w.Code)
	}
	huge := `{"answer":"` + strings.Repeat("x", 3*threads.MaxQuestionTextBytes+1<<20) + `"}`
	if w := h.do(http.MethodPost, answerPath("1", "2"), opsToken, strings.NewReader(huge)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge body: %d", w.Code)
	}
	// A label that is not an option of the question is a 400.
	if w := h.do(http.MethodPost, answerPath("1", "2"), opsToken, jsonBody(t, map[string]any{"selected": []string{"x"}})); w.Code != http.StatusBadRequest ||
		!strings.Contains(errorOf(t, w), `"x" is not an option`) {
		t.Errorf("unknown label: %d %s", w.Code, w.Body)
	}
	stored, _ = h.log.Turns(nsProd, threadAbc)
	if stored[0].Questions[1].Answered() {
		t.Errorf("a rejected answer was stored: %+v", stored[0].Questions[1])
	}
}

func TestAnswerAfterStop(t *testing.T) {
	h := askingThread(t)
	if w := h.do(http.MethodPost, threadsPath+"/"+threadAbc+"/stop", opsToken, nil); w.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	// Stopping expired the questions, so nobody can answer them any more.
	if w := h.do(http.MethodPost, answerPath("1", "1"), opsToken, jsonBody(t, map[string]any{"answer": "late"})); w.Code != http.StatusConflict {
		t.Errorf("answer after stop: %d %s", w.Code, w.Body)
	}
	stored, _ := h.log.Turns(nsProd, threadAbc)
	if q := stored[0].Questions[0]; !q.Expired || q.AnsweredBy != "stop" {
		t.Errorf("question = %+v", q)
	}
}

func TestSummaryQuestion(t *testing.T) {
	h := askingThread(t)
	// A thread whose status still says Working shows as Waiting once the log has an open question.
	for _, route := range []string{"get", "list"} {
		path := threadsPath
		if route == "get" {
			path += "/" + threadAbc
		}
		resp := h.do(http.MethodGet, path, viewerToken, nil)
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", route, resp.Code, resp.Body)
		}
		var phase v1alpha1.ThreadPhase
		var q *Question
		if route == "list" {
			sum := decode[struct{ Threads []ThreadSummary }](t, resp).Threads[0]
			phase, q = sum.Phase, sum.Question
		} else {
			d := decode[ThreadDetail](t, resp)
			phase, q = d.Phase, d.Question
		}
		if phase != v1alpha1.ThreadPhaseWaiting || q == nil {
			t.Fatalf("%s: phase %q question %+v", route, phase, q)
		}
		if q.Turn != 1 || q.ID != 1 || q.Ask != 1 || q.Text != "Which database?" || q.Header != "DB" || q.Multi || q.Remaining != 2 ||
			q.Recommended == nil || *q.Recommended != 1 || len(q.Options) != 2 || q.Options[0].Description != "embedded" || q.AskedAt.IsZero() {
			t.Errorf("%s: question = %+v", route, q)
		}
	}
}

func TestSummaryQuestionFollowsAnswers(t *testing.T) {
	h := askingThread(t)
	get := func() (*httptest.ResponseRecorder, ThreadDetail) {
		t.Helper()
		w := h.do(http.MethodGet, threadsPath+"/"+threadAbc, opsToken, nil)
		return w, decode[ThreadDetail](t, w)
	}

	// Answer the first: the second of the same ask is next, one remaining.
	if _, err := h.log.Answer(nsProd, threadAbc, 1, 1, []string{"sqlite"}, "", "token:ops"); err != nil {
		t.Fatal(err)
	}
	w, d := get()
	if d.Question == nil || d.Question.ID != 2 || d.Question.Remaining != 1 || len(d.Question.Options) != 0 {
		t.Errorf("after the first answer: %+v", d.Question)
	}
	if !strings.Contains(w.Body.String(), `"options":[]`) {
		t.Errorf("options is not an empty list: %.400s", w.Body)
	}
	if len(d.Turns) != 1 || len(d.Turns[0].Questions) != 2 || d.Turns[0].Questions[0].Selected[0] != "sqlite" || d.Turns[0].Questions[1].Answered() {
		t.Errorf("turn questions = %+v", d.Turns)
	}

	// All answered: no question, back to the status phase.
	if _, err := h.log.Answer(nsProd, threadAbc, 1, 2, nil, "because", "token:ops"); err != nil {
		t.Fatal(err)
	}
	w, d = get()
	if d.Question != nil || d.Phase != v1alpha1.ThreadPhaseWorking {
		t.Errorf("answered: phase %q question %+v", d.Phase, d.Question)
	}
	if !strings.Contains(w.Body.String(), `"question":null`) {
		t.Errorf("question is not an explicit null: %.300s", w.Body)
	}
}

func TestSummaryQuestionOfIdleThreadsIsNull(t *testing.T) {
	h := newHarness(t, nil, threadObject(threadAbc, time.Hour, func(th *v1alpha1.OmpThread) {
		th.Status.Phase = v1alpha1.ThreadPhaseIdle
	}))
	w := h.do(http.MethodGet, threadsPath, opsToken, nil)
	if !strings.Contains(w.Body.String(), `"question":null`) || !strings.Contains(w.Body.String(), `"phase":"Idle"`) {
		t.Errorf("idle summary: %s", w.Body)
	}
}

func TestWhoAmICapacityAndThreadCounts(t *testing.T) {
	session := func(name, agent string, phase v1alpha1.SessionPhase) *v1alpha1.OmpSession {
		return &v1alpha1.OmpSession{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsProd, Name: name},
			Spec:       v1alpha1.OmpSessionSpec{AgentRef: v1alpha1.LocalObjectRef{Name: agent}, Prompt: "p"},
			Status:     v1alpha1.OmpSessionStatus{Phase: phase},
		}
	}
	thread := func(name, agent string, phase v1alpha1.ThreadPhase) *v1alpha1.OmpThread {
		return threadObject(name, time.Hour, func(th *v1alpha1.OmpThread) {
			th.Spec.AgentRef.Name = agent
			th.Status.Phase = phase
		})
	}
	h := newHarness(t, nil,
		session("s1", agentCoder, v1alpha1.SessionPhaseRunning),
		session("s2", agentCoder, v1alpha1.SessionPhaseRunning),
		session("s3", agentCoder, v1alpha1.SessionPhasePending),
		session("s4", agentCoder, v1alpha1.SessionPhaseSucceeded),
		session("s5", "nogateway", v1alpha1.SessionPhaseRunning),
		thread("coder-w00001", agentCoder, v1alpha1.ThreadPhaseWorking),
		thread("coder-w00002", agentCoder, ""),
		thread("coder-q00001", agentCoder, v1alpha1.ThreadPhaseWaiting),
		thread("coder-i00001", agentCoder, v1alpha1.ThreadPhaseIdle),
		thread("coder-i00002", agentCoder, v1alpha1.ThreadPhaseIdle),
		thread("coder-i00003", agentCoder, v1alpha1.ThreadPhaseIdle),
		thread("nogateway-i00001", "nogateway", v1alpha1.ThreadPhaseIdle),
	)
	// The coder agent runs four sessions at once.
	var coder v1alpha1.OmpAgent
	if err := h.c.Get(t.Context(), types.NamespacedName{Namespace: nsProd, Name: agentCoder}, &coder); err != nil {
		t.Fatal(err)
	}
	coder.Spec.Limits.MaxConcurrentSessions = 4
	if err := h.c.Update(t.Context(), &coder); err != nil {
		t.Fatal(err)
	}

	w := h.do(http.MethodGet, "/api/v1/whoami", opsToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := map[string]AgentInfo{}
	for _, a := range decode[WhoAmI](t, w).Agents {
		got[a.Name] = a
	}
	if c := got[agentCoder]; c.Capacity != (Capacity{MaxConcurrentSessions: 4, Running: 2}) || c.Threads != (ThreadCounts{Working: 2, Waiting: 1, Idle: 3}) {
		t.Errorf("coder = capacity %+v threads %+v", c.Capacity, c.Threads)
	}
	// An agent without maxConcurrentSessions runs one at a time; other agents' sessions and threads do not leak in.
	if n := got["nogateway"]; n.Capacity != (Capacity{MaxConcurrentSessions: 1, Running: 1}) || n.Threads != (ThreadCounts{Idle: 1}) {
		t.Errorf("nogateway = capacity %+v threads %+v", n.Capacity, n.Threads)
	}
	if d := got["down"]; d.Capacity.Running != 0 || d.Threads != (ThreadCounts{}) {
		t.Errorf("down = %+v", d)
	}
	for _, want := range []string{`"capacity":{"maxConcurrentSessions":4,"running":2}`, `"threads":{"working":2,"waiting":1,"idle":3}`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("whoami lacks %s: %s", want, w.Body)
		}
	}
}
