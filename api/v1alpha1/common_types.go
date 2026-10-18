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

// SecretKeyRef selects one key of a Secret in the referencing object's namespace.
type SecretKeyRef struct {
	// name is the Secret name.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// key is the key inside the Secret.
	// +kubebuilder:validation:MinLength=1
	// +required
	Key string `json:"key"`
}

// LocalObjectRef names an object in the referencing object's namespace.
type LocalObjectRef struct {
	// name is the object name.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// GitAuth authenticates a git clone. Exactly one of the SSH pair
// (sshKeySecretRef with knownHostsSecretRef) or tokenSecretRef is set.
// +kubebuilder:validation:XValidation:rule="[has(self.sshKeySecretRef), has(self.tokenSecretRef)].exists_one(x, x)",message="exactly one of sshKeySecretRef or tokenSecretRef must be set"
// +kubebuilder:validation:XValidation:rule="has(self.sshKeySecretRef) == has(self.knownHostsSecretRef)",message="sshKeySecretRef and knownHostsSecretRef must be set together"
type GitAuth struct {
	// sshKeySecretRef is the private key used for SSH remotes.
	// +optional
	SSHKeySecretRef *SecretKeyRef `json:"sshKeySecretRef,omitempty"`

	// knownHostsSecretRef is the known_hosts file; required with sshKeySecretRef.
	// +optional
	KnownHostsSecretRef *SecretKeyRef `json:"knownHostsSecretRef,omitempty"`

	// tokenSecretRef is the access token used for HTTPS remotes.
	// +optional
	TokenSecretRef *SecretKeyRef `json:"tokenSecretRef,omitempty"`

	// username is the HTTP basic-auth user paired with the token; ignored with SSH.
	// +kubebuilder:default=x-access-token
	// +optional
	Username string `json:"username,omitempty"`
}

// NamespaceRule limits the namespaces an action may target.
type NamespaceRule struct {
	// allow lists permitted target namespaces; "*" permits any. Empty means the action's own namespace.
	// +optional
	Allow []string `json:"allow,omitempty"`

	// deny lists namespaces that are never permitted.
	// +optional
	Deny []string `json:"deny,omitempty"`
}

// ValueOrSecret is a named value that is either literal or read from a Secret key.
// +kubebuilder:validation:XValidation:rule="[has(self.value), has(self.secretRef)].exists_one(x, x)",message="exactly one of value or secretRef must be set"
type ValueOrSecret struct {
	// name is the environment variable or header name.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// value is a literal value.
	// +optional
	Value *string `json:"value,omitempty"`

	// secretRef reads the value from a Secret key.
	// +optional
	SecretRef *SecretKeyRef `json:"secretRef,omitempty"`
}
