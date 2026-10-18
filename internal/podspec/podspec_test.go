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

//nolint:goconst // table-driven fixtures repeat literals on purpose
package podspec

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/config"
)

var testOpts = &config.Options{
	OperatorNamespace:   "omp-system",
	ServiceName:         "omp-operator",
	AgentAPIPort:        8083,
	DefaultRuntimeImage: "example.com/runtime:default",
}

func baseAgent() *v1alpha1.OmpAgent {
	return &v1alpha1.OmpAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "triage"},
		Spec:       v1alpha1.OmpAgentSpec{Model: v1alpha1.ModelSpec{Default: "anthropic/claude"}},
	}
}

func baseSession() *v1alpha1.OmpSession {
	return &v1alpha1.OmpSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "sess"},
		Spec:       v1alpha1.OmpSessionSpec{AgentRef: v1alpha1.LocalObjectRef{Name: "triage"}, Prompt: "go"},
	}
}

func envOf(c corev1.Container) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

func mountsOf(c corev1.Container) map[string]corev1.VolumeMount {
	m := map[string]corev1.VolumeMount{}
	for _, v := range c.VolumeMounts {
		m[v.MountPath] = v
	}
	return m
}

func volume(t *testing.T, p *corev1.Pod, name string) corev1.Volume {
	t.Helper()
	for _, v := range p.Spec.Volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("volume %q not found", name)
	return corev1.Volume{}
}

func TestSessionPod(t *testing.T) { //nolint:gocyclo // table of assertions
	tests := []struct {
		name  string
		agent func() *v1alpha1.OmpAgent
		check func(t *testing.T, p *corev1.Pod)
	}{
		{
			name:  "minimal agent: default SA without token, hardening, shared env and mounts",
			agent: baseAgent,
			check: func(t *testing.T, p *corev1.Pod) {
				if p.Name != "sess-1" || p.Namespace != "ns" {
					t.Errorf("name = %s/%s", p.Namespace, p.Name)
				}
				if p.Spec.ServiceAccountName != "default" || *p.Spec.AutomountServiceAccountToken {
					t.Errorf("SA = %q automount = %v", p.Spec.ServiceAccountName, *p.Spec.AutomountServiceAccountToken)
				}
				if p.Spec.RestartPolicy != corev1.RestartPolicyNever || *p.Spec.EnableServiceLinks {
					t.Errorf("restartPolicy/serviceLinks wrong")
				}
				if got := *p.Spec.ActiveDeadlineSeconds; got != int64((30*time.Minute+10*time.Minute)/time.Second) {
					t.Errorf("activeDeadlineSeconds = %d", got)
				}
				wantLabels := map[string]string{
					"app.kubernetes.io/managed-by": "omp-operator",
					"omp.techdufus.com/agent":      "triage",
					"omp.techdufus.com/session":    "sess",
					"omp.techdufus.com/role":       "session",
				}
				if len(p.Labels) != len(wantLabels) {
					t.Errorf("labels = %v", p.Labels)
				}
				for k, v := range wantLabels {
					if p.Labels[k] != v {
						t.Errorf("label %s = %q, want %q", k, p.Labels[k], v)
					}
				}
				sc := p.Spec.SecurityContext
				if !*sc.RunAsNonRoot || *sc.RunAsUser != 10001 || *sc.RunAsGroup != 10001 || *sc.FSGroup != 10001 ||
					sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
					t.Errorf("pod security context = %+v", sc)
				}
				if len(p.Spec.InitContainers) != 1 || len(p.Spec.Containers) != 1 {
					t.Fatalf("containers = %d init, %d main", len(p.Spec.InitContainers), len(p.Spec.Containers))
				}
				initC, mainC := p.Spec.InitContainers[0], p.Spec.Containers[0]
				if initC.Name != "prepare" || !slices.Equal(initC.Args, []string{"prepare"}) {
					t.Errorf("init = %s %v", initC.Name, initC.Args)
				}
				if mainC.Name != "agent" || !slices.Equal(mainC.Args, []string{"run"}) {
					t.Errorf("main = %s %v", mainC.Name, mainC.Args)
				}
				for _, c := range []corev1.Container{initC, mainC} {
					if c.Image != "example.com/runtime:default" || !slices.Equal(c.Command, []string{"omp-operator-driver"}) {
						t.Errorf("%s image/command = %s %v", c.Name, c.Image, c.Command)
					}
					cs := c.SecurityContext
					if *cs.AllowPrivilegeEscalation || !*cs.ReadOnlyRootFilesystem || !slices.Equal(cs.Capabilities.Drop, []corev1.Capability{"ALL"}) {
						t.Errorf("%s security context = %+v", c.Name, cs)
					}
					env := envOf(c)
					for k, want := range map[string]string{
						"OMP_OPERATOR_SESSION":        "ns/sess",
						"OMP_OPERATOR_API":            "http://omp-operator.omp-system.svc:8083",
						"OMP_OPERATOR_WORKSPACE":      "/workspace",
						"HOME":                        "/home/agent",
						"MISE_DATA_DIR":               "/opt/mise/data",
						"MISE_CACHE_DIR":              "/tmp/mise-cache",
						"MISE_STATE_DIR":              "/tmp/mise-state",
						"MISE_TRUSTED_CONFIG_PATHS":   "/workspace",
						"MISE_DISABLE_TOOLS":          "",
						"MISE_AUTO_INSTALL":           "false",
						"MISE_EXEC_AUTO_INSTALL":      "false",
						"MISE_NOT_FOUND_AUTO_INSTALL": "false",
						"MISE_YES":                    "1",
					} {
						e, ok := env[k]
						if !ok || e.Value != want {
							t.Errorf("%s env %s = %+v (present %v), want %q", c.Name, k, e, ok, want)
						}
					}
					if _, ok := env["OMP_OPERATOR_AGENT"]; ok {
						t.Errorf("%s has OMP_OPERATOR_AGENT", c.Name)
					}
					m := mountsOf(c)
					for _, dir := range []string{"/workspace", "/opt/mise", "/home/agent", "/tmp", agentapi.RunDir, "/var/run/omp-operator/token"} {
						if _, ok := m[dir]; !ok {
							t.Errorf("%s misses mount %s", c.Name, dir)
						}
					}
				}
				// Token Secret volume exposes the file at agentapi.TokenFile.
				tok := volume(t, p, "token").Secret
				if tok.SecretName != "sess-token" || tok.Items[0].Key != "token" || "/var/run/omp-operator/token/"+tok.Items[0].Path != agentapi.TokenFile {
					t.Errorf("token volume = %+v", tok)
				}
				// Sizes of the emptyDirs.
				for name, size := range map[string]string{"workspace": "2Gi", "mise": "2Gi", "home": "1Gi", "tmp": "1Gi"} {
					q := volume(t, p, name).EmptyDir.SizeLimit
					if q == nil || q.Cmp(resource.MustParse(size)) != 0 {
						t.Errorf("volume %s size = %v, want %s", name, q, size)
					}
				}
				if volume(t, p, "run").EmptyDir == nil {
					t.Error("run volume is not an emptyDir")
				}
				env := envOf(mainC)
				for _, k := range []string{"KUBECONFIG", "OMP_GATEWAY_TOKEN"} {
					if _, ok := env[k]; ok {
						t.Errorf("main has %s without SA/gateway", k)
					}
				}
			},
		},
		{
			name: "agent image and service account",
			agent: func() *v1alpha1.OmpAgent {
				a := baseAgent()
				a.Spec.Image = "example.com/custom:1"
				a.Spec.ServiceAccountName = "triage-sa"
				a.Spec.Limits.SessionTimeout = metav1.Duration{Duration: time.Hour}
				return a
			},
			check: func(t *testing.T, p *corev1.Pod) {
				if p.Spec.ServiceAccountName != "triage-sa" || !*p.Spec.AutomountServiceAccountToken {
					t.Errorf("SA = %q automount = %v", p.Spec.ServiceAccountName, *p.Spec.AutomountServiceAccountToken)
				}
				if *p.Spec.ActiveDeadlineSeconds != 4200 {
					t.Errorf("activeDeadlineSeconds = %d, want 4200", *p.Spec.ActiveDeadlineSeconds)
				}
				if p.Spec.Containers[0].Image != "example.com/custom:1" || p.Spec.InitContainers[0].Image != "example.com/custom:1" {
					t.Errorf("images = %s / %s", p.Spec.InitContainers[0].Image, p.Spec.Containers[0].Image)
				}
				if e := envOf(p.Spec.Containers[0])["KUBECONFIG"]; e.Value != agentapi.KubeconfigFile {
					t.Errorf("main KUBECONFIG = %q", e.Value)
				}
				if _, ok := envOf(p.Spec.InitContainers[0])["KUBECONFIG"]; ok {
					t.Error("init container has KUBECONFIG")
				}
			},
		},
		{
			name: "credentials: env as secretKeyRef, files grouped by directory in main only",
			agent: func() *v1alpha1.OmpAgent {
				a := baseAgent()
				a.Spec.Credentials = []v1alpha1.Credential{
					{Name: "tok", Description: "d", SecretRef: v1alpha1.SecretKeyRef{Name: "s1", Key: "k1"}, Env: "DEMO_TOKEN"},
					{Name: "cert", Description: "d", SecretRef: v1alpha1.SecretKeyRef{Name: "s2", Key: "crt"}, File: "/etc/creds/tls.crt"},
					{Name: "other", Description: "d", SecretRef: v1alpha1.SecretKeyRef{Name: "s3", Key: "x"}, File: "/etc/other/x.txt"},
					{Name: "key", Description: "d", SecretRef: v1alpha1.SecretKeyRef{Name: "s2", Key: "key"}, File: "/etc/creds/tls.key"},
				}
				return a
			},
			check: func(t *testing.T, p *corev1.Pod) {
				mainC, initC := p.Spec.Containers[0], p.Spec.InitContainers[0]
				e := envOf(mainC)["DEMO_TOKEN"]
				if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != "s1" || e.ValueFrom.SecretKeyRef.Key != "k1" {
					t.Errorf("DEMO_TOKEN = %+v", e)
				}
				if _, ok := envOf(initC)["DEMO_TOKEN"]; ok {
					t.Error("init container has credential env")
				}
				m := mountsOf(mainC)
				creds, other := m["/etc/creds"], m["/etc/other"]
				if creds.Name == "" || other.Name == "" || creds.Name == other.Name || !creds.ReadOnly {
					t.Fatalf("credential mounts = %+v / %+v", creds, other)
				}
				for _, dir := range []string{"/etc/creds", "/etc/other"} {
					if _, ok := mountsOf(initC)[dir]; ok {
						t.Errorf("init container mounts %s", dir)
					}
				}
				srcs := volume(t, p, creds.Name).Projected.Sources
				if len(srcs) != 2 {
					t.Fatalf("/etc/creds sources = %d", len(srcs))
				}
				if s := srcs[0].Secret; s.Name != "s2" || s.Items[0].Key != "crt" || s.Items[0].Path != "tls.crt" {
					t.Errorf("source 0 = %+v", s)
				}
				if s := srcs[1].Secret; s.Name != "s2" || s.Items[0].Key != "key" || s.Items[0].Path != "tls.key" {
					t.Errorf("source 1 = %+v", s)
				}
				if s := volume(t, p, other.Name).Projected.Sources; len(s) != 1 || s[0].Secret.Name != "s3" || s[0].Secret.Items[0].Path != "x.txt" {
					t.Errorf("/etc/other sources = %+v", s)
				}
			},
		},
		{
			name: "MCP secrets become OMP_MCP_n_m in main: env for stdio, headers for http",
			agent: func() *v1alpha1.OmpAgent {
				a := baseAgent()
				lit := "plain"
				a.Spec.MCPServers = []v1alpha1.MCPServer{
					{Name: "a", Command: []string{"npx", "srv"}, Env: []v1alpha1.ValueOrSecret{
						{Name: "L", Value: &lit},
						{Name: "S", SecretRef: &v1alpha1.SecretKeyRef{Name: "mcp", Key: "e1"}},
					}},
					{Name: "b", URL: "https://x", Headers: []v1alpha1.ValueOrSecret{
						{Name: "Authorization", SecretRef: &v1alpha1.SecretKeyRef{Name: "mcp", Key: "h0"}},
					}},
				}
				return a
			},
			check: func(t *testing.T, p *corev1.Pod) {
				env := envOf(p.Spec.Containers[0])
				if e := env["OMP_MCP_0_1"]; e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Key != "e1" {
					t.Errorf("OMP_MCP_0_1 = %+v", e)
				}
				if e := env["OMP_MCP_1_0"]; e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Key != "h0" {
					t.Errorf("OMP_MCP_1_0 = %+v", e)
				}
				if _, ok := env["OMP_MCP_0_0"]; ok {
					t.Error("literal MCP env must not be an env var")
				}
				for k := range envOf(p.Spec.InitContainers[0]) {
					if strings.HasPrefix(k, "OMP_MCP_") {
						t.Errorf("init container has %s", k)
					}
				}
			},
		},
		{
			name: "managed gateway token in main only",
			agent: func() *v1alpha1.OmpAgent {
				a := baseAgent()
				a.Spec.Model.Gateway = &v1alpha1.GatewaySpec{URL: "http://gw"}
				return a
			},
			check: func(t *testing.T, p *corev1.Pod) {
				e := envOf(p.Spec.Containers[0])["OMP_GATEWAY_TOKEN"]
				if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != "triage-omp-gateway" || e.ValueFrom.SecretKeyRef.Key != "token" {
					t.Errorf("OMP_GATEWAY_TOKEN = %+v", e)
				}
				if _, ok := envOf(p.Spec.InitContainers[0])["OMP_GATEWAY_TOKEN"]; ok {
					t.Error("init container has OMP_GATEWAY_TOKEN")
				}
			},
		},
		{
			name: "user gateway token Secret",
			agent: func() *v1alpha1.OmpAgent {
				a := baseAgent()
				a.Spec.Model.Gateway = &v1alpha1.GatewaySpec{URL: "http://gw", TokenSecretRef: &v1alpha1.SecretKeyRef{Name: "mine", Key: "t"}}
				return a
			},
			check: func(t *testing.T, p *corev1.Pod) {
				e := envOf(p.Spec.Containers[0])["OMP_GATEWAY_TOKEN"]
				if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != "mine" || e.ValueFrom.SecretKeyRef.Key != "t" {
					t.Errorf("OMP_GATEWAY_TOKEN = %+v", e)
				}
			},
		},
		{
			name: "git auth and ConfigMap skills are mounted in init only",
			agent: func() *v1alpha1.OmpAgent {
				a := baseAgent()
				a.Spec.Workspace = &v1alpha1.WorkspaceSpec{Git: &v1alpha1.GitWorkspaceSource{URL: "git@h:r.git", Auth: &v1alpha1.GitAuth{
					SSHKeySecretRef:     &v1alpha1.SecretKeyRef{Name: "ssh", Key: "id"},
					KnownHostsSecretRef: &v1alpha1.SecretKeyRef{Name: "kh", Key: "hosts"},
				}}}
				a.Spec.Skills = []v1alpha1.SkillSource{
					{ConfigMap: &v1alpha1.LocalObjectRef{Name: "skills-cm"}},
					{Git: &v1alpha1.GitSkillSource{URL: "https://h/r.git", Auth: &v1alpha1.GitAuth{TokenSecretRef: &v1alpha1.SecretKeyRef{Name: "tk", Key: "pat"}}}},
					{Git: &v1alpha1.GitSkillSource{URL: "https://h/public.git"}},
				}
				return a
			},
			check: func(t *testing.T, p *corev1.Pod) {
				initM, mainM := mountsOf(p.Spec.InitContainers[0]), mountsOf(p.Spec.Containers[0])
				ws := initM[agentapi.AuthDir("workspace")]
				if ws.Name == "" {
					t.Fatal("workspace auth not mounted in init")
				}
				srcs := volume(t, p, ws.Name).Projected.Sources
				if len(srcs) != 2 ||
					srcs[0].Secret.Name != "ssh" || srcs[0].Secret.Items[0].Key != "id" || srcs[0].Secret.Items[0].Path != agentapi.AuthSSHKeyFile ||
					srcs[1].Secret.Name != "kh" || srcs[1].Secret.Items[0].Key != "hosts" || srcs[1].Secret.Items[0].Path != agentapi.AuthKnownHostsFile {
					t.Errorf("workspace auth sources = %+v", srcs)
				}
				sk := initM[agentapi.AuthDir(agentapi.SkillIndexID(1))]
				if sk.Name == "" {
					t.Fatal("skill 1 auth not mounted in init")
				}
				if s := volume(t, p, sk.Name).Projected.Sources; len(s) != 1 || s[0].Secret.Name != "tk" || s[0].Secret.Items[0].Path != agentapi.AuthTokenFile {
					t.Errorf("skill 1 auth sources = %+v", s)
				}
				if _, ok := initM[agentapi.AuthDir(agentapi.SkillIndexID(2))]; ok {
					t.Error("skill 2 has no auth but is mounted")
				}
				cm := initM[agentapi.ConfigMapSkillDir(0)]
				if cm.Name == "" || volume(t, p, cm.Name).ConfigMap.Name != "skills-cm" {
					t.Errorf("ConfigMap skill mount = %+v", cm)
				}
				for _, dir := range []string{agentapi.AuthDir("workspace"), agentapi.AuthDir("1"), agentapi.ConfigMapSkillDir(0)} {
					if _, ok := mainM[dir]; ok {
						t.Errorf("main container mounts %s", dir)
					}
				}
			},
		},
		{
			name: "toolchain disable list and pod overrides",
			agent: func() *v1alpha1.OmpAgent {
				a := baseAgent()
				a.Spec.Toolchain = &v1alpha1.ToolchainSpec{Disable: []string{"gitleaks", "yamllint"}}
				a.Spec.Pod = v1alpha1.PodOverrides{
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
					Env: []corev1.EnvVar{
						{Name: "FAKEOMP_SCRIPT", Value: "{}"},
						{Name: "HOME", Value: "/evil"},
					},
					Volumes:      []corev1.Volume{{Name: "extra", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
					VolumeMounts: []corev1.VolumeMount{{Name: "extra", MountPath: "/extra"}},
					NodeSelector: map[string]string{"disk": "ssd"},
					Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}},
					Labels:       map[string]string{"team": "sre", "omp.techdufus.com/role": "hijack"},
				}
				return a
			},
			check: func(t *testing.T, p *corev1.Pod) {
				mainC := p.Spec.Containers[0]
				env := envOf(mainC)
				if env["MISE_DISABLE_TOOLS"].Value != "gitleaks,yamllint" {
					t.Errorf("MISE_DISABLE_TOOLS = %q", env["MISE_DISABLE_TOOLS"].Value)
				}
				if env["FAKEOMP_SCRIPT"].Value != "{}" {
					t.Errorf("override env missing: %+v", env["FAKEOMP_SCRIPT"])
				}
				// Operator variables win on clash: the last HOME entry is the operator's.
				var home string
				for _, e := range mainC.Env {
					if e.Name == "HOME" {
						home = e.Value
					}
				}
				if home != "/home/agent" {
					t.Errorf("effective HOME = %q", home)
				}
				if _, ok := envOf(p.Spec.InitContainers[0])["FAKEOMP_SCRIPT"]; ok {
					t.Error("init container has override env")
				}
				if mem := mainC.Resources.Limits.Memory(); mem.Cmp(resource.MustParse("1Gi")) != 0 {
					t.Errorf("memory limit = %v", mem)
				}
				if !p.Spec.InitContainers[0].Resources.Limits.Memory().IsZero() {
					t.Error("init container got resources")
				}
				if m := mountsOf(mainC)["/extra"]; m.Name != "extra" {
					t.Errorf("extra mount = %+v", m)
				}
				if _, ok := mountsOf(p.Spec.InitContainers[0])["/extra"]; ok {
					t.Error("init container has extra mount")
				}
				_ = volume(t, p, "extra")
				if p.Spec.NodeSelector["disk"] != "ssd" || len(p.Spec.Tolerations) != 1 {
					t.Errorf("scheduling = %v %v", p.Spec.NodeSelector, p.Spec.Tolerations)
				}
				if p.Labels["team"] != "sre" || p.Labels["omp.techdufus.com/role"] != "session" {
					t.Errorf("labels = %v", p.Labels)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := *testOpts
			opts.GatewayClientsSecret = "clients"
			tt.check(t, SessionPod(tt.agent(), baseSession(), &opts, 1))
		})
	}
}

func TestSessionPodNameAndLongLabel(t *testing.T) {
	s := baseSession()
	s.Name = strings.Repeat("a", 60) + "-bbbbbbbbb"
	p := SessionPod(baseAgent(), s, testOpts, 3)
	if p.Name != s.Name+"-3" {
		t.Errorf("name = %s", p.Name)
	}
	if got := p.Labels["omp.techdufus.com/session"]; len(got) > 63 || !strings.HasPrefix(s.Name, got) {
		t.Errorf("session label = %q", got)
	}
}

func TestToolsyncPod(t *testing.T) {
	a := baseAgent()
	a.Spec.ServiceAccountName = "triage-sa"
	a.Spec.Toolchain = &v1alpha1.ToolchainSpec{Disable: []string{"gitleaks"}}
	a.Spec.Credentials = []v1alpha1.Credential{{Name: "tok", Description: "d", SecretRef: v1alpha1.SecretKeyRef{Name: "s", Key: "k"}, Env: "TOK"}}
	a.Spec.Model.Gateway = &v1alpha1.GatewaySpec{URL: "http://gw"}
	a.Spec.Pod.Env = []corev1.EnvVar{{Name: "EXTRA", Value: "1"}}
	a.Spec.Pod.Labels = map[string]string{"team": "sre"}
	a.Spec.Workspace = &v1alpha1.WorkspaceSpec{Git: &v1alpha1.GitWorkspaceSource{URL: "https://h/r.git", Auth: &v1alpha1.GitAuth{
		TokenSecretRef: &v1alpha1.SecretKeyRef{Name: "tk", Key: "pat"},
	}}}
	a.Spec.Skills = []v1alpha1.SkillSource{{ConfigMap: &v1alpha1.LocalObjectRef{Name: "cm"}}}

	at := time.Unix(1_800_000_123, 0)
	p := ToolsyncPod(a, testOpts, at)
	if p.Name != "triage-toolsync-1800000123" {
		t.Errorf("name = %s", p.Name)
	}
	if *p.Spec.ActiveDeadlineSeconds != 900 {
		t.Errorf("deadline = %d", *p.Spec.ActiveDeadlineSeconds)
	}
	if *p.Spec.AutomountServiceAccountToken {
		t.Error("toolsync pod automounts the SA token")
	}
	if p.Labels["omp.techdufus.com/role"] != "toolsync" || p.Labels["omp.techdufus.com/agent"] != "triage" ||
		p.Labels["app.kubernetes.io/managed-by"] != "omp-operator" || p.Labels["team"] != "" {
		t.Errorf("labels = %v", p.Labels)
	}
	if _, ok := p.Labels["omp.techdufus.com/session"]; ok {
		t.Error("toolsync pod has a session label")
	}
	initC, mainC := p.Spec.InitContainers[0], p.Spec.Containers[0]
	if !slices.Equal(initC.Args, []string{"prepare"}) || !slices.Equal(mainC.Args, []string{"toolsync"}) {
		t.Errorf("args = %v / %v", initC.Args, mainC.Args)
	}
	for _, c := range []corev1.Container{initC, mainC} {
		env := envOf(c)
		if env["OMP_OPERATOR_AGENT"].Value != "ns/triage" {
			t.Errorf("%s OMP_OPERATOR_AGENT = %q", c.Name, env["OMP_OPERATOR_AGENT"].Value)
		}
		if _, ok := env["OMP_OPERATOR_SESSION"]; ok {
			t.Errorf("%s has OMP_OPERATOR_SESSION", c.Name)
		}
		for _, k := range []string{"TOK", "OMP_GATEWAY_TOKEN", "KUBECONFIG", "EXTRA"} {
			if _, ok := env[k]; ok {
				t.Errorf("%s has session-only env %s", c.Name, k)
			}
		}
		if env["MISE_DISABLE_TOOLS"].Value != "gitleaks" {
			t.Errorf("%s MISE_DISABLE_TOOLS = %q", c.Name, env["MISE_DISABLE_TOOLS"].Value)
		}
	}
	if tok := volume(t, p, "token").Secret.SecretName; tok != "triage-toolsync-token" {
		t.Errorf("token secret = %s", tok)
	}
	if _, ok := mountsOf(initC)[agentapi.AuthDir("workspace")]; !ok {
		t.Error("workspace auth not mounted in toolsync init")
	}
	if _, ok := mountsOf(initC)[agentapi.ConfigMapSkillDir(0)]; ok {
		t.Error("skills mounted in toolsync init")
	}
}

func TestTokenSecret(t *testing.T) {
	a, err := TokenSecret("ns", "sess-token", "triage")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := TokenSecret("ns", "sess-token", "triage")
	tok := string(a.Data["token"])
	if len(tok) != 64 || tok == string(b.Data["token"]) {
		t.Errorf("token = %q (len %d); two tokens equal = %v", tok, len(tok), tok == string(b.Data["token"]))
	}
	if a.Labels["omp.techdufus.com/agent"] != "triage" || a.Name != "sess-token" || a.Namespace != "ns" {
		t.Errorf("secret meta = %+v", a.ObjectMeta)
	}
}

func TestLimitDefaults(t *testing.T) {
	a := baseAgent()
	if SessionTimeout(a) != 30*time.Minute || MaxAttempts(a) != 4 || RetryDelay(a) != 15*time.Minute || SessionRetention(a) != 720*time.Hour {
		t.Error("defaults wrong")
	}
	a.Spec.Limits = v1alpha1.LimitsSpec{SessionTimeout: metav1.Duration{Duration: time.Minute}, MaxAttempts: 2, RetryDelay: metav1.Duration{Duration: time.Second}}
	a.Spec.SessionRetention = metav1.Duration{Duration: time.Hour}
	if SessionTimeout(a) != time.Minute || MaxAttempts(a) != 2 || RetryDelay(a) != time.Second || SessionRetention(a) != time.Hour {
		t.Error("explicit values ignored")
	}
}
