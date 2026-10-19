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

package agentapi

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentspec"
	"github.com/TechDufus/omp-operator/internal/cloudspec"
	"github.com/TechDufus/omp-operator/internal/gateway"
	"github.com/TechDufus/omp-operator/internal/prompt"
)

const (
	// DefaultSessionTimeout is limits.sessionTimeout when unset.
	DefaultSessionTimeout = 30 * time.Minute
	// DefaultThinking is model.thinking when unset.
	DefaultThinking = v1alpha1.ThinkingMedium
)

// DefaultTools is tools.allow when unset: omp built-in tools the agent may use.
var DefaultTools = []string{"read", "grep", "glob", "bash", "todo"}

// sessionConfig builds the SessionConfig of a session. thread is the OmpThread of a thread
// sitting, nil for other sessions.
func (s *Server) sessionConfig(ctx context.Context, agent *v1alpha1.OmpAgent, sess *v1alpha1.OmpSession, thread *v1alpha1.OmpThread) (*SessionConfig, error) {
	defs, _, err := s.Runner.ToolDefs(ctx, agent)
	if err != nil {
		return nil, fmt.Errorf("tool definitions: %w", err)
	}
	spec := &agent.Spec
	if thread != nil {
		// finish ends a session; thread sittings end by idling, so they do not get it.
		defs = slices.DeleteFunc(defs, func(d ToolDef) bool { return d.Name == FinishToolName })
	}

	creds := make([]prompt.Credential, 0, len(spec.Credentials))
	for _, c := range spec.Credentials {
		creds = append(creds, prompt.Credential{Name: c.Name, Description: c.Description, Env: c.Env, File: c.File})
	}
	tools := make([]prompt.Tool, 0, len(defs))
	for _, d := range defs {
		tools = append(tools, prompt.Tool{Name: d.Name, Description: d.Description})
	}

	mcp, mcpRedact, err := renderMCP(spec.MCPServers)
	if err != nil {
		return nil, err
	}

	cfg := &SessionConfig{
		Prompt:         sess.Spec.Prompt,
		SystemPrompt:   prompt.SystemPrompt(spec.Instructions, creds, tools),
		Model:          modelConfig(&spec.Model),
		Tools:          toolAllow(spec),
		TimeoutSeconds: int64(sessionTimeout(spec) / time.Second),
		MCP:            mcp,
		Actions:        defs,
		Redact:         redactSources(spec, mcpRedact),
		Continuity:     sess.Spec.ContinuityKey != "",
	}
	if thread != nil {
		applyThread(cfg, agent, thread)
	}
	return cfg, nil
}

// applyThread adapts cfg to a sitting of thread: the thread's model and thinking level, the
// cloud system prompt section, the shipped secrets to redact, and the turn-loop settings.
func applyThread(cfg *SessionConfig, agent *v1alpha1.OmpAgent, thread *v1alpha1.OmpThread) {
	spec := &agent.Spec
	var cloud v1alpha1.CloudSpec
	if spec.Cloud != nil {
		cloud = *spec.Cloud
	}
	if m := thread.Spec.Model; m != "" {
		cfg.Model.Default = m
	}
	if gw := cfg.Model.Gateway; gw != nil {
		gw.Providers = providers(slices.Concat([]string{cfg.Model.Default, spec.Model.Small}, gateway.AllowedModels(agent))...)
	}
	cfg.Continuity = true

	_, workspace := cloudspec.PodPaths(thread.Spec.Paths)
	host, _, _ := cloudspec.MatchGitHost(cloud.Git, thread.Spec.Repo.RemoteURL)
	push := host != nil && host.Push

	cfg.SystemPrompt = joinSections(cfg.SystemPrompt, prompt.CloudSection(prompt.Cloud{
		Repo:            thread.Spec.Repo.Name,
		Workspace:       workspace,
		Branch:          cloudspec.Branch(thread.Name),
		LocalBranch:     thread.Spec.Repo.LocalBranch,
		Push:            push,
		SecretFiles:     thread.Spec.SecretFiles,
		QuestionTimeout: cloudspec.QuestionTimeout(&cloud),
	}))
	for _, name := range thread.Spec.SecretEnv {
		cfg.Redact = append(cfg.Redact, RedactSource{Name: "thread-env-" + name, Env: name})
	}
	for i, p := range thread.Spec.SecretFiles {
		cfg.Redact = append(cfg.Redact, RedactSource{
			Name: "thread-file-" + strconv.Itoa(i), File: path.Join(workspace, p), Lines: true,
		})
	}

	cfg.Thread = &ThreadConfig{
		Name:                   thread.Name,
		Branch:                 cloudspec.Branch(thread.Name),
		BaseCommit:             thread.Spec.Repo.BaseCommit,
		RepoName:               thread.Spec.Repo.Name,
		LocalBranch:            thread.Spec.Repo.LocalBranch,
		Thinking:               thread.Spec.Thinking,
		TurnTimeoutSeconds:     cfg.TimeoutSeconds,
		IdleTimeoutSeconds:     int64(cloudspec.IdleTimeout(&cloud) / time.Second),
		MaxSittingSeconds:      int64(cloudspec.MaxSitting(&cloud) / time.Second),
		QuestionTimeoutSeconds: int64(cloudspec.QuestionTimeout(&cloud) / time.Second),
		Push:                   push,
	}
}

func joinSections(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

func sessionTimeout(spec *v1alpha1.OmpAgentSpec) time.Duration {
	if d := spec.Limits.SessionTimeout.Duration; d > 0 {
		return d
	}
	return DefaultSessionTimeout
}

// toolAllow is the omp built-in tools the session may use; nil means all of them
// (tools.allow is exactly ["*"]), so no tool list is passed to omp.
func toolAllow(spec *v1alpha1.OmpAgentSpec) []string {
	switch {
	case len(spec.Tools.Allow) == 0:
		return slices.Clone(DefaultTools)
	case len(spec.Tools.Allow) == 1 && spec.Tools.Allow[0] == agentspec.AllTools:
		return nil
	}
	return slices.Clone(spec.Tools.Allow)
}

func modelConfig(m *v1alpha1.ModelSpec) ModelConfig {
	thinking := string(m.Thinking)
	if thinking == "" {
		thinking = string(DefaultThinking)
	}
	cfg := ModelConfig{Default: m.Default, Small: m.Small, Thinking: thinking}
	if m.Gateway != nil {
		cfg.Gateway = &GatewayConfig{URL: m.Gateway.URL, Providers: providers(m.Default, m.Small)}
	}
	return cfg
}

// providers returns the distinct provider prefixes of "provider/id" models, in order.
func providers(models ...string) []string {
	out := []string{}
	for _, m := range models {
		p, _, ok := strings.Cut(m, "/")
		if !ok || p == "" || slices.Contains(out, p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// redactSources lists every secret value the driver must scrub: credentials, MCP secrets and the gateway token.
func redactSources(spec *v1alpha1.OmpAgentSpec, mcp []RedactSource) []RedactSource {
	out := make([]RedactSource, 0, len(spec.Credentials)+len(mcp)+1)
	for _, c := range spec.Credentials {
		out = append(out, RedactSource{Name: c.Name, Env: c.Env, File: c.File})
	}
	out = append(out, mcp...)
	if spec.Model.Gateway != nil {
		out = append(out, RedactSource{Name: "gateway-token", Env: EnvGatewayToken})
	}
	return out
}

// mcpServerEntry is one entry of mcp.json's mcpServers map.
type mcpServerEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// mcpServerType returns the transport: the explicit type, else stdio with a command and http with a url.
func mcpServerType(s *v1alpha1.MCPServer) v1alpha1.MCPServerType {
	switch {
	case s.Type != "":
		return s.Type
	case len(s.Command) > 0:
		return v1alpha1.MCPStdio
	case s.URL != "":
		return v1alpha1.MCPHTTP
	}
	return v1alpha1.MCPStdio
}

// renderMCP renders omp's mcp.json for the servers. Literal values are written as they are;
// secret values become ${OMP_MCP_<n>_<m>} placeholders (n = server index, m = env or header
// index) resolved from the main container's environment. The redact sources name those variables.
// No servers renders nil.
func renderMCP(servers []v1alpha1.MCPServer) (json.RawMessage, []RedactSource, error) {
	if len(servers) == 0 {
		return nil, nil, nil
	}
	entries := make(map[string]mcpServerEntry, len(servers))
	var redact []RedactSource
	for n := range servers {
		srv := &servers[n]
		typ := mcpServerType(srv)
		e := mcpServerEntry{Type: string(typ)}
		var pairs []v1alpha1.ValueOrSecret
		switch typ {
		case v1alpha1.MCPStdio:
			if len(srv.Command) == 0 {
				return nil, nil, fmt.Errorf("mcpServers[%d] %q: stdio server needs a command", n, srv.Name)
			}
			e.Command = srv.Command[0]
			e.Args = srv.Command[1:]
			pairs = srv.Env
		case v1alpha1.MCPHTTP, v1alpha1.MCPSSE:
			e.URL = srv.URL
			pairs = srv.Headers
		}
		values := make(map[string]string, len(pairs))
		for m, p := range pairs {
			switch {
			case p.SecretRef != nil:
				env := MCPEnvName(n, m)
				values[p.Name] = "${" + env + "}"
				redact = append(redact, RedactSource{Name: "mcp-" + srv.Name + "-" + p.Name, Env: env})
			case p.Value != nil:
				values[p.Name] = *p.Value
			}
		}
		if len(values) > 0 {
			if typ == v1alpha1.MCPStdio {
				e.Env = values
			} else {
				e.Headers = values
			}
		}
		entries[srv.Name] = e
	}
	out, err := json.Marshal(map[string]any{"mcpServers": entries})
	if err != nil {
		return nil, nil, fmt.Errorf("render mcp.json: %w", err)
	}
	return out, redact, nil
}
