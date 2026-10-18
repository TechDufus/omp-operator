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
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

func finishPayload() map[string]any {
	return map[string]any{
		"verdict": "fixed", "title": "Restarted web", "summary": "web was wedged; restarted",
		"report": "## Report\nlong form", "proposed_commands": []string{"kubectl get pods"},
	}
}

func TestFinishRecordsAndRefusesSecond(t *testing.T) {
	r, c := newRunner(t, nil, newSession(testSession))
	agent := newAgent()

	res := call(t, r, agent, FinishTool, finishPayload())
	if res.IsError || res.Text != "finish recorded" {
		t.Fatalf("first finish = %+v", res)
	}
	s := getSession(t, c)
	if s.Status.Verdict != v1alpha1.VerdictFixed || s.Status.Title != "Restarted web" ||
		s.Status.Summary != "web was wedged; restarted" || !slices.Equal(s.Status.ProposedCommands, []string{"kubectl get pods"}) {
		t.Fatalf("status = %+v", s.Status)
	}
	report, err := r.Store.ReadReport(testUID)
	if err != nil || report != "## Report\nlong form" {
		t.Fatalf("report = %q, %v", report, err)
	}

	second := finishPayload()
	second["verdict"], second["report"] = "ok", "overwritten"
	res = call(t, r, agent, FinishTool, second)
	if !res.IsError || res.Text != "finish already recorded" {
		t.Fatalf("second finish = %+v", res)
	}
	s = getSession(t, c)
	if s.Status.Verdict != v1alpha1.VerdictFixed {
		t.Fatalf("verdict changed to %q", s.Status.Verdict)
	}
	if report, _ := r.Store.ReadReport(testUID); report != "## Report\nlong form" {
		t.Fatalf("second finish overwrote the report: %q", report)
	}
	events, err := r.Store.ReadEvents(testUID, 10)
	if err != nil || len(events) != 2 || events[0].Kind != "finish" {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if len(s.Status.Actions) != 0 {
		t.Fatalf("finish must not be recorded in status.actions: %+v", s.Status.Actions)
	}
}

func TestFinishValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"bad verdict", func(m map[string]any) { m["verdict"] = "done" }, "verdict must be one of"},
		{"empty title", func(m map[string]any) { m["title"] = "" }, "title must be 1-80"},
		{"long title", func(m map[string]any) { m["title"] = strings.Repeat("t", 81) }, "title must be 1-80"},
		{"empty summary", func(m map[string]any) { m["summary"] = "  " }, "summary must be 1-3000"},
		{"long summary", func(m map[string]any) { m["summary"] = strings.Repeat("s", 3001) }, "summary must be 1-3000"},
		{"empty report", func(m map[string]any) { m["report"] = "" }, "report must be 1-20000"},
		{"long report", func(m map[string]any) { m["report"] = strings.Repeat("r", 20001) }, "report must be 1-20000"},
		{"eleven commands", func(m map[string]any) { m["proposed_commands"] = make([]string, 11) }, "at most 10"},
		{"long command", func(m map[string]any) { m["proposed_commands"] = []string{strings.Repeat("c", 501)} }, "at most 500"},
		{"unknown field", func(m map[string]any) { m["extra"] = 1 }, "invalid arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, c := newRunner(t, nil, newSession(testSession))
			p := finishPayload()
			tt.mutate(p)
			res := call(t, r, newAgent(), FinishTool, p)
			if !res.IsError || !strings.HasPrefix(res.Text, "refused: ") || !strings.Contains(res.Text, tt.want) {
				t.Fatalf("result = %+v, want refusal containing %q", res, tt.want)
			}
			if v := getSession(t, c).Status.Verdict; v != "" {
				t.Fatalf("invalid finish set verdict %q", v)
			}
			// A refused finish does not use up the one allowed call.
			if res := call(t, r, newAgent(), FinishTool, finishPayload()); res.IsError {
				t.Fatalf("valid finish after a refusal = %+v", res)
			}
		})
	}
}

func TestFinishLimitsAreAtTheBoundary(t *testing.T) {
	r, _ := newRunner(t, nil, newSession(testSession))
	p := finishPayload()
	p["title"] = strings.Repeat("é", 80)
	p["summary"] = strings.Repeat("s", 3000)
	p["report"] = strings.Repeat("r", 20000)
	p["proposed_commands"] = slices.Repeat([]string{strings.Repeat("c", 500)}, 10)
	if res := call(t, r, newAgent(), FinishTool, p); res.IsError {
		t.Fatalf("limits are inclusive: %+v", res)
	}
}

func TestFinishDefinition(t *testing.T) {
	d := finishDef()
	if d.Name != "finish" {
		t.Fatalf("name = %q", d.Name)
	}
	var s map[string]any
	if err := json.Unmarshal(d.Parameters, &s); err != nil {
		t.Fatal(err)
	}
	req, _ := s["required"].([]any)
	if len(req) != 4 || s["type"] != "object" {
		t.Fatalf("schema = %v", s)
	}
}
