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

// Condition types.
const (
	// ConditionReady reports that an object is usable.
	ConditionReady = "Ready"
	// ConditionToolchainReady reports the state of an agent's synced toolchain.
	ConditionToolchainReady = "ToolchainReady"
	// ConditionNotified reports that a finished session's sinks were notified.
	ConditionNotified = "Notified"
	// ConditionWorkspaceReady reports that a session's workspace source is available.
	ConditionWorkspaceReady = "WorkspaceReady"
)

// FinalizerCleanup is the finalizer set on agents and sessions to remove stored data.
const FinalizerCleanup = "omp.techdufus.com/cleanup"

// Label keys set by the operator.
const (
	// LabelManagedBy is the standard managed-by label; pods and secrets created by the operator carry ManagedByValue.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the value of LabelManagedBy on operator-created objects.
	ManagedByValue = "omp-operator"

	// LabelAgent is the OmpAgent name.
	LabelAgent = "omp.techdufus.com/agent"
	// LabelTrigger is the OmpTrigger name.
	LabelTrigger = "omp.techdufus.com/trigger"
	// LabelKind is the SessionKind of a session.
	LabelKind = "omp.techdufus.com/kind"
	// LabelGroup is the trigger group id (first 16 hex of sha256 of the event key).
	LabelGroup = "omp.techdufus.com/group"
	// LabelSeverity is the event severity.
	LabelSeverity = "omp.techdufus.com/severity"
	// LabelContinuity is the first 16 hex of sha256 of the continuity key.
	LabelContinuity = "omp.techdufus.com/continuity"
	// LabelSession is the OmpSession name on session pods.
	LabelSession = "omp.techdufus.com/session"
	// LabelRole is the PodRole of an operator-created pod.
	LabelRole = "omp.techdufus.com/role"
	// LabelThread is the OmpThread name on cloud sittings.
	LabelThread = "omp.techdufus.com/thread"
)

// PodRole is the value of LabelRole.
type PodRole string

// Pod roles.
const (
	PodRoleSession  PodRole = "session"
	PodRoleToolsync PodRole = "toolsync"
)

// SessionKind is the value of LabelKind.
type SessionKind string

// Session kinds.
const (
	SessionKindWebhook  SessionKind = "webhook"
	SessionKindSchedule SessionKind = "schedule"
	SessionKindManual   SessionKind = "manual"
	SessionKindCloud    SessionKind = "cloud"
)

// SessionPhase is the lifecycle phase of an OmpSession.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type SessionPhase string

// Session phases.
const (
	SessionPhasePending   SessionPhase = "Pending"
	SessionPhaseRunning   SessionPhase = "Running"
	SessionPhaseSucceeded SessionPhase = "Succeeded"
	SessionPhaseFailed    SessionPhase = "Failed"
)

// SessionOutcome is how the in-pod driver says the latest attempt ended.
// +kubebuilder:validation:Enum=completed;retryable;auth;error
type SessionOutcome string

// Session outcomes.
const (
	OutcomeCompleted SessionOutcome = "completed"
	OutcomeRetryable SessionOutcome = "retryable"
	OutcomeAuth      SessionOutcome = "auth"
	OutcomeError     SessionOutcome = "error"
)

// Verdict is the result the agent reports through finish.
// +kubebuilder:validation:Enum=ok;fixed;needs_owner;inconclusive
type Verdict string

// Verdicts.
const (
	VerdictOK           Verdict = "ok"
	VerdictFixed        Verdict = "fixed"
	VerdictNeedsOwner   Verdict = "needs_owner"
	VerdictInconclusive Verdict = "inconclusive"
)

// NotifyOn selects the results a sink is notified about: a Verdict, or failed
// for a Failed session.
// +kubebuilder:validation:Enum=ok;fixed;needs_owner;inconclusive;failed
type NotifyOn string

// Notification selectors.
const (
	NotifyOnOK           NotifyOn = "ok"
	NotifyOnFixed        NotifyOn = "fixed"
	NotifyOnNeedsOwner   NotifyOn = "needs_owner"
	NotifyOnInconclusive NotifyOn = "inconclusive"
	NotifyOnFailed       NotifyOn = "failed"
)

// SinkType is a notification sink kind.
// +kubebuilder:validation:Enum=ntfy;slack;discord;webhook
type SinkType string

// Sink types.
const (
	SinkNtfy    SinkType = "ntfy"
	SinkSlack   SinkType = "slack"
	SinkDiscord SinkType = "discord"
	SinkWebhook SinkType = "webhook"
)

// ThinkingLevel is omp's default thinking level.
// +kubebuilder:validation:Enum=off;minimal;low;medium;high;xhigh;max
type ThinkingLevel string

// Thinking levels.
const (
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
	ThinkingMax     ThinkingLevel = "max"
)

// MCPServerType is the transport of an MCP server.
// +kubebuilder:validation:Enum=stdio;http;sse
type MCPServerType string

// MCP server types.
const (
	MCPStdio MCPServerType = "stdio"
	MCPHTTP  MCPServerType = "http"
	MCPSSE   MCPServerType = "sse"
)

// FluxSourceKind is the kind of a Flux source usable as a skill or workspace source.
// +kubebuilder:validation:Enum=GitRepository;OCIRepository
type FluxSourceKind string

// Flux source kinds.
const (
	FluxSourceGitRepository FluxSourceKind = "GitRepository"
	FluxSourceOCIRepository FluxSourceKind = "OCIRepository"
)

// WorkloadKind is a workload a rollout-restart action may restart.
// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet
type WorkloadKind string

// Workload kinds.
const (
	WorkloadDeployment  WorkloadKind = "Deployment"
	WorkloadStatefulSet WorkloadKind = "StatefulSet"
	WorkloadDaemonSet   WorkloadKind = "DaemonSet"
)

// FluxKind is a Flux object a reconcile action may target.
// +kubebuilder:validation:Enum=Kustomization;HelmRelease;GitRepository;OCIRepository;HelmRepository
type FluxKind string

// Flux kinds.
const (
	FluxKustomization  FluxKind = "Kustomization"
	FluxHelmRelease    FluxKind = "HelmRelease"
	FluxGitRepository  FluxKind = "GitRepository"
	FluxOCIRepository  FluxKind = "OCIRepository"
	FluxHelmRepository FluxKind = "HelmRepository"
)

// ActionType is the union member an OmpAction sets; it is reported in status.type.
// +kubebuilder:validation:Enum=kubernetesRolloutRestart;fluxReconcile;alertmanagerSilence;httpRequest
type ActionType string

// Action types.
const (
	ActionKubernetesRolloutRestart ActionType = "kubernetesRolloutRestart"
	ActionFluxReconcile            ActionType = "fluxReconcile"
	ActionAlertmanagerSilence      ActionType = "alertmanagerSilence"
	ActionHTTPRequest              ActionType = "httpRequest"
)

// TriggerType is the union member an OmpTrigger sets; it is reported in status.type.
// +kubebuilder:validation:Enum=webhook;schedule
type TriggerType string

// Trigger types.
const (
	TriggerWebhook  TriggerType = "webhook"
	TriggerSchedule TriggerType = "schedule"
)

// WebhookAdapter selects how a webhook payload is turned into events.
// +kubebuilder:validation:Enum=alertmanager;grafana;generic
type WebhookAdapter string

// Webhook adapters.
const (
	AdapterAlertmanager WebhookAdapter = "alertmanager"
	AdapterGrafana      WebhookAdapter = "grafana"
	AdapterGeneric      WebhookAdapter = "generic"
)

// ResumeMode selects how a trigger's sessions share omp history.
// +kubebuilder:validation:Enum=none;episode;key
type ResumeMode string

// Resume modes.
const (
	ResumeNone    ResumeMode = "none"
	ResumeEpisode ResumeMode = "episode"
	ResumeKey     ResumeMode = "key"
)

// HTTPMethod is an HTTP method an httpRequest action may use.
// +kubebuilder:validation:Enum=GET;POST;PUT;PATCH;DELETE
type HTTPMethod string

// HTTP methods.
const (
	MethodGet    HTTPMethod = "GET"
	MethodPost   HTTPMethod = "POST"
	MethodPut    HTTPMethod = "PUT"
	MethodPatch  HTTPMethod = "PATCH"
	MethodDelete HTTPMethod = "DELETE"
)

// ThreadPhase is the lifecycle phase of an OmpThread.
// +kubebuilder:validation:Enum=Working;Idle
type ThreadPhase string

// Thread phases.
const (
	// ThreadPhaseWorking means a turn is queued or running.
	ThreadPhaseWorking ThreadPhase = "Working"
	// ThreadPhaseIdle means every turn has finished.
	ThreadPhaseIdle ThreadPhase = "Idle"
)

// TurnState is the state of one turn (one message) of an OmpThread.
// +kubebuilder:validation:Enum=queued;running;done;failed;cancelled
type TurnState string

// Turn states.
const (
	TurnStateQueued    TurnState = "queued"
	TurnStateRunning   TurnState = "running"
	TurnStateDone      TurnState = "done"
	TurnStateFailed    TurnState = "failed"
	TurnStateCancelled TurnState = "cancelled"
)
