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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// OmpSessionSpec defines the desired state of OmpSession
type OmpSessionSpec struct {
	// agentRef is the OmpAgent that runs the session.
	// +required
	AgentRef LocalObjectRef `json:"agentRef"`

	// triggerRef is the OmpTrigger that created the session, if any.
	// +optional
	TriggerRef *LocalObjectRef `json:"triggerRef,omitempty"`

	// prompt is the text sent to omp.
	// +kubebuilder:validation:MinLength=1
	// +required
	Prompt string `json:"prompt"`

	// priority orders pending sessions of one agent, lower first. Empty means 1 (manual).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3
	// +optional
	Priority *int32 `json:"priority,omitempty"`

	// continuityKey makes sessions of one agent with the same key share omp history.
	// +optional
	ContinuityKey string `json:"continuityKey,omitempty"`

	// event describes the webhook event that created the session.
	// +optional
	Event *SessionEvent `json:"event,omitempty"`

	// threadRef is the OmpThread this session is a sitting of, if any.
	// +optional
	ThreadRef *LocalObjectRef `json:"threadRef,omitempty"`
}

// SessionEvent describes the webhook event behind a session.
type SessionEvent struct {
	// key is the event's grouping key.
	// +optional
	Key string `json:"key,omitempty"`

	// groupId is the trigger group id.
	// +optional
	GroupID string `json:"groupId,omitempty"`

	// name is the event name.
	// +optional
	Name string `json:"name,omitempty"`

	// severity is the event severity.
	// +optional
	Severity string `json:"severity,omitempty"`
}

// OmpSessionStatus defines the observed state of OmpSession.
type OmpSessionStatus struct {
	// phase is the lifecycle phase.
	// +optional
	Phase SessionPhase `json:"phase,omitempty"`

	// attempts is the pods started so far.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// retryAt is when a retryable attempt may run again.
	// +optional
	RetryAt *metav1.Time `json:"retryAt,omitempty"`

	// podName is the current or last pod.
	// +optional
	PodName string `json:"podName,omitempty"`

	// workspaceRevision is the revision the workspace was prepared from.
	// +optional
	WorkspaceRevision string `json:"workspaceRevision,omitempty"`

	// startTime is when the first attempt started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// completionTime is when the session reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// outcome is how the latest attempt ended.
	// +optional
	Outcome SessionOutcome `json:"outcome,omitempty"`

	// verdict is the agent's reported result.
	// +optional
	Verdict Verdict `json:"verdict,omitempty"`

	// title is the agent's one-line title.
	// +kubebuilder:validation:MaxLength=80
	// +optional
	Title string `json:"title,omitempty"`

	// summary is the agent's summary.
	// +kubebuilder:validation:MaxLength=3000
	// +optional
	Summary string `json:"summary,omitempty"`

	// proposedCommands are commands the agent suggests an owner run.
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:MaxLength=500
	// +optional
	ProposedCommands []string `json:"proposedCommands,omitempty"`

	// error describes why the session failed.
	// +kubebuilder:validation:MaxLength=1000
	// +optional
	Error string `json:"error,omitempty"`

	// actions records the action calls the agent made.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	Actions []ActionRecord `json:"actions,omitempty"`

	// usage is the token and cost accounting.
	// +optional
	Usage *SessionUsage `json:"usage,omitempty"`

	// continuity reports whether omp history was restored and stored.
	// +optional
	Continuity *ContinuityStatus `json:"continuity,omitempty"`

	// conditions are Notified and WorkspaceReady.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ActionRecord is one action call made by the agent.
type ActionRecord struct {
	// action is the action's tool name.
	// +required
	Action string `json:"action"`

	// target describes what the call acted on.
	// +optional
	Target string `json:"target,omitempty"`

	// ok is whether the call succeeded.
	// +required
	OK bool `json:"ok"`

	// at is when the call was made.
	// +required
	At metav1.Time `json:"at"`

	// result is the text returned to the agent.
	// +kubebuilder:validation:MaxLength=300
	// +optional
	Result string `json:"result,omitempty"`
}

// SessionUsage is token and cost accounting for a session.
type SessionUsage struct {
	// inputTokens is the input tokens used.
	// +optional
	InputTokens int64 `json:"inputTokens,omitempty"`

	// outputTokens is the output tokens used.
	// +optional
	OutputTokens int64 `json:"outputTokens,omitempty"`

	// cacheReadTokens is the cache-read tokens used.
	// +optional
	CacheReadTokens int64 `json:"cacheReadTokens,omitempty"`

	// cacheWriteTokens is the cache-write tokens used.
	// +optional
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`

	// cost is the cost as a decimal string.
	// +optional
	Cost string `json:"cost,omitempty"`
}

// ContinuityStatus reports omp history handling.
type ContinuityStatus struct {
	// restored is whether stored history was loaded.
	// +optional
	Restored bool `json:"restored,omitempty"`

	// stored is whether history was saved.
	// +optional
	Stored bool `json:"stored,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ompsession
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Trigger",type=string,JSONPath=`.spec.triggerRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Verdict",type=string,JSONPath=`.status.verdict`
// +kubebuilder:printcolumn:name="Title",type=string,JSONPath=`.status.title`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OmpSession is the Schema for the ompsessions API
type OmpSession struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of OmpSession
	// +required
	Spec OmpSessionSpec `json:"spec"`

	// status defines the observed state of OmpSession
	// +optional
	Status OmpSessionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// OmpSessionList contains a list of OmpSession
type OmpSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []OmpSession `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &OmpSession{}, &OmpSessionList{})
		return nil
	})
}
