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

// Package agentapi is the HTTP API that session and toolsync pods call on the operator
// (default :8083), and the wire types and in-pod paths both sides share.
package agentapi

import (
	"encoding/json"
	"fmt"
	"time"
)

// Paths and environment variables shared by the pod builder and the in-pod driver.
const (
	// RunDir holds the token, auth material, ConfigMap skills and the kubeconfig.
	RunDir = "/var/run/omp-operator"
	// TokenFile is the bearer token for the agent API (Secret key "token").
	TokenFile = RunDir + "/token/token"
	// KubeconfigFile is written by prepare when the agent sets a service account.
	KubeconfigFile = RunDir + "/kubeconfig"
	// WorkspaceDir is the agent's working directory.
	WorkspaceDir = "/workspace"
	// HomeDir is $HOME for the agent user.
	HomeDir = "/home/agent"
	// SessionDir is omp's --session-dir; continuity tarballs pack and unpack it.
	SessionDir = HomeDir + "/session"
	// SkillsDir is where prepare installs skills (<SkillsDir>/<skill>/SKILL.md).
	SkillsDir = HomeDir + "/.omp/agent/skills"
	// OmpAgentDir holds config.yml, models.yml and mcp.json.
	OmpAgentDir = HomeDir + "/.omp/agent"
	// MiseDir is the toolchain root; mise's data dir is MiseDir/data.
	MiseDir = "/opt/mise"
	// MiseDataDir is packed by toolsync and unpacked by prepare.
	MiseDataDir = MiseDir + "/data"
	// SystemPromptFile is passed to omp with --append-system-prompt.
	SystemPromptFile = "/tmp/system-prompt.md"
	// TranscriptFile is where export_html writes before upload.
	TranscriptFile = "/tmp/transcript.html"

	// EnvSession is "<ns>/<name>" in session pods.
	EnvSession = "OMP_OPERATOR_SESSION"
	// EnvAgent is "<ns>/<name>" in toolsync pods (instead of EnvSession).
	EnvAgent = "OMP_OPERATOR_AGENT"
	// EnvAPI is the agent API base URL, e.g. http://omp-operator.omp-system.svc:8083.
	EnvAPI = "OMP_OPERATOR_API"
	// EnvWorkspace is the workspace path.
	EnvWorkspace = "OMP_OPERATOR_WORKSPACE"
	// EnvGatewayToken carries the gateway token in the main container.
	EnvGatewayToken = "OMP_GATEWAY_TOKEN"
)

// AuthDir is where a git source's auth Secret keys are mounted in the init container.
// id is "workspace" for the workspace, or the decimal index of the entry in spec.skills.
// Files inside: "ssh-privatekey" and "known_hosts" (SSH), or "token" (token auth).
func AuthDir(id string) string { return RunDir + "/auth/" + id }

// SkillIndexID returns the AuthDir id of spec.skills[i].
func SkillIndexID(i int) string { return fmt.Sprintf("%d", i) }

// ConfigMapSkillDir is where spec.skills[i].configMap is mounted in the init container.
func ConfigMapSkillDir(i int) string { return fmt.Sprintf("%s/skills-cm/%d", RunDir, i) }

// MCPEnvName is the env var carrying the secret value of mcpServers[n].env[m] (stdio)
// or mcpServers[n].headers[m] (http/sse). mcp.json refers to it as ${OMP_MCP_<n>_<m>}.
func MCPEnvName(n, m int) string { return fmt.Sprintf("OMP_MCP_%d_%d", n, m) }

// Auth file names inside AuthDir.
const (
	AuthSSHKeyFile     = "ssh-privatekey"
	AuthKnownHostsFile = "known_hosts"
	AuthTokenFile      = "token"
)

// Plan is what prepare needs: GET /v1/sessions/{ns}/{name}/plan and
// GET /v1/agents/{ns}/{name}/plan (the latter fills only Workspace and Toolchain).
type Plan struct {
	Workspace  *WorkspacePlan  `json:"workspace,omitempty"`
	Skills     []SkillSource   `json:"skills,omitempty"`
	Links      []Link          `json:"links,omitempty"`
	Toolchain  *ToolchainPlan  `json:"toolchain,omitempty"`
	Continuity *ContinuityPlan `json:"continuity,omitempty"`
	// Kubeconfig is true when the agent sets serviceAccountName.
	Kubeconfig bool `json:"kubeconfig,omitempty"`
}

// WorkspacePlan is exactly one of Artifact or Git; nil Plan.Workspace means an empty dir.
type WorkspacePlan struct {
	Artifact *Artifact  `json:"artifact,omitempty"`
	Git      *GitSource `json:"git,omitempty"`
}

// Artifact is a Flux source artifact.
type Artifact struct {
	URL string `json:"url"`
	// Digest is "sha256:<hex>".
	Digest   string `json:"digest"`
	Revision string `json:"revision"`
}

// GitSource is a repository the init container clones.
type GitSource struct {
	URL string `json:"url"`
	// Ref is a branch or tag; empty = default branch.
	Ref   string `json:"ref,omitempty"`
	Depth int    `json:"depth,omitempty"`
	// AuthDir is set when the source has auth; see AuthDir.
	AuthDir string `json:"authDir,omitempty"`
	// AuthType is "ssh" or "token" when AuthDir is set.
	AuthType string `json:"authType,omitempty"`
	// Username for token auth (default x-access-token).
	Username string `json:"username,omitempty"`
}

// SkillSource is exactly one of Git, Artifact or ConfigMapDir.
type SkillSource struct {
	Git      *GitSource `json:"git,omitempty"`
	Artifact *Artifact  `json:"artifact,omitempty"`
	// Path inside the repository or artifact that holds <skill>/SKILL.md directories.
	Path string `json:"path,omitempty"`
	// ConfigMapDir holds "<skill>.md" files; see ConfigMapSkillDir.
	ConfigMapDir string `json:"configMapDir,omitempty"`
}

// Link is a symlink prepare creates: Path (relative to the workspace) -> Target (absolute).
type Link struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

// ToolchainPlan is set when the agent has spec.toolchain.
type ToolchainPlan struct {
	// Hash is status.toolchain.hash: the stored toolchain; empty when none is stored.
	Hash    string   `json:"hash,omitempty"`
	Disable []string `json:"disable,omitempty"`
}

// ContinuityPlan is set when the session has a continuityKey.
type ContinuityPlan struct {
	// ID is the first 16 hex characters of sha256(continuityKey).
	ID string `json:"id"`
}

// Prepared is POSTed to /v1/sessions/{ns}/{name}/prepared.
type Prepared struct {
	WorkspaceRevision string `json:"workspaceRevision"`
	ToolchainHash     string `json:"toolchainHash"`
}

// SessionConfig is GET /v1/sessions/{ns}/{name}.
type SessionConfig struct {
	Prompt string `json:"prompt"`
	// SystemPrompt is instructions, then the credentials manifest, then the actions section.
	SystemPrompt   string      `json:"systemPrompt"`
	Model          ModelConfig `json:"model"`
	Tools          []string    `json:"tools"`
	TimeoutSeconds int64       `json:"timeoutSeconds"`
	// MCP is the rendered mcp.json with ${OMP_MCP_<n>_<m>} placeholders.
	MCP json.RawMessage `json:"mcp,omitempty"`
	// Actions are the host tools: one per OmpAction plus finish (last).
	Actions []ToolDef `json:"actions"`
	// Redact lists the values the driver scrubs from everything it sends.
	Redact []RedactSource `json:"redact,omitempty"`
	// Continuity is true when the session has a continuityKey.
	Continuity bool `json:"continuity,omitempty"`
}

// ModelConfig mirrors spec.model.
type ModelConfig struct {
	// Default and Small are "provider/id".
	Default  string         `json:"default"`
	Small    string         `json:"small,omitempty"`
	Thinking string         `json:"thinking"`
	Gateway  *GatewayConfig `json:"gateway,omitempty"`
}

// GatewayConfig points omp at an auth-gateway; the token is in $OMP_GATEWAY_TOKEN.
type GatewayConfig struct {
	URL string `json:"url"`
	// Providers are the providers routed through the gateway (from the default and small models).
	Providers []string `json:"providers"`
}

// ToolDef is a host tool definition.
type ToolDef struct {
	// Name is the tool name the model sees (action name with '-' -> '_').
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// RedactSource names one secret value by env var or file path.
type RedactSource struct {
	Name string `json:"name"`
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
}

// ActionResult is the response of POST /v1/sessions/{ns}/{name}/actions/{tool}.
type ActionResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"isError"`
}

// Event is one progress line.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	// Text is at most 500 characters.
	Text string `json:"text"`
}

// Events is POSTed to /v1/sessions/{ns}/{name}/events.
type Events struct {
	Events []Event `json:"events"`
}

// Outcome values for Complete.Outcome.
const (
	OutcomeCompleted = "completed"
	OutcomeRetryable = "retryable"
	OutcomeAuth      = "auth"
	OutcomeError     = "error"
)

// Complete is POSTed to /v1/sessions/{ns}/{name}/complete.
type Complete struct {
	Outcome            string `json:"outcome"`
	Error              string `json:"error,omitempty"`
	Usage              Usage  `json:"usage"`
	FinishCalled       bool   `json:"finishCalled"`
	ContinuityRestored bool   `json:"continuityRestored"`
}

// Usage is token and cost accounting from get_session_stats.
type Usage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	// Cost is a decimal string, e.g. "0.0123".
	Cost string `json:"cost"`
}

// ToolchainHash is GET /v1/agents/{ns}/{name}/toolchain and the body of
// POST /v1/agents/{ns}/{name}/toolchain/unchanged.
type ToolchainHash struct {
	Hash string `json:"hash"`
}
