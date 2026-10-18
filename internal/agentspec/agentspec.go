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

// Package agentspec validates an OmpAgent spec without cluster access and lists
// what it refers to. Reconciling code and pod builders share its rules.
package agentspec

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
)

// Reasons of the Ready condition that Validate can produce.
const (
	ReasonInvalidSpec       = "InvalidSpec"
	ReasonDuplicateName     = "DuplicateName"
	ReasonInvalidCredential = "InvalidCredential"
)

// Problem is why an agent spec is not valid.
type Problem struct {
	Reason  string
	Message string
}

func invalidSpec(format string, args ...any) *Problem {
	return &Problem{ReasonInvalidSpec, fmt.Sprintf(format, args...)}
}

// Reserved names of credentials (plan step 2).
var (
	reservedEnv       = map[string]bool{"HOME": true, "PATH": true, "KUBECONFIG": true}
	reservedEnvPrefix = []string{"OMP_", "MISE_"}
	reservedFileDirs  = []string{"/workspace", "/home/agent", "/tmp", "/opt/mise", "/var/run/omp-operator"}
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Validate checks everything that needs no cluster access: unions, MCP
// consistency, name uniqueness and the credential rules. It returns the first
// problem found, or nil.
func Validate(a *ompv1alpha1.OmpAgent) *Problem {
	if p := validateStructure(&a.Spec); p != nil {
		return p
	}
	if p := validateUniqueNames(&a.Spec); p != nil {
		return p
	}
	return validateCredentials(a.Spec.Credentials)
}

func validateStructure(spec *ompv1alpha1.OmpAgentSpec) *Problem {
	if p := validateModel(&spec.Model); p != nil {
		return p
	}
	for i := range spec.Skills {
		if p := validateSkill(i, &spec.Skills[i]); p != nil {
			return p
		}
	}
	if w := spec.Workspace; w != nil {
		if p := validateWorkspace(w); p != nil {
			return p
		}
	}
	for i := range spec.Credentials {
		c := &spec.Credentials[i]
		if msg := dnsLabelError(c.Name); msg != "" {
			return invalidSpec("credentials[%d]: name %q: %s", i, c.Name, msg)
		}
		if (c.Env == "") == (c.File == "") {
			return invalidSpec("credential %q: exactly one of env or file must be set", c.Name)
		}
		if c.Description == "" {
			return invalidSpec("credential %q: description is required", c.Name)
		}
		if c.SecretRef.Name == "" || c.SecretRef.Key == "" {
			return invalidSpec("credential %q: secretRef needs a name and a key", c.Name)
		}
	}
	for i := range spec.MCPServers {
		if p := validateMCP(&spec.MCPServers[i]); p != nil {
			return p
		}
	}
	for i, a := range spec.Actions {
		if a.Name == "" {
			return invalidSpec("actions[%d]: name is required", i)
		}
	}
	for i := range spec.Notify {
		if p := validateSink(i, &spec.Notify[i]); p != nil {
			return p
		}
	}
	return nil
}

func dnsLabelError(name string) string {
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return errs[0]
	}
	return ""
}

func validateModel(m *ompv1alpha1.ModelSpec) *Problem {
	if !providerModel(m.Default) {
		return invalidSpec("model.default %q must be provider/id", m.Default)
	}
	if m.Small != "" && !providerModel(m.Small) {
		return invalidSpec("model.small %q must be provider/id", m.Small)
	}
	if g := m.Gateway; g != nil {
		if !absoluteHTTPURL(g.URL) {
			return invalidSpec("model.gateway.url must be an absolute http(s) URL")
		}
		if r := g.TokenSecretRef; r != nil && (r.Name == "" || r.Key == "") {
			return invalidSpec("model.gateway.tokenSecretRef needs a name and a key")
		}
	}
	return nil
}

func providerModel(s string) bool {
	provider, id, ok := strings.Cut(s, "/")
	return ok && provider != "" && id != ""
}

func absoluteHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// safeRelPath reports whether p is a relative path that stays inside its base.
func safeRelPath(p string) bool {
	if p == "" || path.IsAbs(p) {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

func validateSkill(i int, s *ompv1alpha1.SkillSource) *Problem {
	n := 0
	for _, set := range []bool{s.Git != nil, s.ConfigMap != nil, s.FluxSource != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return invalidSpec("skills[%d]: exactly one of git, configMap or fluxSource must be set", i)
	}
	switch {
	case s.Git != nil:
		if s.Git.URL == "" {
			return invalidSpec("skills[%d].git: url is required", i)
		}
		if s.Git.Path != "" && !safeRelPath(s.Git.Path) {
			return invalidSpec("skills[%d].git: path must be relative and must not contain '..'", i)
		}
		return validateGitAuth(fmt.Sprintf("skills[%d].git", i), s.Git.Auth)
	case s.ConfigMap != nil:
		if s.ConfigMap.Name == "" {
			return invalidSpec("skills[%d].configMap: name is required", i)
		}
	default:
		if p := validateFluxRef(fmt.Sprintf("skills[%d].fluxSource", i), &s.FluxSource.FluxSourceRef); p != nil {
			return p
		}
		if s.FluxSource.Path != "" && !safeRelPath(s.FluxSource.Path) {
			return invalidSpec("skills[%d].fluxSource: path must be relative and must not contain '..'", i)
		}
	}
	return nil
}

func validateFluxRef(where string, r *ompv1alpha1.FluxSourceRef) *Problem {
	if r.Kind != ompv1alpha1.FluxSourceGitRepository && r.Kind != ompv1alpha1.FluxSourceOCIRepository {
		return invalidSpec("%s: kind must be GitRepository or OCIRepository", where)
	}
	if r.Namespace == "" || r.Name == "" {
		return invalidSpec("%s: namespace and name are required", where)
	}
	return nil
}

func validateGitAuth(where string, a *ompv1alpha1.GitAuth) *Problem {
	if a == nil {
		return nil
	}
	ssh := a.SSHKeySecretRef != nil || a.KnownHostsSecretRef != nil
	token := a.TokenSecretRef != nil
	if ssh == token {
		return invalidSpec("%s.auth: exactly one of the SSH pair (sshKeySecretRef with knownHostsSecretRef) or tokenSecretRef must be set", where)
	}
	if ssh && (a.SSHKeySecretRef == nil || a.KnownHostsSecretRef == nil) {
		return invalidSpec("%s.auth: sshKeySecretRef and knownHostsSecretRef must be set together", where)
	}
	for _, r := range []*ompv1alpha1.SecretKeyRef{a.SSHKeySecretRef, a.KnownHostsSecretRef, a.TokenSecretRef} {
		if r != nil && (r.Name == "" || r.Key == "") {
			return invalidSpec("%s.auth: a secretRef needs a name and a key", where)
		}
	}
	return nil
}

func validateWorkspace(w *ompv1alpha1.WorkspaceSpec) *Problem {
	if (w.Git == nil) == (w.FluxSource == nil) {
		return invalidSpec("workspace: exactly one of git or fluxSource must be set")
	}
	if w.Git != nil {
		if w.Git.URL == "" {
			return invalidSpec("workspace.git: url is required")
		}
		if p := validateGitAuth("workspace.git", w.Git.Auth); p != nil {
			return p
		}
	} else if p := validateFluxRef("workspace.fluxSource", w.FluxSource); p != nil {
		return p
	}
	for i, l := range w.Links {
		if !safeRelPath(l.Path) {
			return invalidSpec("workspace.links[%d]: path must be relative and must not contain '..'", i)
		}
		if !path.IsAbs(l.Target) {
			return invalidSpec("workspace.links[%d]: target must be an absolute path", i)
		}
	}
	return nil
}

func validateValueOrSecret(where string, vs []ompv1alpha1.ValueOrSecret) *Problem {
	for i, v := range vs {
		if v.Name == "" {
			return invalidSpec("%s[%d]: name is required", where, i)
		}
		if (v.Value == nil) == (v.SecretRef == nil) {
			return invalidSpec("%s %q: exactly one of value or secretRef must be set", where, v.Name)
		}
		if v.SecretRef != nil && (v.SecretRef.Name == "" || v.SecretRef.Key == "") {
			return invalidSpec("%s %q: secretRef needs a name and a key", where, v.Name)
		}
	}
	return nil
}

// MCPType returns the server's transport, applying the computed default: stdio
// with a command, http with a url. It is empty when neither is set.
func MCPType(m *ompv1alpha1.MCPServer) ompv1alpha1.MCPServerType {
	switch {
	case m.Type != "":
		return m.Type
	case len(m.Command) > 0:
		return ompv1alpha1.MCPStdio
	case m.URL != "":
		return ompv1alpha1.MCPHTTP
	}
	return ""
}

func validateMCP(m *ompv1alpha1.MCPServer) *Problem {
	if msg := dnsLabelError(m.Name); msg != "" {
		return invalidSpec("mcpServers: name %q: %s", m.Name, msg)
	}
	switch MCPType(m) {
	case ompv1alpha1.MCPStdio:
		if len(m.Command) == 0 || m.Command[0] == "" {
			return invalidSpec("mcpServer %q: a stdio server needs a command", m.Name)
		}
		if m.URL != "" || len(m.Headers) > 0 {
			return invalidSpec("mcpServer %q: a stdio server takes no url or headers", m.Name)
		}
		return validateValueOrSecret(fmt.Sprintf("mcpServer %q env", m.Name), m.Env)
	case ompv1alpha1.MCPHTTP, ompv1alpha1.MCPSSE:
		if !absoluteHTTPURL(m.URL) {
			return invalidSpec("mcpServer %q: an http or sse server needs an absolute http(s) url", m.Name)
		}
		if len(m.Command) > 0 || len(m.Env) > 0 {
			return invalidSpec("mcpServer %q: an http or sse server takes no command or env", m.Name)
		}
		return validateValueOrSecret(fmt.Sprintf("mcpServer %q header", m.Name), m.Headers)
	}
	return invalidSpec("mcpServer %q: set a command (stdio) or a url (http, sse), or a valid type", m.Name)
}

func validateSink(i int, n *ompv1alpha1.NotifySink) *Problem {
	members := map[ompv1alpha1.SinkType]bool{
		ompv1alpha1.SinkNtfy: n.Ntfy != nil, ompv1alpha1.SinkSlack: n.Slack != nil,
		ompv1alpha1.SinkDiscord: n.Discord != nil, ompv1alpha1.SinkWebhook: n.Webhook != nil,
	}
	set := 0
	for _, ok := range members {
		if ok {
			set++
		}
	}
	if set != 1 || !members[n.Type] {
		return invalidSpec("notify[%d]: exactly one sink member, the one named by type, must be set", i)
	}
	for _, r := range SinkRefs(n) {
		if r.Name == "" || r.Key == "" {
			return invalidSpec("notify[%d]: a secretRef needs a name and a key", i)
		}
	}
	return nil
}

// SinkRefs lists the Secret keys a notify sink refers to.
func SinkRefs(n *ompv1alpha1.NotifySink) []ompv1alpha1.SecretKeyRef {
	switch {
	case n.Ntfy != nil:
		return []ompv1alpha1.SecretKeyRef{n.Ntfy.TopicSecretRef}
	case n.Slack != nil:
		return []ompv1alpha1.SecretKeyRef{n.Slack.WebhookURLSecretRef}
	case n.Discord != nil:
		return []ompv1alpha1.SecretKeyRef{n.Discord.WebhookURLSecretRef}
	case n.Webhook != nil:
		refs := []ompv1alpha1.SecretKeyRef{n.Webhook.URLSecretRef}
		if n.Webhook.HMACSecretRef != nil {
			refs = append(refs, *n.Webhook.HMACSecretRef)
		}
		return refs
	}
	return nil
}

func validateUniqueNames(spec *ompv1alpha1.OmpAgentSpec) *Problem {
	seen := map[string]bool{}
	for _, c := range spec.Credentials {
		if seen[c.Name] {
			return &Problem{ReasonDuplicateName, fmt.Sprintf("credential name %q is used more than once", c.Name)}
		}
		seen[c.Name] = true
	}
	seen = map[string]bool{}
	for _, m := range spec.MCPServers {
		if seen[m.Name] {
			return &Problem{ReasonDuplicateName, fmt.Sprintf("mcpServer name %q is used more than once", m.Name)}
		}
		seen[m.Name] = true
	}
	seen = map[string]bool{}
	for _, a := range spec.Actions {
		if seen[a.Name] {
			return &Problem{ReasonDuplicateName, fmt.Sprintf("action %q is listed more than once", a.Name)}
		}
		seen[a.Name] = true
	}
	return nil
}

func invalidCredential(name, format string, args ...any) *Problem {
	return &Problem{ReasonInvalidCredential, fmt.Sprintf("credential %q: %s", name, fmt.Sprintf(format, args...))}
}

func validateCredentials(creds []ompv1alpha1.Credential) *Problem {
	envs := map[string]string{}
	files := map[string]string{}
	for _, c := range creds {
		if c.Env != "" {
			if !envNamePattern.MatchString(c.Env) {
				return invalidCredential(c.Name, "env %q is not a valid variable name", c.Env)
			}
			if reservedEnv[c.Env] {
				return invalidCredential(c.Name, "env %q is reserved", c.Env)
			}
			for _, p := range reservedEnvPrefix {
				if strings.HasPrefix(c.Env, p) {
					return invalidCredential(c.Name, "env %q is reserved: names starting with %s are used by the operator", c.Env, p)
				}
			}
			if other, dup := envs[c.Env]; dup {
				return invalidCredential(c.Name, "env %q is already used by credential %q", c.Env, other)
			}
			envs[c.Env] = c.Name
			continue
		}
		if !path.IsAbs(c.File) || path.Clean(c.File) != c.File || c.File == "/" {
			return invalidCredential(c.Name, "file %q must be a clean absolute path to a file", c.File)
		}
		for _, dir := range reservedFileDirs {
			if c.File == dir || strings.HasPrefix(c.File, dir+"/") {
				return invalidCredential(c.Name, "file %q is under %s, which the operator reserves", c.File, dir)
			}
		}
		if other, dup := files[c.File]; dup {
			return invalidCredential(c.Name, "file %q is already used by credential %q", c.File, other)
		}
		files[c.File] = c.Name
	}
	return nil
}

// SecretUse is one Secret key an agent's spec refers to, with where it is used.
type SecretUse struct {
	What string
	Ref  ompv1alpha1.SecretKeyRef
}

// SecretRefs lists every Secret key the spec refers to, in spec order.
func SecretRefs(spec *ompv1alpha1.OmpAgentSpec) []SecretUse {
	var out []SecretUse
	add := func(what string, r *ompv1alpha1.SecretKeyRef) {
		if r != nil {
			out = append(out, SecretUse{what, *r})
		}
	}
	addAuth := func(what string, a *ompv1alpha1.GitAuth) {
		if a != nil {
			add(what+" sshKey", a.SSHKeySecretRef)
			add(what+" knownHosts", a.KnownHostsSecretRef)
			add(what+" token", a.TokenSecretRef)
		}
	}
	if g := spec.Model.Gateway; g != nil {
		add("model.gateway token", g.TokenSecretRef)
	}
	for _, c := range spec.Credentials {
		add(fmt.Sprintf("credential %q", c.Name), &c.SecretRef)
	}
	for _, m := range spec.MCPServers {
		for _, v := range m.Env {
			add(fmt.Sprintf("mcpServer %q env %q", m.Name, v.Name), v.SecretRef)
		}
		for _, v := range m.Headers {
			add(fmt.Sprintf("mcpServer %q header %q", m.Name, v.Name), v.SecretRef)
		}
	}
	for i, s := range spec.Skills {
		if s.Git != nil {
			addAuth(fmt.Sprintf("skills[%d].git", i), s.Git.Auth)
		}
	}
	if w := spec.Workspace; w != nil && w.Git != nil {
		addAuth("workspace.git", w.Git.Auth)
	}
	for i := range spec.Notify {
		for j, r := range SinkRefs(&spec.Notify[i]) {
			add(fmt.Sprintf("notify[%d] %s secret %d", i, spec.Notify[i].Type, j+1), &r)
		}
	}
	return out
}
