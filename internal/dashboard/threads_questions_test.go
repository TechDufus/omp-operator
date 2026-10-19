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

package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/TechDufus/omp-operator/internal/threads"
)

const hostileQuestion = `Use <script>alert("q")</script> or not?`

func TestThreadPageQuestions(t *testing.T) {
	f := threadFixture(t)
	log := f.srv.Threads
	const (
		name    = "hello-work01"
		sitting = "hello-work01-1"
	)
	if got, err := log.Next(t.Context(), nsProd, name, sitting, 0); err != nil || got == nil {
		t.Fatalf("next: %v %v", got, err)
	}
	rec := 0
	if _, _, err := log.Ask(nsProd, name, sitting, 1, []threads.QuestionInput{
		{Key: "a", Text: hostileQuestion, Header: "<b>Header</b>", Recommended: &rec,
			Options: []threads.QuestionOption{{Label: "<i>yes</i>", Description: "do it"}, {Label: "no"}}},
		{Key: "b", Text: "Which branch?"},
		{Key: "c", Text: "Free text?"},
	}); err != nil {
		t.Fatal(err)
	}
	page := func() string {
		t.Helper()
		w := do(f.h, http.MethodGet, "/threads/prod/"+name, asAlice())
		if w.Code != http.StatusOK {
			t.Fatalf("code %d", w.Code)
		}
		return w.Body.String()
	}

	p := page()
	for _, want := range []string{
		"Waiting for your answer", "Turn 1, question 1", "3 questions in this ask are open",
		"Use &lt;script&gt;alert(&#34;q&#34;)&lt;/script&gt; or not?", "&lt;b&gt;Header&lt;/b&gt;", "&lt;i&gt;yes&lt;/i&gt;", "(recommended)", "do it",
		"/cloud answer hello-work01", `<span class="Waiting">Waiting</span>`, `<tr class="pending">`, "waiting for an answer",
		`<meta http-equiv="refresh" content="10">`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("waiting page missing %q", want)
		}
	}
	for _, raw := range []string{"<script>alert", "<b>Header</b>", "<i>yes</i>"} {
		if strings.Contains(p, raw) {
			t.Errorf("page contains %q unescaped", raw)
		}
	}
	// Only the oldest question is highlighted.
	if n := strings.Count(p, `<tr class="pending">`); n != 1 {
		t.Errorf("%d highlighted rows, want 1", n)
	}

	// Answer the first with an option and a hostile free text, expire the rest via stop.
	if _, err := log.Answer(nsProd, name, 1, 1, []string{"<i>yes</i>"}, `fine </pre><img src=x onerror=alert(1)>`, "token:dev"); err != nil {
		t.Fatal(err)
	}
	p = page()
	for _, want := range []string{
		"Turn 1, question 2", "2 questions in this ask are open", "Which branch?",
		"answered", "<strong>&lt;i&gt;yes&lt;/i&gt;</strong>", "fine &lt;/pre&gt;&lt;img src=x onerror=alert(1)&gt;", "by token:dev",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("page after the first answer missing %q", want)
		}
	}
	if strings.Contains(p, "<img src=x") {
		t.Error("free-text answer is not escaped")
	}

	if _, _, err := log.Stop(nsProd, name, "token:dev"); err != nil {
		t.Fatal(err)
	}
	p = page()
	if strings.Contains(p, "Waiting for your answer") || strings.Contains(p, `<tr class="pending">`) {
		t.Error("a stopped thread still shows a pending question")
	}
	if !strings.Contains(p, "expired (stop)") {
		t.Errorf("expired questions not shown as expired")
	}
}

func TestThreadPageWithoutQuestions(t *testing.T) {
	f := threadFixture(t)
	p := do(f.h, http.MethodGet, threadPath, asAlice()).Body.String()
	if strings.Contains(p, "<h2>Questions</h2>") || strings.Contains(p, "Waiting for your answer") {
		t.Error("a thread without questions shows a question section")
	}
}
