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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// OmpActionSpec defines the desired state of OmpAction. Exactly one action
// type is set. The object's name must match ^[a-z][a-z0-9-]{0,62}$ and not be
// reserved; the reconciler enforces this and reports it through Ready.
// +kubebuilder:validation:XValidation:rule="[has(self.kubernetesRolloutRestart), has(self.fluxReconcile), has(self.alertmanagerSilence), has(self.httpRequest)].exists_one(x, x)",message="exactly one of kubernetesRolloutRestart, fluxReconcile, alertmanagerSilence or httpRequest must be set"
type OmpActionSpec struct {
	// description is the tool description the model sees.
	// +kubebuilder:validation:MinLength=1
	// +required
	Description string `json:"description"`

	// maxPerSession is the calls one session may make.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxPerSession int32 `json:"maxPerSession,omitempty"`

	// kubernetesRolloutRestart restarts a workload.
	// +optional
	KubernetesRolloutRestart *KubernetesRolloutRestartAction `json:"kubernetesRolloutRestart,omitempty"`

	// fluxReconcile asks Flux to reconcile an object.
	// +optional
	FluxReconcile *FluxReconcileAction `json:"fluxReconcile,omitempty"`

	// alertmanagerSilence creates an Alertmanager silence.
	// +optional
	AlertmanagerSilence *AlertmanagerSilenceAction `json:"alertmanagerSilence,omitempty"`

	// httpRequest sends an HTTP request to a fixed URL prefix.
	// +optional
	HTTPRequest *HTTPRequestAction `json:"httpRequest,omitempty"`
}

// KubernetesRolloutRestartAction restarts a Deployment, StatefulSet or DaemonSet.
type KubernetesRolloutRestartAction struct {
	// kinds are the workload kinds that may be restarted.
	// +kubebuilder:default={"Deployment"}
	// +optional
	Kinds []WorkloadKind `json:"kinds,omitempty"`

	// namespaces limits the target namespaces.
	// +kubebuilder:default={}
	// +optional
	Namespaces NamespaceRule `json:"namespaces,omitzero"`

	// cooldown is the minimum time between successful restarts of one target.
	// +kubebuilder:default="6h"
	// +optional
	Cooldown metav1.Duration `json:"cooldown,omitzero"`

	// waitTimeout is how long to wait for the rollout to complete.
	// +kubebuilder:default="5m"
	// +optional
	WaitTimeout metav1.Duration `json:"waitTimeout,omitzero"`
}

// FluxReconcileAction triggers a Flux reconciliation.
type FluxReconcileAction struct {
	// kinds are the Flux kinds that may be reconciled.
	// +kubebuilder:default={"Kustomization","HelmRelease","GitRepository","OCIRepository"}
	// +optional
	Kinds []FluxKind `json:"kinds,omitempty"`

	// namespaces limits the target namespaces.
	// +kubebuilder:default={}
	// +optional
	Namespaces NamespaceRule `json:"namespaces,omitzero"`

	// waitTimeout is how long to wait for the reconcile to be handled.
	// +kubebuilder:default="5m"
	// +optional
	WaitTimeout metav1.Duration `json:"waitTimeout,omitzero"`
}

// AlertmanagerSilenceAction creates silences in an Alertmanager.
type AlertmanagerSilenceAction struct {
	// url is the Alertmanager base URL (absolute http or https).
	// +kubebuilder:validation:MinLength=1
	// +required
	URL string `json:"url"`

	// authSecretRef is an optional bearer token.
	// +optional
	AuthSecretRef *SecretKeyRef `json:"authSecretRef,omitempty"`

	// maxDuration is the longest silence the agent may create.
	// +kubebuilder:default="24h"
	// +optional
	MaxDuration metav1.Duration `json:"maxDuration,omitzero"`

	// protectedAlertnames can never be silenced.
	// +kubebuilder:default={"Watchdog","InfoInhibitor"}
	// +optional
	ProtectedAlertnames []string `json:"protectedAlertnames,omitempty"`
}

// HTTPRequestAction sends an HTTP request to a URL under allowedURLPrefix.
type HTTPRequestAction struct {
	// method is the HTTP method.
	// +required
	Method HTTPMethod `json:"method"`

	// url is a Go template over .Args.
	// +kubebuilder:validation:MinLength=1
	// +required
	URL string `json:"url"`

	// allowedURLPrefix is the absolute http(s) URL (no userinfo, query or fragment) the rendered URL must stay under.
	// +kubebuilder:validation:MinLength=1
	// +required
	AllowedURLPrefix string `json:"allowedURLPrefix"`

	// headers are sent with every request.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Headers []ValueOrSecret `json:"headers,omitempty"`

	// body is a Go template over .Args with the funcs json and urlquery.
	// +optional
	Body string `json:"body,omitempty"`

	// parameters is a JSON Schema (type object) validating the agent's arguments.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	// +required
	Parameters apiextensionsv1.JSON `json:"parameters"`

	// responseMaxBytes truncates the response returned to the agent.
	// +kubebuilder:default=65536
	// +kubebuilder:validation:Minimum=1
	// +optional
	ResponseMaxBytes int32 `json:"responseMaxBytes,omitempty"`
}

// OmpActionStatus defines the observed state of OmpAction.
type OmpActionStatus struct {
	// type is the action type that is set in the spec.
	// +optional
	Type ActionType `json:"type,omitempty"`

	// conditions holds Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ompaction
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.status.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OmpAction is the Schema for the ompactions API
type OmpAction struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of OmpAction
	// +required
	Spec OmpActionSpec `json:"spec"`

	// status defines the observed state of OmpAction
	// +optional
	Status OmpActionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// OmpActionList contains a list of OmpAction
type OmpActionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []OmpAction `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &OmpAction{}, &OmpActionList{})
		return nil
	})
}
