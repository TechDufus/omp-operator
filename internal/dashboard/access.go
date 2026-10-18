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

// Package dashboard serves omp-operator's read-only web UI: server-rendered
// pages over agents, triggers and sessions, behind OIDC sign-in and/or
// HTTP basic auth, with per-namespace access rules.
package dashboard

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"sigs.k8s.io/yaml"
)

const (
	// RoleViewer is the only role in v0.1.
	RoleViewer = "viewer"
	// BasicUserPrefix prefixes basic-auth user names in access rules.
	BasicUserPrefix = "basic:"
	// AllNamespaces in a rule matches every namespace the operator serves.
	AllNamespaces = "*"

	defaultUsernameClaim = "email"
	defaultGroupsClaim   = "groups"
)

// OIDCConfig configures OpenID Connect sign-in.
type OIDCConfig struct {
	IssuerURL        string   `json:"issuerURL"`
	ClientID         string   `json:"clientID"`
	ClientSecretFile string   `json:"clientSecretFile"`
	RedirectURL      string   `json:"redirectURL"`
	Scopes           []string `json:"scopes,omitempty"`
	UsernameClaim    string   `json:"usernameClaim,omitempty"`
	GroupsClaim      string   `json:"groupsClaim,omitempty"`
}

// BasicConfig configures HTTP basic auth.
type BasicConfig struct {
	// HtpasswdFile holds "name:bcrypt-hash" lines.
	HtpasswdFile string `json:"htpasswdFile"`
}

// Rule grants a role on namespaces to users or groups.
type Rule struct {
	// Users are OIDC usernames, or "basic:<name>" for basic-auth users.
	Users []string `json:"users,omitempty"`
	// Groups are OIDC group names.
	Groups []string `json:"groups,omitempty"`
	// Namespaces are the granted namespaces; "*" means every namespace the operator serves.
	Namespaces []string `json:"namespaces"`
	// Role is "viewer" (the default and only role).
	Role string `json:"role,omitempty"`
}

// Access is the dashboard's authentication configuration and access rules,
// loaded once at startup with [LoadAccess].
type Access struct {
	OIDC  *OIDCConfig  `json:"oidc,omitempty"`
	Basic *BasicConfig `json:"basic,omitempty"`
	Rules []Rule       `json:"rules,omitempty"`

	oidcClientSecret string
	htpasswd         map[string][]byte // basic user -> bcrypt hash
}

// LoadAccess reads and validates the access file. A missing file yields an
// Access with no authentication method, so the dashboard answers 503 until one
// is configured. Secret files named by the config are read here.
func LoadAccess(path string) (*Access, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Access{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read access config: %w", err)
	}
	a := &Access{}
	if err := yaml.UnmarshalStrict(raw, a); err != nil {
		return nil, fmt.Errorf("parse access config %s: %w", path, err)
	}
	if err := a.validate(); err != nil {
		return nil, fmt.Errorf("access config %s: %w", path, err)
	}
	return a, nil
}

func (a *Access) validate() error {
	if o := a.OIDC; o != nil {
		switch {
		case o.IssuerURL == "":
			return errors.New("oidc.issuerURL is required")
		case o.ClientID == "":
			return errors.New("oidc.clientID is required")
		case o.ClientSecretFile == "":
			return errors.New("oidc.clientSecretFile is required")
		case o.RedirectURL == "":
			return errors.New("oidc.redirectURL is required")
		}
		if len(o.Scopes) == 0 {
			o.Scopes = []string{"openid", "email", "profile", "groups"}
		}
		if !slices.Contains(o.Scopes, "openid") {
			o.Scopes = append([]string{"openid"}, o.Scopes...)
		}
		if o.UsernameClaim == "" {
			o.UsernameClaim = defaultUsernameClaim
		}
		if o.GroupsClaim == "" {
			o.GroupsClaim = defaultGroupsClaim
		}
		secret, err := os.ReadFile(o.ClientSecretFile)
		if err != nil {
			return fmt.Errorf("read oidc.clientSecretFile: %w", err)
		}
		a.oidcClientSecret = strings.TrimSpace(string(secret))
		if a.oidcClientSecret == "" {
			return errors.New("oidc.clientSecretFile is empty")
		}
	}
	if b := a.Basic; b != nil {
		if b.HtpasswdFile == "" {
			return errors.New("basic.htpasswdFile is required")
		}
		raw, err := os.ReadFile(b.HtpasswdFile)
		if err != nil {
			return fmt.Errorf("read basic.htpasswdFile: %w", err)
		}
		if a.htpasswd, err = parseHtpasswd(raw); err != nil {
			return fmt.Errorf("basic.htpasswdFile: %w", err)
		}
	}
	for i := range a.Rules {
		r := &a.Rules[i]
		if len(r.Users) == 0 && len(r.Groups) == 0 {
			return fmt.Errorf("rules[%d]: users or groups is required", i)
		}
		if len(r.Namespaces) == 0 {
			return fmt.Errorf("rules[%d]: namespaces is required", i)
		}
		if slices.Contains(r.Namespaces, "") {
			return fmt.Errorf("rules[%d]: empty namespace", i)
		}
		if r.Role == "" {
			r.Role = RoleViewer
		}
		if r.Role != RoleViewer {
			return fmt.Errorf("rules[%d]: role %q is not supported (only %q)", i, r.Role, RoleViewer)
		}
	}
	return nil
}

// parseHtpasswd reads "name:hash" lines, accepting bcrypt hashes only.
func parseHtpasswd(raw []byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	for n, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		name, hash, ok := bytes.Cut(line, []byte(":"))
		if !ok || len(name) == 0 || len(hash) == 0 {
			return nil, fmt.Errorf("line %d: want name:hash", n+1)
		}
		if _, err := bcrypt.Cost(hash); err != nil {
			return nil, fmt.Errorf("line %d: user %q: only bcrypt hashes are supported: %w", n+1, name, err)
		}
		if _, dup := out[string(name)]; dup {
			return nil, fmt.Errorf("line %d: duplicate user %q", n+1, name)
		}
		out[string(name)] = bytes.Clone(hash)
	}
	return out, nil
}

// configured reports whether any authentication method is set.
func (a *Access) configured() bool {
	return a != nil && (a.OIDC != nil || a.Basic != nil)
}

// principal is an authenticated user.
type principal struct {
	Name   string
	Groups []string
	// Session is true when the user signed in through OIDC (cookie), so logout makes sense.
	Session bool
}

func (a *Access) matches(r *Rule, p *principal) bool {
	if slices.Contains(r.Users, p.Name) {
		return true
	}
	for _, g := range p.Groups {
		if slices.Contains(r.Groups, g) {
			return true
		}
	}
	return false
}

// allows reports whether a rule grants p access to ns (before the operator's own namespace filter).
func (a *Access) allows(p *principal, ns string) bool {
	for i := range a.Rules {
		r := &a.Rules[i]
		if a.matches(r, p) && (slices.Contains(r.Namespaces, AllNamespaces) || slices.Contains(r.Namespaces, ns)) {
			return true
		}
	}
	return false
}

// scope returns the namespaces p is granted: all=true when any matching rule
// carries "*", otherwise the explicit list.
func (a *Access) scope(p *principal) (all bool, namespaces []string) {
	for i := range a.Rules {
		r := &a.Rules[i]
		if !a.matches(r, p) {
			continue
		}
		if slices.Contains(r.Namespaces, AllNamespaces) {
			return true, nil
		}
		for _, ns := range r.Namespaces {
			if !slices.Contains(namespaces, ns) {
				namespaces = append(namespaces, ns)
			}
		}
	}
	slices.Sort(namespaces)
	return false, namespaces
}

// ruleGroups keeps only the groups some rule refers to, bounding the cookie's size.
func (a *Access) ruleGroups(groups []string) []string {
	var out []string
	for _, g := range groups {
		if slices.ContainsFunc(a.Rules, func(r Rule) bool { return slices.Contains(r.Groups, g) }) && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}
