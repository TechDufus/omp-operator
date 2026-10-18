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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"

	"github.com/TechDufus/omp-operator/internal/actions"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/driver"
)

const (
	// flagsModel is the model the plan's real-model smoke uses; it must exist in the pinned omp's catalog.
	flagsModel   = "anthropic/claude-sonnet-5-5"
	mcpHeader    = "X-Compat"
	mcpSecret    = "mcp-secret-value"
	gatewayToken = "gateway-token-value"
)

// registryList matches the tool lists omp prints when --tools names a tool it does not have.
var registryList = regexp.MustCompile(`(?:Built-in tools|Other registered tools): ([a-z_, ]+?)\.`)

// finishHostTool is the finish host tool as the driver registers it: an essential (top-level) tool.
func finishHostTool(def agentapi.ToolDef) omprpc.HostTool {
	params := map[string]json.RawMessage{}
	if err := json.Unmarshal(def.Parameters, &params); err != nil {
		panic(err)
	}
	return omprpc.HostTool{
		Definition: omprpc.HostToolDefinition{
			Name:        def.Name,
			Description: def.Description,
			Parameters:  params,
			LoadMode:    omprpc.Ptr(omprpc.ToolLoadModeEssential),
		},
		Handler: func(context.Context, *omprpc.HostToolCall, json.RawMessage) (omprpc.HostToolResultPayload, error) {
			return omprpc.TextResult("finish recorded"), nil
		},
	}
}

func toolNames(state omprpc.SessionState) []string {
	names := make([]string, 0, len(state.DumpTools))
	for _, tool := range state.DumpTools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// mcpProbe is an MCP "server" that records the requests omp sends to it.
type mcpProbe struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []string // value of mcpHeader per request
}

func newMCPProbe(t *testing.T) *mcpProbe {
	t.Helper()
	p := &mcpProbe{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.got = append(p.got, r.Header.Get(mcpHeader))
		p.mu.Unlock()
		http.Error(w, "not an MCP server", http.StatusNotFound)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *mcpProbe) headers() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.got)
}

// TestFlagsAndConfig is plan test 1: omp accepts the driver's exact command line, config.yml,
// models.yml and mcp.json, and a host tool registered as essential is a top-level tool next to
// the allowed built-ins even under --tools.
func TestFlagsAndConfig(t *testing.T) {
	paths := testPaths(t)
	probe := newMCPProbe(t)
	t.Setenv("OMP_MCP_0_0", mcpSecret)
	t.Setenv(agentapi.EnvGatewayToken, gatewayToken)

	probeServer := map[string]any{
		"type": "http", "url": probe.srv.URL, "headers": map[string]string{mcpHeader: "${OMP_MCP_0_0}"},
	}
	mcp, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"probe": probeServer}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := agentapi.SessionConfig{
		Prompt:         "unused",
		SystemPrompt:   "You are a compatibility test agent.",
		Model:          gatewayModel(flagsModel, "http://127.0.0.1:9", "medium"),
		Tools:          agentapi.DefaultTools,
		TimeoutSeconds: 600,
		MCP:            mcp,
		Actions:        []agentapi.ToolDef{finishDef(t)},
	}
	d := newAgentAPI(t, driver.KindSessions).driver(t, driver.KindSessions, paths)
	if err := d.WriteOmpConfig(cfg); err != nil {
		t.Fatal(err)
	}
	args := driver.OmpArgs(cfg, 10*time.Minute, paths)
	t.Logf("omp %v", args)
	client := startOmp(t, paths, args, finishHostTool(cfg.Actions[0]))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	state, err := client.GetState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(state)
	t.Logf("tools: %v", names)
	for _, want := range []string{finishTool, toolBash} {
		if !slices.Contains(names, want) {
			t.Errorf("dumpTools = %v, want %q", names, want)
		}
	}
	// --tools is the allow-list: the allowed built-ins plus the host tool, nothing else.
	allowed := append(slices.Clone(agentapi.DefaultTools), finishTool)
	slices.Sort(allowed)
	if !slices.Equal(names, allowed) {
		t.Errorf("dumpTools = %v, want exactly %v", names, allowed)
	}

	// config.yml: the default role selects the configured model.
	if state.Model == nil || state.Model.Provider != "anthropic" || state.Model.ID != "claude-sonnet-5-5" {
		t.Errorf("model = %+v, want %s (config.yml modelRoles.default)", state.Model, flagsModel)
	}

	// mcp.json: omp reads it, connects to the http server and resolves ${ENV} in headers.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && len(probe.headers()) == 0 {
		time.Sleep(200 * time.Millisecond)
	}
	got := probe.headers()
	if len(got) == 0 {
		t.Fatal("omp never contacted the mcp.json http server")
	}
	for _, h := range got {
		if h != mcpSecret {
			t.Fatalf("MCP request header %s = %q, want the resolved ${OMP_MCP_0_0} value", mcpHeader, h)
		}
	}
}

// reservedFixture is a configured omp home and the driver's command line without its --tools.
func reservedFixture(t *testing.T) (driver.Paths, agentapi.SessionConfig, []string) {
	t.Helper()
	paths := testPaths(t)
	cfg := agentapi.SessionConfig{
		Prompt: "unused", Tools: agentapi.DefaultTools, TimeoutSeconds: 600, Actions: []agentapi.ToolDef{finishDef(t)},
		Model: gatewayModel(flagsModel, "http://127.0.0.1:9", "medium"),
	}
	d := newAgentAPI(t, driver.KindSessions).driver(t, driver.KindSessions, paths)
	if err := d.WriteOmpConfig(cfg); err != nil {
		t.Fatal(err)
	}
	args := driver.OmpArgs(cfg, 10*time.Minute, paths)
	i := slices.Index(args, "--tools")
	if i < 0 {
		t.Fatalf("driver args %v have no --tools", args)
	}
	return paths, cfg, slices.Delete(slices.Clone(args), i, i+2)
}

// TestReservedToolNames is plan test 3: every tool name omp has is reserved, so an OmpAction
// can never shadow one. It checks the tools omp reports for a session started with the driver's
// command line minus --tools, and every tool in omp's registry, which omp lists when --tools
// names one it does not have.
func TestReservedToolNames(t *testing.T) {
	t.Run("get_state without --tools", func(t *testing.T) {
		paths, cfg, args := reservedFixture(t)
		client := startOmp(t, paths, args, finishHostTool(cfg.Actions[0]))
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		state, err := client.GetState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		names := toolNames(state)
		t.Logf("omp reports %d tools: %v", len(names), names)
		for _, want := range []string{toolBash, finishTool} {
			if !slices.Contains(names, want) {
				t.Errorf("tools = %v, want %q", names, want)
			}
		}
		for _, name := range names {
			if !slices.Contains(actions.ReservedToolNames, name) {
				t.Errorf("omp tool %q is not in actions.ReservedToolNames", name)
			}
		}
	})

	t.Run("omp tool registry", func(t *testing.T) {
		paths, _, args := reservedFixture(t)
		cmd := exec.Command(ompBin, append(args, "--tools", "no_such_tool")...)
		cmd.Dir = paths.Workspace
		cmd.Env = append(os.Environ(), "HOME="+paths.Home)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("omp accepted an unknown tool:\n%s", out)
		}
		registry := make([]string, 0, 40)
		for _, m := range registryList.FindAllStringSubmatch(string(out), -1) {
			registry = append(registry, strings.Split(m[1], ", ")...)
		}
		t.Logf("omp registry (%d tools): %v", len(registry), registry)
		if len(registry) < 20 {
			t.Fatalf("could not read omp's tool registry from its error message:\n%s", out)
		}
		for _, name := range registry {
			if !slices.Contains(actions.ReservedToolNames, name) {
				t.Errorf("omp registers tool %q, which is not in actions.ReservedToolNames", name)
			}
		}
	})

	for _, name := range []string{finishTool, toolBash, "read", "web-search", "ast-grep"} {
		if reason, ok := actions.ValidateName(name); ok || reason != actions.ReasonReservedName {
			t.Errorf("ValidateName(%q) = %q, %v; want %s", name, reason, ok, actions.ReasonReservedName)
		}
	}
}
