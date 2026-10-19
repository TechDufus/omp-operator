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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// OmpAgentSpec defines the desired state of OmpAgent
type OmpAgentSpec struct {
	// image is the runtime image; it must contain omp, omp-operator-driver and git.
	// Empty means the operator's --default-runtime-image.
	// +optional
	Image string `json:"image,omitempty"`

	// serviceAccountName is the ServiceAccount session pods run as, with its token mounted.
	// Empty means the namespace's "default" account with no token.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// model selects the model and how it is reached.
	// +required
	Model ModelSpec `json:"model"`

	// instructions are appended to omp's system prompt.
	// +optional
	Instructions string `json:"instructions,omitempty"`

	// tools configures omp's built-in tools.
	// +kubebuilder:default={}
	// +optional
	Tools ToolsSpec `json:"tools,omitzero"`

	// skills are copied to ~/.omp/agent/skills in order; a later skill with the same name replaces an earlier one.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Skills []SkillSource `json:"skills,omitempty"`

	// workspace is the working directory content; absent means an empty directory.
	// +optional
	Workspace *WorkspaceSpec `json:"workspace,omitempty"`

	// credentials are secrets exposed to the agent's tools; the agent is told their name, description and location, never the value.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Credentials []Credential `json:"credentials,omitempty"`

	// mcpServers are MCP servers omp connects to.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	MCPServers []MCPServer `json:"mcpServers,omitempty"`

	// actions are the OmpActions (same namespace) the agent may call.
	// +optional
	Actions []LocalObjectRef `json:"actions,omitempty"`

	// toolchain serves the workspace's mise tools from the operator.
	// +optional
	Toolchain *ToolchainSpec `json:"toolchain,omitempty"`

	// limits bound session duration, retries and webhook-created sessions.
	// +kubebuilder:default={}
	// +optional
	Limits LimitsSpec `json:"limits,omitzero"`

	// notify lists sinks told about finished sessions.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	Notify []NotifySink `json:"notify,omitempty"`

	// sessionRetention is how long a finished session is kept.
	// +kubebuilder:default="720h"
	// +optional
	SessionRetention metav1.Duration `json:"sessionRetention,omitzero"`

	// pod overrides are merged into the session pod's main container.
	// +optional
	Pod PodOverrides `json:"pod,omitzero"`

	// cloud enables OmpThreads (cloud agents) on this agent: long-lived conversations handed off from a local omp.
	// +optional
	Cloud *CloudSpec `json:"cloud,omitempty"`
}

// ModelSpec selects the model and how it is reached.
type ModelSpec struct {
	// default is the main model as "provider/id".
	// +kubebuilder:validation:MinLength=1
	// +required
	Default string `json:"default"`

	// small is the model for small tasks; empty means default.
	// +optional
	Small string `json:"small,omitempty"`

	// thinking is the default thinking level.
	// +kubebuilder:default=medium
	// +optional
	Thinking ThinkingLevel `json:"thinking,omitempty"`

	// gateway routes model calls through an omp auth-gateway; absent means provider keys come from credentials.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`
}

// GatewaySpec points an agent at an omp auth-gateway.
type GatewaySpec struct {
	// url is the gateway base URL.
	// +kubebuilder:validation:MinLength=1
	// +required
	URL string `json:"url"`

	// tokenSecretRef is the gateway token; absent means an operator-managed token.
	// +optional
	TokenSecretRef *SecretKeyRef `json:"tokenSecretRef,omitempty"`

	// allowedModels are the model patterns the gateway permits this agent; empty means "<provider of default>/*".
	// +optional
	AllowedModels []string `json:"allowedModels,omitempty"`
}

// ToolsSpec configures omp's built-in tools.
type ToolsSpec struct {
	// allow lists omp built-in tool names the agent may use; exactly ["*"] means every built-in tool.
	// +kubebuilder:default={"read","grep","glob","bash","todo"}
	// +optional
	Allow []string `json:"allow,omitempty"`
}

// CloudSpec configures cloud threads of an agent. Its presence enables threads.
type CloudSpec struct {
	// idleTimeout is how long a sitting waits for the next message before it saves state and exits.
	// +kubebuilder:default="10m"
	// +optional
	IdleTimeout metav1.Duration `json:"idleTimeout,omitzero"`

	// maxSitting bounds one sitting: it takes no turn that could end past this, and the next message starts a new pod.
	// +kubebuilder:default="4h"
	// +optional
	MaxSitting metav1.Duration `json:"maxSitting,omitzero"`

	// threadRetention is how long an idle thread is kept after its last activity.
	// +kubebuilder:default="168h"
	// +optional
	ThreadRetention metav1.Duration `json:"threadRetention,omitzero"`

	// git lists the git hosts threads may fetch from and push to.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	Git []CloudGitHost `json:"git,omitempty"`
}

// CloudGitHost authenticates a git host for cloud threads.
type CloudGitHost struct {
	// host is the hostname, for example github.com.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	// +required
	Host string `json:"host"`

	// auth authenticates fetches (and pushes) to the host.
	// +required
	Auth GitAuth `json:"auth"`

	// push pushes the result branch omp-cloud/<thread> to the thread's remote after every turn.
	// +optional
	Push bool `json:"push,omitempty"`
}

// SkillSource is one source of skills; exactly one member is set.
// +kubebuilder:validation:XValidation:rule="[has(self.git), has(self.configMap), has(self.fluxSource)].exists_one(x, x)",message="exactly one of git, configMap or fluxSource must be set"
type SkillSource struct {
	// git clones skills from a git repository.
	// +optional
	Git *GitSkillSource `json:"git,omitempty"`

	// configMap provides skills as keys named "<skill>.md", each becoming <skill>/SKILL.md.
	// +optional
	ConfigMap *LocalObjectRef `json:"configMap,omitempty"`

	// fluxSource reads skills from a Flux source artifact.
	// +optional
	FluxSource *FluxSkillSource `json:"fluxSource,omitempty"`
}

// GitSkillSource clones skills from git.
type GitSkillSource struct {
	// url is the repository URL.
	// +kubebuilder:validation:MinLength=1
	// +required
	URL string `json:"url"`

	// ref is a branch or tag; empty means the default branch.
	// +optional
	Ref string `json:"ref,omitempty"`

	// path is the directory holding <skill>/SKILL.md entries.
	// +kubebuilder:default=skills
	// +optional
	Path string `json:"path,omitempty"`

	// auth authenticates the clone.
	// +optional
	Auth *GitAuth `json:"auth,omitempty"`
}

// FluxSourceRef names a Flux source.
type FluxSourceRef struct {
	// kind is the Flux source kind.
	// +required
	Kind FluxSourceKind `json:"kind"`

	// namespace is the source's namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Namespace string `json:"namespace"`

	// name is the source's name.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// FluxSkillSource reads skills from a Flux source artifact.
type FluxSkillSource struct {
	FluxSourceRef `json:",inline"`

	// path is the directory holding <skill>/SKILL.md entries.
	// +kubebuilder:default=skills
	// +optional
	Path string `json:"path,omitempty"`
}

// WorkspaceSpec is the session's working directory content; exactly one of git or fluxSource is set.
// +kubebuilder:validation:XValidation:rule="[has(self.git), has(self.fluxSource)].exists_one(x, x)",message="exactly one of git or fluxSource must be set"
type WorkspaceSpec struct {
	// git clones the workspace from a git repository.
	// +optional
	Git *GitWorkspaceSource `json:"git,omitempty"`

	// fluxSource downloads the workspace from a Flux source artifact.
	// +optional
	FluxSource *FluxSourceRef `json:"fluxSource,omitempty"`

	// links are symlinks created in the workspace.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Links []WorkspaceLink `json:"links,omitempty"`
}

// GitWorkspaceSource clones the workspace from git.
type GitWorkspaceSource struct {
	// url is the repository URL.
	// +kubebuilder:validation:MinLength=1
	// +required
	URL string `json:"url"`

	// ref is a branch or tag; empty means the default branch.
	// +optional
	Ref string `json:"ref,omitempty"`

	// depth is the clone depth.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Depth int32 `json:"depth,omitempty"`

	// auth authenticates the clone.
	// +optional
	Auth *GitAuth `json:"auth,omitempty"`
}

// WorkspaceLink is a symlink created inside the workspace.
type WorkspaceLink struct {
	// path is the link's location, relative to the workspace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/') && !self.split('/').exists(s, s == '..')",message="path must be relative and must not contain '..'"
	// +required
	Path string `json:"path"`

	// target is the absolute path the link points to.
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:XValidation:rule="self.startsWith('/')",message="target must be an absolute path"
	// +required
	Target string `json:"target"`
}

// Credential exposes one Secret key to the agent's tools; exactly one of env or file is set.
// +kubebuilder:validation:XValidation:rule="[has(self.env), has(self.file)].exists_one(x, x)",message="exactly one of env or file must be set"
type Credential struct {
	// name is a DNS label identifying the credential in the agent's prompt.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Name string `json:"name"`

	// description tells the agent what the credential is for.
	// +kubebuilder:validation:MinLength=1
	// +required
	Description string `json:"description"`

	// secretRef is the Secret key holding the value.
	// +required
	SecretRef SecretKeyRef `json:"secretRef"`

	// env is the environment variable the value is exposed as.
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +optional
	Env string `json:"env,omitempty"`

	// file is the absolute path the value is mounted at.
	// +kubebuilder:validation:Pattern=`^/.+`
	// +optional
	File string `json:"file,omitempty"`
}

// MCPServer is an MCP server written to omp's mcp.json.
type MCPServer struct {
	// name is a DNS label.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Name string `json:"name"`

	// type is the transport; empty means stdio with command, http with url.
	// +optional
	Type MCPServerType `json:"type,omitempty"`

	// command is the stdio command: [0] is the binary (it must exist in the image), the rest are arguments.
	// +optional
	Command []string `json:"command,omitempty"`

	// url is the http or sse endpoint.
	// +optional
	URL string `json:"url,omitempty"`

	// env is the stdio server's environment.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Env []ValueOrSecret `json:"env,omitempty"`

	// headers are sent to http and sse servers.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Headers []ValueOrSecret `json:"headers,omitempty"`
}

// ToolchainSpec serves the workspace's mise tools from the operator.
type ToolchainSpec struct {
	// disable lists mise tool short names to skip.
	// +optional
	Disable []string `json:"disable,omitempty"`
}

// LimitsSpec bounds an agent's sessions.
type LimitsSpec struct {
	// sessionTimeout is the time one attempt may run.
	// +kubebuilder:default="30m"
	// +optional
	SessionTimeout metav1.Duration `json:"sessionTimeout,omitzero"`

	// maxAttempts is the attempts a session gets before it fails.
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxAttempts int32 `json:"maxAttempts,omitempty"`

	// retryDelay is the wait before a retryable attempt is retried.
	// +kubebuilder:default="15m"
	// +optional
	RetryDelay metav1.Duration `json:"retryDelay,omitzero"`

	// eventSessionsPerDay caps webhook-created sessions in a rolling 24 hours.
	// +kubebuilder:default=12
	// +kubebuilder:validation:Minimum=1
	// +optional
	EventSessionsPerDay int32 `json:"eventSessionsPerDay,omitempty"`

	// nonCriticalSessionsPerDay caps the part of eventSessionsPerDay whose severity is not critical.
	// +kubebuilder:default=8
	// +kubebuilder:validation:Minimum=1
	// +optional
	NonCriticalSessionsPerDay int32 `json:"nonCriticalSessionsPerDay,omitempty"`

	// maxConcurrentSessions is how many sessions of the agent run at once.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +optional
	MaxConcurrentSessions int32 `json:"maxConcurrentSessions,omitempty"`
}

// NotifySink is a notification destination; the member named by type is set.
// +kubebuilder:validation:XValidation:rule="[has(self.ntfy), has(self.slack), has(self.discord), has(self.webhook)].exists_one(x, x)",message="exactly one of ntfy, slack, discord or webhook must be set"
// +kubebuilder:validation:XValidation:rule="(self.type == 'ntfy' && has(self.ntfy)) || (self.type == 'slack' && has(self.slack)) || (self.type == 'discord' && has(self.discord)) || (self.type == 'webhook' && has(self.webhook))",message="the member named by type must be set"
type NotifySink struct {
	// type selects the sink.
	// +required
	Type SinkType `json:"type"`

	// on lists the results that notify this sink; empty means all.
	// +kubebuilder:default={"ok","fixed","needs_owner","inconclusive","failed"}
	// +optional
	On []NotifyOn `json:"on,omitempty"`

	// ntfy configures an ntfy sink.
	// +optional
	Ntfy *NtfySink `json:"ntfy,omitempty"`

	// slack configures a Slack incoming-webhook sink.
	// +optional
	Slack *SlackSink `json:"slack,omitempty"`

	// discord configures a Discord webhook sink.
	// +optional
	Discord *DiscordSink `json:"discord,omitempty"`

	// webhook configures a generic JSON webhook sink.
	// +optional
	Webhook *WebhookSink `json:"webhook,omitempty"`
}

// NtfySink configures an ntfy sink.
type NtfySink struct {
	// url is the ntfy server.
	// +kubebuilder:default="https://ntfy.sh"
	// +optional
	URL string `json:"url,omitempty"`

	// topicSecretRef holds the topic.
	// +required
	TopicSecretRef SecretKeyRef `json:"topicSecretRef"`
}

// SlackSink configures a Slack sink.
type SlackSink struct {
	// webhookURLSecretRef holds the incoming-webhook URL.
	// +required
	WebhookURLSecretRef SecretKeyRef `json:"webhookURLSecretRef"`
}

// DiscordSink configures a Discord sink.
type DiscordSink struct {
	// webhookURLSecretRef holds the webhook URL.
	// +required
	WebhookURLSecretRef SecretKeyRef `json:"webhookURLSecretRef"`
}

// WebhookSink configures a generic webhook sink.
type WebhookSink struct {
	// urlSecretRef holds the destination URL.
	// +required
	URLSecretRef SecretKeyRef `json:"urlSecretRef"`

	// hmacSecretRef holds a key used to sign the body into X-Omp-Signature-256.
	// +optional
	HMACSecretRef *SecretKeyRef `json:"hmacSecretRef,omitempty"`
}

// PodOverrides are merged into the session pod's main container.
type PodOverrides struct {
	// resources are the main container's resource requirements.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitzero"`

	// env is added to the main container's environment.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// volumes are added to the pod.
	// +optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// volumeMounts are added to the main container.
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`

	// nodeSelector constrains scheduling.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations are added to the pod.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// labels are added to the pod.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// OmpAgentStatus defines the observed state of OmpAgent.
type OmpAgentStatus struct {
	// observedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions are Ready and ToolchainReady.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// toolchain is the state of the agent's synced toolchain.
	// +optional
	Toolchain ToolchainStatus `json:"toolchain,omitzero"`

	// notices records when each rate-limited notice was last sent, by key.
	// +optional
	Notices map[string]metav1.Time `json:"notices,omitempty"`
}

// ToolchainStatus is the state of an agent's synced toolchain.
type ToolchainStatus struct {
	// hash identifies the stored toolchain.
	// +optional
	Hash string `json:"hash,omitempty"`

	// syncedAt is when the toolchain was last confirmed.
	// +optional
	SyncedAt *metav1.Time `json:"syncedAt,omitempty"`

	// stale is set when a session saw a different toolchain hash than the stored one.
	// +optional
	Stale bool `json:"stale,omitempty"`

	// lastAttemptAt is when the last toolsync attempt started.
	// +optional
	LastAttemptAt *metav1.Time `json:"lastAttemptAt,omitempty"`

	// lastError is the last toolsync failure.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ompagent
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model.default`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OmpAgent is the Schema for the ompagents API
type OmpAgent struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of OmpAgent
	// +required
	Spec OmpAgentSpec `json:"spec"`

	// status defines the observed state of OmpAgent
	// +optional
	Status OmpAgentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// OmpAgentList contains a list of OmpAgent
type OmpAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []OmpAgent `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &OmpAgent{}, &OmpAgentList{})
		return nil
	})
}
