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

// Thread sittings: where prepare finds the matched git host's auth (an AuthDir id)
// and the shipped secret files.
const (
	// CloudGitAuthID is the AuthDir id of the thread's matched cloud.git host.
	CloudGitAuthID = "cloud-git"
	// ThreadSecretsDir holds the Secret <thread>-secrets file-<i> keys in the init container.
	ThreadSecretsDir = RunDir + "/thread-secrets"
)

// AuthDir is where a git source's auth Secret keys are mounted in the init container.
// id is "workspace" for the workspace, "cloud-git" for a thread's git host, or the
// decimal index of the entry in spec.skills.
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

// FinishToolName is the name of the finish tool every non-thread session gets.
const FinishToolName = "finish"

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
	// Thread is set for sittings of an OmpThread; Workspace and Links are then nil.
	Thread *ThreadPlan `json:"thread,omitempty"`
}

// ThreadPlan is how prepare restores a thread's workspace and home.
type ThreadPlan struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	// FetchURL is the URL prepare fetches the base from, without credentials;
	// empty when no cloud.git host matches a non-http(s) remote.
	FetchURL string `json:"fetchURL,omitempty"`
	// RemoteURL is the raw remote URL, added as "origin" without credentials.
	RemoteURL     string `json:"remoteURL,omitempty"`
	BaseRef       string `json:"baseRef,omitempty"`
	BaseCommit    string `json:"baseCommit,omitempty"`
	HandoffCommit string `json:"handoffCommit"`
	// HasSource, HasResult and HasSetup say which of GET thread/source, thread/result
	// and thread/setup have content.
	HasSource bool `json:"hasSource,omitempty"`
	HasResult bool `json:"hasResult,omitempty"`
	HasSetup  bool `json:"hasSetup,omitempty"`
	// AuthDir, AuthType and Username describe the matched cloud.git host's auth
	// (see AuthDir); empty when none matches.
	AuthDir  string `json:"authDir,omitempty"`
	AuthType string `json:"authType,omitempty"`
	Username string `json:"username,omitempty"`
	// SecretFiles are copied from ThreadSecretsDir/<Key> to <workspace>/<Path>.
	SecretFiles []ThreadSecretFile `json:"secretFiles,omitempty"`
	Author      ThreadAuthor       `json:"author"`
	// Toolchain is set when the agent has spec.toolchain.
	Toolchain *ToolchainPlan `json:"toolchain,omitempty"`
}

// ThreadSecretFile is one shipped secret file.
type ThreadSecretFile struct {
	// Key is the file name inside ThreadSecretsDir ("file-<i>").
	Key string `json:"key"`
	// Path is relative to the workspace.
	Path string `json:"path"`
}

// ThreadAuthor is the git identity commits are made with.
type ThreadAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
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
	// Thread is set for sittings of an OmpThread (spec.threadRef).
	Thread *ThreadConfig `json:"thread,omitempty"`
}

// ThreadConfig is what the driver needs to serve the turns of a thread sitting.
type ThreadConfig struct {
	Name       string `json:"name"`
	Branch     string `json:"branch"`
	BaseCommit string `json:"baseCommit,omitempty"`
	RepoName   string `json:"repoName"`
	// LocalBranch is the branch checked out on the user's machine at handoff.
	LocalBranch string `json:"localBranch,omitempty"`
	// Thinking is the thread's thinking level; empty keeps the agent's.
	Thinking string `json:"thinking,omitempty"`
	// TurnTimeoutSeconds bounds one turn (limits.sessionTimeout).
	TurnTimeoutSeconds int64 `json:"turnTimeoutSeconds"`
	// IdleTimeoutSeconds is how long the sitting waits for the next turn (cloud.idleTimeout).
	IdleTimeoutSeconds int64 `json:"idleTimeoutSeconds"`
	// MaxSittingSeconds is the sitting's lifetime (cloud.maxSitting).
	MaxSittingSeconds int64 `json:"maxSittingSeconds"`
	// Push is true when the matched git host has push set.
	Push bool `json:"push,omitempty"`
	// QuestionTimeoutSeconds is how long the driver waits for an answer to an ask
	// before expiring it (cloud.questionTimeout).
	QuestionTimeoutSeconds int64 `json:"questionTimeoutSeconds,omitempty"`
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
	// Lines marks File as a key=value or key: value file: besides the whole content,
	// each line's value (after the first '=' or ':', trimmed and unquoted, when at
	// least 8 characters) is scrubbed.
	Lines bool `json:"lines,omitempty"`
}

// TurnAssignment is the 200 response of GET /v1/sessions/{ns}/{name}/thread/next.
type TurnAssignment struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
}

// TurnComplete is POSTed to /v1/sessions/{ns}/{name}/thread/turns/{id}/complete.
type TurnComplete struct {
	// Outcome is OutcomeCompleted or OutcomeError; a turn that ends retryably or
	// for auth is not completed, it stays running and is redelivered.
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
	// Reply is the agent's last assistant text, at most 20000 characters.
	Reply string `json:"reply,omitempty"`
	Usage Usage  `json:"usage"`
	// Head is the result branch tip after the turn.
	Head         string `json:"head,omitempty"`
	FilesChanged int    `json:"filesChanged,omitempty"`
	Insertions   int    `json:"insertions,omitempty"`
	Deletions    int    `json:"deletions,omitempty"`
	// Pushed is true when Head was pushed to the thread's remote; PushError is why
	// not (at most 300 characters).
	Pushed    bool   `json:"pushed,omitempty"`
	PushError string `json:"pushError,omitempty"`
}

// QuestionOption is one choice of a question; it has the JSON of threads.QuestionOption.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is one asked question and its answer; it has the JSON of threads.Question.
type Question struct {
	ID          int              `json:"id"`
	Ask         int              `json:"ask"`
	Key         string           `json:"key"`
	Text        string           `json:"text"`
	Header      string           `json:"header,omitempty"`
	Options     []QuestionOption `json:"options,omitempty"`
	Multi       bool             `json:"multi,omitempty"`
	Recommended *int             `json:"recommended,omitempty"`
	AskedAt     time.Time        `json:"askedAt"`
	Selected    []string         `json:"selected,omitempty"`
	Answer      string           `json:"answer,omitempty"`
	AnsweredBy  string           `json:"answeredBy,omitempty"`
	AnsweredAt  *time.Time       `json:"answeredAt,omitempty"`
	Expired     bool             `json:"expired,omitempty"`
}

// AskQuestion is one question of an AskRequest (omp's ask tool: one entry per question).
type AskQuestion struct {
	// Key is omp's question id inside its ask call.
	Key         string           `json:"key"`
	Text        string           `json:"text"`
	Header      string           `json:"header,omitempty"`
	Options     []QuestionOption `json:"options,omitempty"`
	Multi       bool             `json:"multi,omitempty"`
	Recommended *int             `json:"recommended,omitempty"`
}

// AskRequest is POSTed to /v1/sessions/{ns}/{name}/thread/turns/{id}/asks: one ask call.
type AskRequest struct {
	Questions []AskQuestion `json:"questions"`
}

// AskState is the state of one ask call: the 201 of POST asks, the 200 of
// GET asks/{ask} (only when Done) and the 200 of POST asks/{ask}/expire.
type AskState struct {
	Ask int `json:"ask"`
	// Done is true when every question has an answer.
	Done      bool       `json:"done"`
	Questions []Question `json:"questions"`
}

// AskExpire is POSTed to .../asks/{ask}/expire: Answer becomes the answer of every
// question of the ask that is still open.
type AskExpire struct {
	Answer string `json:"answer"`
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
