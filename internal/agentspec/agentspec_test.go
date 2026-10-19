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

package agentspec

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
)

const (
	testURL      = "http://x"
	wantReserved = "reserved"
)

func ref(name string) ompv1alpha1.SecretKeyRef { return ompv1alpha1.SecretKeyRef{Name: name, Key: "k"} }

func base() *ompv1alpha1.OmpAgent {
	return &ompv1alpha1.OmpAgent{Spec: ompv1alpha1.OmpAgentSpec{
		Model: ompv1alpha1.ModelSpec{Default: "anthropic/claude-sonnet-5-5"},
	}}
}

func TestValidate(t *testing.T) {
	cred := func(name, env, file string) ompv1alpha1.Credential {
		return ompv1alpha1.Credential{Name: name, Description: "d", SecretRef: ref("s"), Env: env, File: file}
	}
	val := "v"
	tests := []struct {
		name   string
		mutate func(*ompv1alpha1.OmpAgentSpec)
		reason string // empty = valid
		substr string
	}{
		{"minimal", func(*ompv1alpha1.OmpAgentSpec) {}, "", ""},
		{"model without provider", func(s *ompv1alpha1.OmpAgentSpec) { s.Model.Default = "claude" }, ReasonInvalidSpec, "provider/id"},
		{"small without id", func(s *ompv1alpha1.OmpAgentSpec) { s.Model.Small = "anthropic/" }, ReasonInvalidSpec, "model.small"},
		{"gateway url", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Model.Gateway = &ompv1alpha1.GatewaySpec{URL: "omp-auth:4000"}
		}, ReasonInvalidSpec, "gateway.url"},
		{"skill union none", func(s *ompv1alpha1.OmpAgentSpec) { s.Skills = []ompv1alpha1.SkillSource{{}} }, ReasonInvalidSpec, "skills[0]"},
		{"skill union two", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Skills = []ompv1alpha1.SkillSource{{Git: &ompv1alpha1.GitSkillSource{URL: "u"}, ConfigMap: &ompv1alpha1.LocalObjectRef{Name: "c"}}}
		}, ReasonInvalidSpec, "exactly one"},
		{"skill path escapes", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Skills = []ompv1alpha1.SkillSource{{Git: &ompv1alpha1.GitSkillSource{URL: "u", Path: "../x"}}}
		}, ReasonInvalidSpec, "path"},
		{"skill flux kind", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Skills = []ompv1alpha1.SkillSource{{FluxSource: &ompv1alpha1.FluxSkillSource{FluxSourceRef: ompv1alpha1.FluxSourceRef{Kind: "Bucket", Namespace: "n", Name: "x"}}}}
		}, ReasonInvalidSpec, "kind"},
		{"git auth both", func(s *ompv1alpha1.OmpAgentSpec) {
			a := ref("a")
			s.Skills = []ompv1alpha1.SkillSource{{Git: &ompv1alpha1.GitSkillSource{URL: "u", Auth: &ompv1alpha1.GitAuth{SSHKeySecretRef: &a, KnownHostsSecretRef: &a, TokenSecretRef: &a}}}}
		}, ReasonInvalidSpec, "auth"},
		{"git auth half ssh", func(s *ompv1alpha1.OmpAgentSpec) {
			a := ref("a")
			s.Workspace = &ompv1alpha1.WorkspaceSpec{Git: &ompv1alpha1.GitWorkspaceSource{URL: "u", Auth: &ompv1alpha1.GitAuth{SSHKeySecretRef: &a}}}
		}, ReasonInvalidSpec, "together"},
		{"workspace union none", func(s *ompv1alpha1.OmpAgentSpec) { s.Workspace = &ompv1alpha1.WorkspaceSpec{} }, ReasonInvalidSpec, "workspace"},
		{"workspace link escapes", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Workspace = &ompv1alpha1.WorkspaceSpec{Git: &ompv1alpha1.GitWorkspaceSource{URL: "u"}, Links: []ompv1alpha1.WorkspaceLink{{Path: "a/../../b", Target: "/x"}}}
		}, ReasonInvalidSpec, "links[0]"},
		{"workspace link target", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Workspace = &ompv1alpha1.WorkspaceSpec{Git: &ompv1alpha1.GitWorkspaceSource{URL: "u"}, Links: []ompv1alpha1.WorkspaceLink{{Path: "a", Target: "x"}}}
		}, ReasonInvalidSpec, "absolute"},
		{"credential env and file", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "TOKEN", "/etc/a")}
		}, ReasonInvalidSpec, "exactly one of env or file"},
		{"credential neither", func(s *ompv1alpha1.OmpAgentSpec) { s.Credentials = []ompv1alpha1.Credential{cred("a", "", "")} }, ReasonInvalidSpec, "exactly one"},
		{"credential name not a label", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("Bad_Name", "TOKEN", "")}
		}, ReasonInvalidSpec, "Bad_Name"},
		{"credential ok", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "TOKEN", ""), cred("b", "", "/etc/creds/b"), cred("c", "", "/etc/creds/c")}
		}, "", ""},
		{"credential duplicate name", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "A", ""), cred("a", "B", "")}
		}, ReasonDuplicateName, `"a"`},
		{"mcp duplicate name", func(s *ompv1alpha1.OmpAgentSpec) {
			s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m", Command: []string{"x"}}, {Name: "m", URL: testURL}}
		}, ReasonDuplicateName, "mcpServer"},
		{"action listed twice", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Actions = []ompv1alpha1.LocalObjectRef{{Name: "r"}, {Name: "r"}}
		}, ReasonDuplicateName, "action"},
		{"env HOME", func(s *ompv1alpha1.OmpAgentSpec) { s.Credentials = []ompv1alpha1.Credential{cred("a", "HOME", "")} }, ReasonInvalidCredential, wantReserved},
		{"env PATH", func(s *ompv1alpha1.OmpAgentSpec) { s.Credentials = []ompv1alpha1.Credential{cred("a", "PATH", "")} }, ReasonInvalidCredential, wantReserved},
		{"env KUBECONFIG", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "KUBECONFIG", "")}
		}, ReasonInvalidCredential, wantReserved},
		{"env OMP_ prefix", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "OMP_API_KEY", "")}
		}, ReasonInvalidCredential, "OMP_"},
		{"env MISE_ prefix", func(s *ompv1alpha1.OmpAgentSpec) { s.Credentials = []ompv1alpha1.Credential{cred("a", "MISE_X", "")} }, ReasonInvalidCredential, "MISE_"},
		{"env not reserved lookalike", func(s *ompv1alpha1.OmpAgentSpec) { s.Credentials = []ompv1alpha1.Credential{cred("a", "HOMEDIR", "")} }, "", ""},
		{"env duplicated", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "T", ""), cred("b", "T", "")}
		}, ReasonInvalidCredential, "already used"},
		{"file under /workspace", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "", "/workspace/k")}
		}, ReasonInvalidCredential, "/workspace"},
		{"file under /home/agent", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "", "/home/agent/.ssh/id")}
		}, ReasonInvalidCredential, "/home/agent"},
		{"file under /tmp", func(s *ompv1alpha1.OmpAgentSpec) { s.Credentials = []ompv1alpha1.Credential{cred("a", "", "/tmp/k")} }, ReasonInvalidCredential, "/tmp"},
		{"file under /opt/mise", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "", "/opt/mise/k")}
		}, ReasonInvalidCredential, "/opt/mise"},
		{"file under run dir", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "", "/var/run/omp-operator/token/token")}
		}, ReasonInvalidCredential, "/var/run/omp-operator"},
		{"file near a reserved dir", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "", "/tmpfiles/k")}
		}, "", ""},
		{"file path twice", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "", "/etc/x"), cred("b", "", "/etc/x")}
		}, ReasonInvalidCredential, "already used"},
		{"file not clean", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Credentials = []ompv1alpha1.Credential{cred("a", "", "/etc/../tmp/k")}
		}, ReasonInvalidCredential, "clean"},
		{"mcp stdio ok", func(s *ompv1alpha1.OmpAgentSpec) {
			s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m", Command: []string{"mcp-server", "--x"}, Env: []ompv1alpha1.ValueOrSecret{{Name: "A", Value: &val}}}}
		}, "", ""},
		{"mcp http ok", func(s *ompv1alpha1.OmpAgentSpec) {
			s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m", URL: "https://mcp.example.com/sse", Type: ompv1alpha1.MCPSSE, Headers: []ompv1alpha1.ValueOrSecret{{Name: "Authorization", SecretRef: new(ref("s"))}}}}
		}, "", ""},
		{"mcp nothing set", func(s *ompv1alpha1.OmpAgentSpec) { s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m"}} }, ReasonInvalidSpec, "command"},
		{"mcp stdio without command", func(s *ompv1alpha1.OmpAgentSpec) {
			s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m", Type: ompv1alpha1.MCPStdio, URL: testURL}}
		}, ReasonInvalidSpec, "command"},
		{"mcp http with command", func(s *ompv1alpha1.OmpAgentSpec) {
			s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m", Type: ompv1alpha1.MCPHTTP, URL: testURL, Command: []string{"c"}}}
		}, ReasonInvalidSpec, "no command"},
		{"mcp http relative url", func(s *ompv1alpha1.OmpAgentSpec) {
			s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m", Type: ompv1alpha1.MCPHTTP, URL: "/mcp"}}
		}, ReasonInvalidSpec, "url"},
		{"mcp stdio with headers", func(s *ompv1alpha1.OmpAgentSpec) {
			s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m", Command: []string{"c"}, Headers: []ompv1alpha1.ValueOrSecret{{Name: "H", Value: &val}}}}
		}, ReasonInvalidSpec, "no url or headers"},
		{"mcp env both value and secret", func(s *ompv1alpha1.OmpAgentSpec) {
			s.MCPServers = []ompv1alpha1.MCPServer{{Name: "m", Command: []string{"c"}, Env: []ompv1alpha1.ValueOrSecret{{Name: "A", Value: &val, SecretRef: new(ref("s"))}}}}
		}, ReasonInvalidSpec, "value or secretRef"},
		{"notify member mismatch", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Notify = []ompv1alpha1.NotifySink{{Type: ompv1alpha1.SinkSlack, Discord: &ompv1alpha1.DiscordSink{WebhookURLSecretRef: ref("s")}}}
		}, ReasonInvalidSpec, "notify[0]"},
		{"notify two members", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Notify = []ompv1alpha1.NotifySink{{Type: ompv1alpha1.SinkSlack, Slack: &ompv1alpha1.SlackSink{WebhookURLSecretRef: ref("s")}, Discord: &ompv1alpha1.DiscordSink{WebhookURLSecretRef: ref("s")}}}
		}, ReasonInvalidSpec, "notify[0]"},
		{"notify ok", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Notify = []ompv1alpha1.NotifySink{{Type: ompv1alpha1.SinkSlack, Slack: &ompv1alpha1.SlackSink{WebhookURLSecretRef: ref("s")}}}
		}, "", ""},
		{"tools all alone", func(s *ompv1alpha1.OmpAgentSpec) { s.Tools.Allow = []string{"*"} }, "", ""},
		{"tools all mixed", func(s *ompv1alpha1.OmpAgentSpec) { s.Tools.Allow = []string{"read", "*"} }, ReasonInvalidSpec, "tools.allow"},
		{"cloud defaults", func(s *ompv1alpha1.OmpAgentSpec) { s.Cloud = &ompv1alpha1.CloudSpec{} }, "", ""},
		{"cloud idleTimeout bounds ok", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Cloud = &ompv1alpha1.CloudSpec{IdleTimeout: dur(time.Minute), MaxSitting: dur(24 * time.Hour), ThreadRetention: dur(time.Hour)}
		}, "", ""},
		{"cloud idleTimeout too short", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Cloud = &ompv1alpha1.CloudSpec{IdleTimeout: dur(59 * time.Second)}
		}, ReasonInvalidSpec, "cloud.idleTimeout"},
		{"cloud idleTimeout too long", func(s *ompv1alpha1.OmpAgentSpec) { s.Cloud = &ompv1alpha1.CloudSpec{IdleTimeout: dur(25 * time.Hour)} }, ReasonInvalidSpec, "cloud.idleTimeout"},
		{"cloud maxSitting too short", func(s *ompv1alpha1.OmpAgentSpec) { s.Cloud = &ompv1alpha1.CloudSpec{MaxSitting: dur(9 * time.Minute)} }, ReasonInvalidSpec, "cloud.maxSitting"},
		{"cloud maxSitting too long", func(s *ompv1alpha1.OmpAgentSpec) { s.Cloud = &ompv1alpha1.CloudSpec{MaxSitting: dur(25 * time.Hour)} }, ReasonInvalidSpec, "cloud.maxSitting"},
		{"cloud threadRetention too short", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Cloud = &ompv1alpha1.CloudSpec{ThreadRetention: dur(59 * time.Minute)}
		}, ReasonInvalidSpec, "cloud.threadRetention"},
		{"cloud git ok", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Cloud = &ompv1alpha1.CloudSpec{Git: []ompv1alpha1.CloudGitHost{cloudHost("github.com"), cloudHost("git.example.com")}}
		}, "", ""},
		{"cloud git duplicate host", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Cloud = &ompv1alpha1.CloudSpec{Git: []ompv1alpha1.CloudGitHost{cloudHost("github.com"), cloudHost("github.com")}}
		}, ReasonInvalidSpec, "more than once"},
		{"cloud git bad host", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Cloud = &ompv1alpha1.CloudSpec{Git: []ompv1alpha1.CloudGitHost{cloudHost("GitHub.com")}}
		}, ReasonInvalidSpec, "hostname"},
		{"cloud git auth missing", func(s *ompv1alpha1.OmpAgentSpec) {
			s.Cloud = &ompv1alpha1.CloudSpec{Git: []ompv1alpha1.CloudGitHost{{Host: "github.com"}}}
		}, ReasonInvalidSpec, "cloud.git[0].auth"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := base()
			tt.mutate(&a.Spec)
			p := Validate(a)
			if tt.reason == "" {
				if p != nil {
					t.Fatalf("unexpected problem %s: %s", p.Reason, p.Message)
				}
				return
			}
			if p == nil {
				t.Fatalf("want %s, got valid", tt.reason)
			}
			if p.Reason != tt.reason || !strings.Contains(p.Message, tt.substr) {
				t.Errorf("got %s: %q, want %s containing %q", p.Reason, p.Message, tt.reason, tt.substr)
			}
		})
	}
}

func dur(d time.Duration) metav1.Duration { return metav1.Duration{Duration: d} }

func cloudHost(host string) ompv1alpha1.CloudGitHost {
	return ompv1alpha1.CloudGitHost{Host: host, Auth: ompv1alpha1.GitAuth{TokenSecretRef: new(ref("tok"))}}
}

func TestValidateCloudAgentName(t *testing.T) {
	tests := []struct {
		name  string
		cloud bool
		want  bool // valid
	}{
		{strings.Repeat("a", 50), true, true},
		{strings.Repeat("a", 51), true, false},
		{strings.Repeat("a", 51), false, true},
	}
	for _, tt := range tests {
		a := base()
		a.Name = tt.name
		if tt.cloud {
			a.Spec.Cloud = &ompv1alpha1.CloudSpec{}
		}
		p := Validate(a)
		if tt.want && p != nil {
			t.Errorf("%d chars cloud=%v: unexpected problem %v", len(tt.name), tt.cloud, p)
		}
		if !tt.want && (p == nil || p.Reason != ReasonInvalidSpec || !strings.Contains(p.Message, "at most 50")) {
			t.Errorf("%d chars cloud=%v: got %v, want InvalidSpec about 50 characters", len(tt.name), tt.cloud, p)
		}
	}
}

func TestCheckEnvName(t *testing.T) {
	tests := []struct{ name, want string }{
		{"TOKEN", ""},
		{"HOMEDIR", ""},
		{"1BAD", "not a valid"},
		{"HOME", "reserved"},
		{"PATH", "reserved"},
		{"KUBECONFIG", "reserved"},
		{"OMP_X", "OMP_"},
		{"MISE_X", "MISE_"},
	}
	for _, tt := range tests {
		got := CheckEnvName(tt.name)
		if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
			t.Errorf("CheckEnvName(%q) = %q, want containing %q", tt.name, got, tt.want)
		}
	}
}

func TestMCPTypeDefaults(t *testing.T) {
	tests := []struct {
		m    ompv1alpha1.MCPServer
		want ompv1alpha1.MCPServerType
	}{
		{ompv1alpha1.MCPServer{Command: []string{"x"}}, ompv1alpha1.MCPStdio},
		{ompv1alpha1.MCPServer{URL: testURL}, ompv1alpha1.MCPHTTP},
		{ompv1alpha1.MCPServer{URL: testURL, Type: ompv1alpha1.MCPSSE}, ompv1alpha1.MCPSSE},
		{ompv1alpha1.MCPServer{}, ""},
	}
	for _, tt := range tests {
		if got := MCPType(&tt.m); got != tt.want {
			t.Errorf("MCPType(%+v) = %q, want %q", tt.m, got, tt.want)
		}
	}
}

func TestSecretRefsCoversEverySource(t *testing.T) {
	val := "v"
	spec := ompv1alpha1.OmpAgentSpec{
		Model:       ompv1alpha1.ModelSpec{Default: "a/b", Gateway: &ompv1alpha1.GatewaySpec{URL: "http://g", TokenSecretRef: new(ref("gw"))}},
		Credentials: []ompv1alpha1.Credential{{Name: "c", SecretRef: ref("cred")}},
		MCPServers: []ompv1alpha1.MCPServer{{Name: "m",
			Env:     []ompv1alpha1.ValueOrSecret{{Name: "E", SecretRef: new(ref("mcp-env"))}, {Name: "L", Value: &val}},
			Headers: []ompv1alpha1.ValueOrSecret{{Name: "H", SecretRef: new(ref("mcp-hdr"))}}}},
		Skills: []ompv1alpha1.SkillSource{{Git: &ompv1alpha1.GitSkillSource{URL: "u", Auth: &ompv1alpha1.GitAuth{TokenSecretRef: new(ref("skill-tok"))}}}},
		Workspace: &ompv1alpha1.WorkspaceSpec{Git: &ompv1alpha1.GitWorkspaceSource{URL: "u", Auth: &ompv1alpha1.GitAuth{
			SSHKeySecretRef: new(ref("ws-key")), KnownHostsSecretRef: new(ref("ws-kh"))}}},
		Cloud: &ompv1alpha1.CloudSpec{Git: []ompv1alpha1.CloudGitHost{cloudHost("github.com")}},
		Notify: []ompv1alpha1.NotifySink{
			{Type: ompv1alpha1.SinkNtfy, Ntfy: &ompv1alpha1.NtfySink{TopicSecretRef: ref("topic")}},
			{Type: ompv1alpha1.SinkWebhook, Webhook: &ompv1alpha1.WebhookSink{URLSecretRef: ref("hook"), HMACSecretRef: new(ref("hmac"))}},
		},
	}
	got := make([]string, 0, 10)
	for _, u := range SecretRefs(&spec) {
		got = append(got, u.Ref.Name)
	}
	want := "gw cred mcp-env mcp-hdr skill-tok ws-key ws-kh tok topic hook hmac"
	if strings.Join(got, " ") != want {
		t.Errorf("SecretRefs = %v, want %s", got, want)
	}
}
