//go:build ompcompat

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

package ompcompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/driver"
	"github.com/TechDufus/omp-operator/test/fakeprovider"
)

const systemMarker = "COMPAT-SYSTEM-PROMPT"

// TestScriptedTurn is plan test 2: the driver's Run loop against the real omp, a fake
// openai-completions provider and an httptest agent API. The model calls finish once, the call
// reaches the API as an action, and the run is reported complete with finishCalled.
func TestScriptedTurn(t *testing.T) {
	model := fakeprovider.New(t, fakeprovider.WithToolCall(finishTool, finishArgs))
	paths := testPaths(t)
	writeFile(t, paths.OmpAgentDir+"/models.yml", model.ModelsYAML())

	api := newAgentAPI(t, driver.KindSessions)
	api.cfg = agentapi.SessionConfig{
		Prompt:         "Call finish with verdict ok.",
		SystemPrompt:   systemMarker,
		Model:          agentapi.ModelConfig{Default: fakeprovider.Model, Thinking: "off"},
		Tools:          agentapi.DefaultTools,
		TimeoutSeconds: 120,
		Actions:        []agentapi.ToolDef{finishDef(t)},
	}
	d := api.driver(t, driver.KindSessions, paths)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	api.snapshot(func() {
		if len(api.completes) != 1 {
			t.Fatalf("complete reports = %+v", api.completes)
		}
		done := api.completes[0]
		if done.Outcome != agentapi.OutcomeCompleted || !done.FinishCalled || done.Error != "" {
			t.Errorf("complete = %+v, want completed with finishCalled", done)
		}
		// Two model calls (the finish call, then "done"), 120 in / 12 out each.
		if done.Usage.InputTokens != 240 || done.Usage.OutputTokens != 24 {
			t.Errorf("usage = %+v, want 240 in / 24 out", done.Usage)
		}
		if len(api.actions) != 1 || api.actions[0].Tool != finishTool {
			t.Fatalf("action calls = %+v, want exactly one finish", api.actions)
		}
		var args map[string]string
		err := json.Unmarshal([]byte(api.actions[0].Body), &args)
		if err != nil || args["verdict"] != "ok" || args["title"] != "compat" {
			t.Errorf("finish args = %s (%v)", api.actions[0].Body, err)
		}
		if !strings.Contains(string(api.transcript), "<html") {
			t.Errorf("transcript = %.80q, want exported HTML", api.transcript)
		}
	})

	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model saw %d requests, want 2 (finish call, then done):\n%v", len(reqs), model.RawRequests())
	}
	// The session's tools reached the model: allowed built-ins plus the essential host tool.
	first := reqs[0].ToolNames()
	for _, want := range append(slices.Clone(agentapi.DefaultTools), finishTool) {
		if !slices.Contains(first, want) {
			t.Errorf("first request tools = %v, want %q", first, want)
		}
	}
	if !strings.Contains(model.RawRequests()[0], systemMarker) {
		t.Errorf("--append-system-prompt text %q missing from the first request", systemMarker)
	}
	// The host tool's result went back to the model.
	if !strings.Contains(model.RawRequests()[1], "finish recorded") {
		t.Errorf("second request does not carry the finish result:\n%s", model.RawRequests()[1])
	}
}

// gatewayRecorder is a stand-in auth-gateway that records requests and answers every one with
// an error status (401 unless status is set).
type gatewayRecorder struct {
	srv    *httptest.Server
	status int
	body   string
	mu     sync.Mutex
	got    []string // "METHOD path auth"
}

func newGatewayRecorder(t *testing.T) *gatewayRecorder {
	t.Helper()
	g := &gatewayRecorder{status: http.StatusUnauthorized, body: `{"error":{"message":"gateway says no"}}`}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		g.mu.Lock()
		g.got = append(g.got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.status)
		_, _ = io.WriteString(w, g.body)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gatewayRecorder) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.got)
}

// runGatewayModel runs the driver's Run loop with a model routed through the gateway at url and
// returns the one complete report.
func runGatewayModel(t *testing.T, url, token string) agentapi.Complete {
	t.Helper()
	t.Setenv(agentapi.EnvGatewayToken, token)
	paths := testPaths(t)
	api := newAgentAPI(t, driver.KindSessions)
	api.cfg = agentapi.SessionConfig{
		Prompt:         "hello",
		Model:          gatewayModel(flagsModel, url, "off"),
		Tools:          agentapi.DefaultTools,
		TimeoutSeconds: 120,
		Actions:        []agentapi.ToolDef{finishDef(t)},
	}
	d := api.driver(t, driver.KindSessions, paths)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var done agentapi.Complete
	api.snapshot(func() {
		if len(api.completes) != 1 {
			t.Fatalf("complete reports = %+v", api.completes)
		}
		done = api.completes[0]
	})
	t.Logf("complete = %+v", done)
	return done
}

// TestGatewayProviderShape confirms the models.yml the driver writes for a gateway: omp sends
// the model request to <gateway url>/v1/pi/stream with the gateway token as bearer, and a 401 or
// 403 from the gateway (answered as {"error": "<text>"}, like omp's own gateway) ends the run as
// an auth failure although omp reports no httpStatus for gateway errors.
func TestGatewayProviderShape(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			gw := newGatewayRecorder(t)
			gw.status = status
			gw.body = `{"error":"gateway says no"}`
			done := runGatewayModel(t, gw.srv.URL, gatewayToken)

			got := gw.requests()
			want := "POST /v1/pi/stream Bearer " + gatewayToken
			if !slices.Contains(got, want) {
				t.Fatalf("gateway requests = %q, want one %q", got, want)
			}
			if done.Outcome != agentapi.OutcomeAuth {
				t.Errorf("outcome = %q, want %q for a gateway %d", done.Outcome, agentapi.OutcomeAuth, status)
			}
			if strings.Contains(done.Error, gatewayToken) {
				t.Errorf("error leaks the gateway token: %q", done.Error)
			}
		})
	}
}
