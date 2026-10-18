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

// OmpTriggerSpec defines the desired state of OmpTrigger. Exactly one of
// webhook or schedule is set.
// +kubebuilder:validation:XValidation:rule="[has(self.webhook), has(self.schedule)].exists_one(x, x)",message="exactly one of webhook or schedule must be set"
type OmpTriggerSpec struct {
	// agentRef is the OmpAgent that runs the sessions.
	// +required
	AgentRef LocalObjectRef `json:"agentRef"`

	// enabled turns the trigger on or off.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// prompt is a Go text/template (missingkey=error) rendered into the session prompt.
	// +kubebuilder:validation:MinLength=1
	// +required
	Prompt string `json:"prompt"`

	// priority orders pending sessions of one agent, lower first. Empty means
	// webhook 2 (1 when severity is critical) and schedule 3.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3
	// +optional
	Priority *int32 `json:"priority,omitempty"`

	// session configures the sessions this trigger creates.
	// +kubebuilder:default={}
	// +optional
	Session TriggerSessionSpec `json:"session,omitzero"`

	// webhook makes the trigger fire on HTTP requests.
	// +optional
	Webhook *WebhookSpec `json:"webhook,omitempty"`

	// schedule makes the trigger fire on a cron schedule.
	// +optional
	Schedule *ScheduleSpec `json:"schedule,omitempty"`
}

// TriggerSessionSpec configures a trigger's sessions.
type TriggerSessionSpec struct {
	// resume selects how sessions share omp history; "episode" only applies to webhook triggers.
	// +kubebuilder:default=none
	// +optional
	Resume ResumeMode `json:"resume,omitempty"`
}

// WebhookSpec configures a webhook trigger served at /hooks/<namespace>/<name>.
type WebhookSpec struct {
	// adapter turns the payload into events.
	// +required
	Adapter WebhookAdapter `json:"adapter"`

	// auth authenticates requests.
	// +required
	Auth WebhookAuth `json:"auth"`

	// alertFilter is a CEL expression over `alert` returning bool (alert adapters only).
	// Empty means everything except Watchdog and InfoInhibitor.
	// +optional
	AlertFilter string `json:"alertFilter,omitempty"`

	// filter is a CEL expression over `event` returning bool. Empty means true.
	// +optional
	Filter string `json:"filter,omitempty"`

	// mapping turns a generic payload into events; required for the generic adapter.
	// +optional
	Mapping *WebhookMapping `json:"mapping,omitempty"`

	// grouping controls how events are grouped, deduplicated and rate limited.
	// +kubebuilder:default={}
	// +optional
	Grouping GroupingSpec `json:"grouping,omitzero"`
}

// WebhookAuth authenticates webhook requests; exactly one member is set.
// +kubebuilder:validation:XValidation:rule="[has(self.bearer), has(self.basic), has(self.hmac)].exists_one(x, x)",message="exactly one of bearer, basic or hmac must be set"
type WebhookAuth struct {
	// bearer checks an Authorization: Bearer token.
	// +optional
	Bearer *BearerAuth `json:"bearer,omitempty"`

	// basic checks HTTP basic credentials.
	// +optional
	Basic *BasicAuth `json:"basic,omitempty"`

	// hmac checks a hex SHA-256 signature of the body.
	// +optional
	HMAC *HMACAuth `json:"hmac,omitempty"`
}

// BearerAuth checks a bearer token.
type BearerAuth struct {
	// secretRef holds the token.
	// +required
	SecretRef SecretKeyRef `json:"secretRef"`
}

// BasicAuth checks HTTP basic credentials.
type BasicAuth struct {
	// secretRef is a Secret with keys "username" and "password".
	// +required
	SecretRef LocalObjectRef `json:"secretRef"`
}

// HMACAuth checks an HMAC-SHA256 signature.
type HMACAuth struct {
	// secretRef holds the shared key.
	// +required
	SecretRef SecretKeyRef `json:"secretRef"`

	// header carries the hex signature.
	// +kubebuilder:default=X-Hub-Signature-256
	// +optional
	Header string `json:"header,omitempty"`

	// prefix precedes the signature in the header. Unset means "sha256=" for
	// X-Hub-Signature-256 and "" for any other header.
	// +optional
	Prefix *string `json:"prefix,omitempty"`

	// timestampHeader carries Unix seconds; when set the signed text is "<timestamp>:<body>".
	// +optional
	TimestampHeader string `json:"timestampHeader,omitempty"`

	// maxSkew is the allowed distance of the timestamp from now.
	// +kubebuilder:default="5m"
	// +optional
	MaxSkew metav1.Duration `json:"maxSkew,omitzero"`
}

// WebhookMapping maps a generic payload to events with CEL over `payload`.
type WebhookMapping struct {
	// events returns the list of events in the payload. Empty means "[payload]".
	// +optional
	Events string `json:"events,omitempty"`

	// key returns the event's grouping key (string).
	// +kubebuilder:validation:MinLength=1
	// +required
	Key string `json:"key"`

	// title returns the event name (string). Empty means "''".
	// +optional
	Title string `json:"title,omitempty"`

	// severity returns the event severity (string). Empty means "'info'".
	// +optional
	Severity string `json:"severity,omitempty"`

	// status returns "firing" or "resolved". Empty means "'firing'".
	// +optional
	Status string `json:"status,omitempty"`

	// fingerprint returns the event fingerprint (string). Empty means the sha256 of the event's JSON.
	// +optional
	Fingerprint string `json:"fingerprint,omitempty"`
}

// GroupingSpec controls grouping, dedupe and rate limiting of webhook events.
type GroupingSpec struct {
	// episodeGap is how long a resolved group stays quiet before a new episode starts.
	// +kubebuilder:default="30m"
	// +optional
	EpisodeGap metav1.Duration `json:"episodeGap,omitzero"`

	// retriageAfter is the minimum gap between sessions for new fingerprints of one group.
	// +kubebuilder:default="1h"
	// +optional
	RetriageAfter metav1.Duration `json:"retriageAfter,omitzero"`

	// perGroupPerDay caps sessions per group in a rolling 24 hours.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	PerGroupPerDay int32 `json:"perGroupPerDay,omitempty"`
}

// ScheduleSpec configures a cron trigger.
type ScheduleSpec struct {
	// cron is a 5-field cron expression.
	// +kubebuilder:validation:MinLength=1
	// +required
	Cron string `json:"cron"`

	// timeZone is an IANA time zone name.
	// +kubebuilder:default=UTC
	// +optional
	TimeZone string `json:"timeZone,omitempty"`

	// input is available to the prompt template as .Input.
	// +optional
	Input string `json:"input,omitempty"`
}

// OmpTriggerStatus defines the observed state of OmpTrigger.
type OmpTriggerStatus struct {
	// type is the trigger type that is set in the spec.
	// +optional
	Type TriggerType `json:"type,omitempty"`

	// conditions holds Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// hookPath is the webhook path, /hooks/<namespace>/<name>.
	// +optional
	HookPath string `json:"hookPath,omitempty"`

	// groups is the state of recent event groups.
	// +kubebuilder:validation:MaxItems=100
	// +optional
	Groups []TriggerGroup `json:"groups,omitempty"`

	// lastFireAt is the schedule slot last fired.
	// +optional
	LastFireAt *metav1.Time `json:"lastFireAt,omitempty"`

	// nextFireAt is the next schedule slot.
	// +optional
	NextFireAt *metav1.Time `json:"nextFireAt,omitempty"`
}

// TriggerGroup is the dedupe and rate-limit state of one event group.
type TriggerGroup struct {
	// id is the first 16 hex characters of the sha256 of the event key.
	// +kubebuilder:validation:MinLength=1
	// +required
	ID string `json:"id"`

	// name is the latest event name.
	// +optional
	Name string `json:"name,omitempty"`

	// severity is the latest event severity.
	// +optional
	Severity string `json:"severity,omitempty"`

	// episodeStart is when the current episode began.
	// +optional
	EpisodeStart metav1.Time `json:"episodeStart,omitzero"`

	// lastNotified is when the group last received a firing event.
	// +optional
	LastNotified metav1.Time `json:"lastNotified,omitzero"`

	// resolvedAt is when the group was last resolved; unset while firing.
	// +optional
	ResolvedAt *metav1.Time `json:"resolvedAt,omitempty"`

	// triagedFingerprints are the fingerprints already handed to a session, newest last.
	// +kubebuilder:validation:MaxItems=50
	// +optional
	TriagedFingerprints []string `json:"triagedFingerprints,omitempty"`

	// lastSession is the name of the latest session created for the group.
	// +optional
	LastSession string `json:"lastSession,omitempty"`

	// lastSessionAt is when that session was created.
	// +optional
	LastSessionAt *metav1.Time `json:"lastSessionAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=omptrigger
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.status.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OmpTrigger is the Schema for the omptriggers API
type OmpTrigger struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of OmpTrigger
	// +required
	Spec OmpTriggerSpec `json:"spec"`

	// status defines the observed state of OmpTrigger
	// +optional
	Status OmpTriggerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// OmpTriggerList contains a list of OmpTrigger
type OmpTriggerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []OmpTrigger `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &OmpTrigger{}, &OmpTriggerList{})
		return nil
	})
}
