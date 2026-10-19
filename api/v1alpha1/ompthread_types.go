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

// OmpThreadSpec defines the desired state of OmpThread: a conversation handed
// off from a local omp to a cloud agent.
type OmpThreadSpec struct {
	// agentRef is the cloud-enabled OmpAgent that runs the thread.
	// +required
	AgentRef LocalObjectRef `json:"agentRef"`

	// owner is the API principal that created the thread, for example token:dev.
	// +kubebuilder:validation:MaxLength=253
	// +required
	Owner string `json:"owner"`

	// title is the first line of the first message.
	// +kubebuilder:validation:MaxLength=80
	// +optional
	Title string `json:"title,omitempty"`

	// model is provider/id used for every turn; empty means the agent's model.default.
	// +optional
	Model string `json:"model,omitempty"`

	// thinking is the thinking level used for every turn; empty means the agent's.
	// +kubebuilder:validation:Enum=off;minimal;low;medium;high;xhigh;max
	// +optional
	Thinking string `json:"thinking,omitempty"`

	// repo describes the repository the thread works on.
	// +required
	Repo ThreadRepo `json:"repo"`

	// paths are the absolute home and workspace paths of the user's machine, mirrored in the pod.
	// +optional
	Paths ThreadPaths `json:"paths,omitzero"`

	// author is the git identity commits are made with.
	// +required
	Author GitAuthor `json:"author"`

	// secretFiles are workspace-relative files shipped as secrets; Secret <thread>-secrets holds them as file-<i>.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	SecretFiles []string `json:"secretFiles,omitempty"`

	// secretEnv are environment variable names shipped as secrets; Secret <thread>-secrets holds them as env.<NAME>.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	SecretEnv []string `json:"secretEnv,omitempty"`
}

// ThreadRepo describes the repository a thread works on.
type ThreadRepo struct {
	// name is the checkout directory's base name, for display.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// remoteURL is the raw configured remote URL; empty means the whole history was uploaded.
	// +optional
	RemoteURL string `json:"remoteURL,omitempty"`

	// baseRef is the remote branch the base commit came from.
	// +optional
	BaseRef string `json:"baseRef,omitempty"`

	// baseCommit is the commit on the remote that the uploaded delta builds on; empty means a full-history upload.
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})?$`
	// +optional
	BaseCommit string `json:"baseCommit,omitempty"`

	// handoffCommit is the commit holding the user's state at handoff, including uncommitted changes.
	// +kubebuilder:validation:Pattern=`^([0-9a-f]{40}|[0-9a-f]{64})$`
	// +required
	HandoffCommit string `json:"handoffCommit"`

	// localBranch is the branch checked out on the user's machine at handoff.
	// +optional
	LocalBranch string `json:"localBranch,omitempty"`
}

// ThreadPaths are the user's absolute paths, mirrored inside the pod.
type ThreadPaths struct {
	// home is the user's $HOME; empty means /home/agent.
	// +optional
	Home string `json:"home,omitempty"`

	// workspace is the repository root on the user's machine; empty means /workspace.
	// +optional
	Workspace string `json:"workspace,omitempty"`
}

// GitAuthor is a git identity.
type GitAuthor struct {
	// name is the author and committer name.
	// +optional
	Name string `json:"name,omitempty"`

	// email is the author and committer email.
	// +optional
	Email string `json:"email,omitempty"`
}

// OmpThreadStatus defines the observed state of OmpThread.
type OmpThreadStatus struct {
	// observedGeneration is the generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase is Waiting while the running turn has an unanswered question, else Working while a turn is queued or
	// running, else Idle.
	// +optional
	Phase ThreadPhase `json:"phase,omitempty"`

	// turns is the number of turns, in any state.
	// +optional
	Turns int32 `json:"turns,omitempty"`

	// queued is the number of turns waiting for a sitting.
	// +optional
	Queued int32 `json:"queued,omitempty"`

	// activeSession is the OmpSession currently serving the thread.
	// +optional
	ActiveSession string `json:"activeSession,omitempty"`

	// latest is the newest finished turn.
	// +optional
	Latest *ThreadTurnSummary `json:"latest,omitempty"`

	// result is the newest result branch state.
	// +optional
	Result *ThreadResult `json:"result,omitempty"`

	// question is the oldest unanswered question of the running turn.
	// +optional
	Question *ThreadQuestion `json:"question,omitempty"`

	// lastActivityAt is the latest of the thread's creation and turn timestamps.
	// +optional
	LastActivityAt *metav1.Time `json:"lastActivityAt,omitempty"`

	// conditions are the thread's conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ThreadTurnSummary is a finished turn as shown in the thread status.
type ThreadTurnSummary struct {
	// id is the turn number.
	// +required
	ID int32 `json:"id"`

	// state is the turn state.
	// +required
	State TurnState `json:"state"`

	// title is the first line of the reply.
	// +kubebuilder:validation:MaxLength=80
	// +optional
	Title string `json:"title,omitempty"`

	// reply is the start of the agent's reply.
	// +kubebuilder:validation:MaxLength=3000
	// +optional
	Reply string `json:"reply,omitempty"`

	// error is why the turn failed.
	// +kubebuilder:validation:MaxLength=1000
	// +optional
	Error string `json:"error,omitempty"`

	// completedAt is when the turn finished.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// ThreadQuestion is a question the cloud agent asked and nobody answered yet.
type ThreadQuestion struct {
	// turn is the turn that asked.
	// +required
	Turn int32 `json:"turn"`

	// id is the question number within the turn.
	// +required
	ID int32 `json:"id"`

	// text is the question, cut to 1000 characters.
	// +kubebuilder:validation:MaxLength=1000
	// +optional
	Text string `json:"text,omitempty"`

	// askedAt is when the question was asked.
	// +required
	AskedAt metav1.Time `json:"askedAt"`
}

// ThreadResult is the state of the result branch omp-cloud/<thread>.
type ThreadResult struct {
	// branch is the result branch name.
	// +optional
	Branch string `json:"branch,omitempty"`

	// head is the commit the result branch points at.
	// +optional
	Head string `json:"head,omitempty"`

	// base is the commit the result is a delta from.
	// +optional
	Base string `json:"base,omitempty"`

	// filesChanged is the number of files changed against base.
	// +optional
	FilesChanged int32 `json:"filesChanged,omitempty"`

	// insertions is the number of lines added against base.
	// +optional
	Insertions int32 `json:"insertions,omitempty"`

	// deletions is the number of lines removed against base.
	// +optional
	Deletions int32 `json:"deletions,omitempty"`

	// pushed reports that head was pushed to the thread's remote.
	// +optional
	Pushed bool `json:"pushed,omitempty"`

	// pushError is why the last push failed.
	// +kubebuilder:validation:MaxLength=300
	// +optional
	PushError string `json:"pushError,omitempty"`

	// updatedAt is when the result last changed.
	// +required
	UpdatedAt metav1.Time `json:"updatedAt"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ompthread
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Turns",type=integer,JSONPath=`.status.turns`
// +kubebuilder:printcolumn:name="Latest",type=string,JSONPath=`.status.latest.state`
// +kubebuilder:printcolumn:name="Title",type=string,JSONPath=`.spec.title`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OmpThread is the Schema for the ompthreads API
type OmpThread struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of OmpThread
	// +required
	Spec OmpThreadSpec `json:"spec"`

	// status defines the observed state of OmpThread
	// +optional
	Status OmpThreadStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// OmpThreadList contains a list of OmpThread
type OmpThreadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []OmpThread `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &OmpThread{}, &OmpThreadList{})
		return nil
	})
}
