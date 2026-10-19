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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"sigs.k8s.io/yaml"
)

const (
	// RoleViewer may read: the dashboard and the read routes of the cloud API.
	RoleViewer = "viewer"
	// RoleOperator may also change things through the cloud API; it includes RoleViewer.
	RoleOperator = "operator"
	// BasicUserPrefix prefixes basic-auth user names in access rules.
	BasicUserPrefix = "basic:"
	// TokenUserPrefix prefixes cloud API token names in access rules ("token:<name>").
	TokenUserPrefix = "token:"
	// AllNamespaces in a rule matches every namespace the operator serves.
	AllNamespaces = "*"

	defaultUsernameClaim = "email"
	defaultGroupsClaim   = "groups"

	// minAPITokenLen is the shortest accepted token read from a tokenFile.
	minAPITokenLen = 32
)

var tokenNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

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
	// Users are OIDC usernames, "basic:<name>" for basic-auth users, or "token:<name>" for cloud API tokens.
	Users []string `json:"users,omitempty"`
	// Groups are OIDC group names.
	Groups []string `json:"groups,omitempty"`
	// Namespaces are the granted namespaces; "*" means every namespace the operator serves.
	Namespaces []string `json:"namespaces"`
	// Role is "viewer" (the default) or "operator", which includes viewer.
	Role string `json:"role,omitempty"`
}

// APIToken is a bearer token of the cloud API. Exactly one of SHA256 and
// TokenFile is set.
type APIToken struct {
	// Name identifies the token in rules as "token:<name>".
	Name string `json:"name"`
	// SHA256 is the hex sha256 of the token.
	SHA256 string `json:"sha256,omitempty"`
	// TokenFile holds the token; it is read at load.
	TokenFile string `json:"tokenFile,omitempty"`
}

// Access is the dashboard's authentication configuration and access rules,
// loaded once at startup with [LoadAccess].
type Access struct {
	OIDC      *OIDCConfig  `json:"oidc,omitempty"`
	Basic     *BasicConfig `json:"basic,omitempty"`
	APITokens []APIToken   `json:"apiTokens,omitempty"`
	Rules     []Rule       `json:"rules,omitempty"`

	oidcClientSecret string
	htpasswd         map[string][]byte // basic user -> bcrypt hash
	tokens           []tokenHash
}

// tokenHash is a loaded API token.
type tokenHash struct {
	name string
	sum  [sha256.Size]byte
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
	if err := a.loadTokens(); err != nil {
		return err
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
		if r.Role != RoleViewer && r.Role != RoleOperator {
			return fmt.Errorf("rules[%d]: role %q is not supported (use %q or %q)", i, r.Role, RoleViewer, RoleOperator)
		}
	}
	return nil
}

// loadTokens validates apiTokens and keeps the sha256 of each.
func (a *Access) loadTokens() error {
	a.tokens = nil
	for i := range a.APITokens {
		t := &a.APITokens[i]
		if !tokenNamePattern.MatchString(t.Name) {
			return fmt.Errorf("apiTokens[%d]: name %q must match %s", i, t.Name, tokenNamePattern)
		}
		if slices.ContainsFunc(a.tokens, func(o tokenHash) bool { return o.name == t.Name }) {
			return fmt.Errorf("apiTokens[%d]: duplicate name %q", i, t.Name)
		}
		if (t.SHA256 == "") == (t.TokenFile == "") {
			return fmt.Errorf("apiTokens[%d] (%s): exactly one of sha256 and tokenFile is required", i, t.Name)
		}
		th := tokenHash{name: t.Name}
		if t.SHA256 != "" {
			raw, err := hex.DecodeString(t.SHA256)
			if err != nil || len(raw) != sha256.Size {
				return fmt.Errorf("apiTokens[%d] (%s): sha256 must be 64 hex characters", i, t.Name)
			}
			copy(th.sum[:], raw)
		} else {
			raw, err := os.ReadFile(t.TokenFile)
			if err != nil {
				return fmt.Errorf("apiTokens[%d] (%s): read tokenFile: %w", i, t.Name, err)
			}
			tok := strings.TrimSpace(string(raw))
			if len(tok) < minAPITokenLen {
				return fmt.Errorf("apiTokens[%d] (%s): token is shorter than %d characters", i, t.Name, minAPITokenLen)
			}
			th.sum = sha256.Sum256([]byte(tok))
		}
		a.tokens = append(a.tokens, th)
	}
	return nil
}

// TokensConfigured reports whether any API token is configured.
func (a *Access) TokensConfigured() bool { return a != nil && len(a.tokens) > 0 }

// TokenPrincipal returns the principal "token:<name>" of a bearer token. It
// compares against every stored hash in constant time.
func (a *Access) TokenPrincipal(token string) (string, bool) {
	if a == nil || token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	name, found := "", 0
	for i := range a.tokens {
		eq := subtle.ConstantTimeCompare(sum[:], a.tokens[i].sum[:])
		if eq == 1 {
			name = a.tokens[i].name
		}
		found |= eq
	}
	if found != 1 {
		return "", false
	}
	return TokenUserPrefix + name, true
}

// Allows reports whether the rules grant user (with groups) at least role on
// namespace ns: RoleOperator needs an operator rule, RoleViewer any rule. It
// does not apply the operator's own namespace filter.
func (a *Access) Allows(user string, groups []string, ns, role string) bool {
	if a == nil {
		return false
	}
	p := &principal{Name: user, Groups: groups}
	for i := range a.Rules {
		r := &a.Rules[i]
		inScope := slices.Contains(r.Namespaces, AllNamespaces) || slices.Contains(r.Namespaces, ns)
		if !a.matches(r, p) || !inScope {
			continue
		}
		if role != RoleOperator || r.Role == RoleOperator {
			return true
		}
	}
	return false
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
