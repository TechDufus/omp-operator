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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

func cloudAgent(mutate func(*v1alpha1.CloudSpec)) *v1alpha1.OmpAgent {
	a := baseAgent()
	a.Spec.Cloud = &v1alpha1.CloudSpec{
		MaxSitting: metav1.Duration{Duration: 2 * time.Hour},
		Git: []v1alpha1.CloudGitHost{{
			Host: "github.com",
			Auth: v1alpha1.GitAuth{TokenSecretRef: &v1alpha1.SecretKeyRef{Name: "gh", Key: "token"}},
			Push: true,
		}},
	}
	if mutate != nil {
		mutate(a.Spec.Cloud)
	}
	return a
}

func baseThread() *v1alpha1.OmpThread {
	return &v1alpha1.OmpThread{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "triage-k3x9fa"},
		Spec: v1alpha1.OmpThreadSpec{
			AgentRef: v1alpha1.LocalObjectRef{Name: "triage"},
			Owner:    "token:dev",
			Repo: v1alpha1.ThreadRepo{
				Name:          "app",
				RemoteURL:     "git@github.com:acme/app.git",
				BaseRef:       "main",
				HandoffCommit: "0123456789012345678901234567890123456789",
			},
			Paths:       v1alpha1.ThreadPaths{Home: "/home/techdufus", Workspace: "/home/techdufus/dev/app"},
			SecretFiles: []string{".env", "config/secrets.yaml"},
			SecretEnv:   []string{"API_KEY"},
		},
	}
}

func threadSession() *v1alpha1.OmpSession {
	s := baseSession()
	s.Name = "triage-k3x9fa-1"
	s.Spec.ThreadRef = &v1alpha1.LocalObjectRef{Name: "triage-k3x9fa"}
	return s
}

func initContainer(t *testing.T, p *corev1.Pod, name string) corev1.Container {
	t.Helper()
	for _, c := range p.Spec.InitContainers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("init container %q not found", name)
	return corev1.Container{}
}

func mountedVolumes(c corev1.Container) []string {
	out := make([]string, 0, len(c.VolumeMounts))
	for _, m := range c.VolumeMounts {
		out = append(out, m.Name)
	}
	return out
}

func TestThreadPod(t *testing.T) { //nolint:gocyclo // table of assertions
	p := SessionPod(cloudAgent(nil), threadSession(), baseThread(), testOpts, 1)

	if got := p.Labels["omp.techdufus.com/thread"]; got != "triage-k3x9fa" {
		t.Errorf("thread label = %q", got)
	}
	if got := *p.Spec.ActiveDeadlineSeconds; got != int64((2*time.Hour+10*time.Minute)/time.Second) {
		t.Errorf("activeDeadlineSeconds = %d, want maxSitting + 10m", got)
	}
	if got := *p.Spec.TerminationGracePeriodSeconds; got != 60 {
		t.Errorf("terminationGracePeriodSeconds = %d", got)
	}

	// Home and workspace are mounted where they are on the user's machine.
	prepare, agent := initContainer(t, p, "prepare"), p.Spec.Containers[0]
	for _, c := range []corev1.Container{prepare, agent} {
		m := mountsOf(c)
		if m["/home/techdufus/dev/app"].Name != "workspace" || m["/home/techdufus"].Name != "home" {
			t.Errorf("%s mounts = %v", c.Name, mountedVolumes(c))
		}
		if _, ok := m["/workspace"]; ok {
			t.Errorf("%s still mounts /workspace", c.Name)
		}
		if c.WorkingDir != "/home/techdufus/dev/app" {
			t.Errorf("%s workingDir = %q", c.Name, c.WorkingDir)
		}
		env := envOf(c)
		for k, want := range map[string]string{
			"HOME":                      "/home/techdufus",
			"OMP_OPERATOR_WORKSPACE":    "/home/techdufus/dev/app",
			"MISE_TRUSTED_CONFIG_PATHS": "/home/techdufus/dev/app",
			"PATH":                      "/opt/mise/data/shims:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		} {
			if env[k].Value != want {
				t.Errorf("%s env %s = %q, want %q", c.Name, k, env[k].Value, want)
			}
		}
	}

	// emptyDir limits.
	for name, want := range map[string]string{"workspace": "20Gi", "home": "10Gi", "mise": "10Gi", "tmp": "5Gi"} {
		if got := volume(t, p, name).EmptyDir.SizeLimit.String(); got != want {
			t.Errorf("volume %s size limit = %s, want %s", name, got, want)
		}
	}

	// The git credential reaches only prepare and gitpush, never the agent container.
	auth := volume(t, p, "auth-cloud-git")
	if src := auth.Projected.Sources; len(src) != 1 || src[0].Secret.Name != "gh" || src[0].Secret.Items[0].Path != "token" {
		t.Errorf("auth volume = %+v", auth.Projected)
	}
	push := initContainer(t, p, "gitpush")
	for name, c := range map[string]corev1.Container{"prepare": prepare, "gitpush": push} {
		if m := mountsOf(c)["/var/run/omp-operator/auth/cloud-git"]; m.Name != "auth-cloud-git" || !m.ReadOnly {
			t.Errorf("%s misses the auth mount: %v", name, mountedVolumes(c))
		}
	}
	for _, v := range []string{"auth-cloud-git", "thread-secrets", "gitpush-tmp"} {
		if slices.Contains(mountedVolumes(agent), v) {
			t.Errorf("agent container mounts %s", v)
		}
	}

	// Shipped files: only prepare mounts them, as file-<i> items of <thread>-secrets.
	sec := volume(t, p, "thread-secrets").Secret
	if sec.SecretName != "triage-k3x9fa-secrets" || len(sec.Items) != 2 ||
		sec.Items[0].Key != "file-0" || sec.Items[1].Key != "file-1" {
		t.Errorf("thread-secrets volume = %+v", sec)
	}
	if m := mountsOf(prepare)["/var/run/omp-operator/thread-secrets"]; m.Name != "thread-secrets" || !m.ReadOnly {
		t.Errorf("prepare misses the thread secrets mount: %v", mountedVolumes(prepare))
	}

	// Shipped variables: only the agent container has them.
	e := envOf(agent)["API_KEY"]
	if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != "triage-k3x9fa-secrets" || e.ValueFrom.SecretKeyRef.Key != "env.API_KEY" {
		t.Errorf("agent API_KEY = %+v", e)
	}
	for _, c := range []corev1.Container{prepare, push} {
		if _, ok := envOf(c)["API_KEY"]; ok {
			t.Errorf("%s has API_KEY", c.Name)
		}
	}
	if envOf(agent)["OMP_GITPUSH_ADDR"].Value != "127.0.0.1:8090" {
		t.Errorf("agent OMP_GITPUSH_ADDR = %q", envOf(agent)["OMP_GITPUSH_ADDR"].Value)
	}

	// The sidecar: native, with the target fixed by its own env.
	if push.RestartPolicy == nil || *push.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("gitpush restartPolicy = %v", push.RestartPolicy)
	}
	if !slices.Equal(push.Command, []string{"omp-operator-driver"}) || !slices.Equal(push.Args, []string{"gitpush"}) {
		t.Errorf("gitpush command = %v %v", push.Command, push.Args)
	}
	penv := envOf(push)
	for k, want := range map[string]string{
		"OMP_GITPUSH_URL":       "https://github.com/acme/app.git",
		"OMP_GITPUSH_BRANCH":    "omp-cloud/triage-k3x9fa",
		"OMP_GITPUSH_WORKSPACE": "/home/techdufus/dev/app",
		"OMP_GITPUSH_AUTH":      "token",
		"OMP_GITPUSH_AUTHDIR":   "/var/run/omp-operator/auth/cloud-git",
		"OMP_GITPUSH_USERNAME":  "x-access-token",
		"HOME":                  "/tmp",
	} {
		if penv[k].Value != want {
			t.Errorf("gitpush env %s = %q, want %q", k, penv[k].Value, want)
		}
	}
	if m := mountsOf(push)["/home/techdufus/dev/app"]; m.Name != "workspace" || !m.ReadOnly {
		t.Errorf("gitpush workspace mount = %+v", m)
	}
	if m := mountsOf(push)["/tmp"]; m.Name != "gitpush-tmp" {
		t.Errorf("gitpush /tmp mount = %+v", m)
	}
	if push.SecurityContext == nil || !*push.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("gitpush security context = %+v", push.SecurityContext)
	}
	// prepare comes before the sidecar.
	if p.Spec.InitContainers[0].Name != "prepare" || p.Spec.InitContainers[1].Name != "gitpush" {
		t.Errorf("init order = %s, %s", p.Spec.InitContainers[0].Name, p.Spec.InitContainers[1].Name)
	}
}

func TestThreadPodWithoutPush(t *testing.T) {
	agent := cloudAgent(func(c *v1alpha1.CloudSpec) { c.Git[0].Push = false })
	p := SessionPod(agent, threadSession(), baseThread(), testOpts, 1)
	if len(p.Spec.InitContainers) != 1 {
		t.Errorf("init containers = %d, want prepare only", len(p.Spec.InitContainers))
	}
	if _, ok := envOf(p.Spec.Containers[0])["OMP_GITPUSH_ADDR"]; ok {
		t.Error("agent has OMP_GITPUSH_ADDR without push")
	}
	for _, v := range p.Spec.Volumes {
		if v.Name == "gitpush-tmp" {
			t.Error("gitpush-tmp volume exists without push")
		}
	}
	// Prepare still fetches with the host's credential.
	if _, ok := mountsOf(p.Spec.InitContainers[0])["/var/run/omp-operator/auth/cloud-git"]; !ok {
		t.Error("prepare misses the auth mount")
	}
}

func TestThreadPodSSHAuth(t *testing.T) {
	agent := cloudAgent(func(c *v1alpha1.CloudSpec) {
		c.Git[0].Auth = v1alpha1.GitAuth{
			SSHKeySecretRef:     &v1alpha1.SecretKeyRef{Name: "key", Key: "id"},
			KnownHostsSecretRef: &v1alpha1.SecretKeyRef{Name: "kh", Key: "hosts"},
		}
	})
	p := SessionPod(agent, threadSession(), baseThread(), testOpts, 1)
	files := make([]string, 0, 2)
	for _, s := range volume(t, p, "auth-cloud-git").Projected.Sources {
		files = append(files, s.Secret.Items[0].Path)
	}
	if !slices.Equal(files, []string{"ssh-privatekey", "known_hosts"}) {
		t.Errorf("auth files = %v", files)
	}
	penv := envOf(initContainer(t, p, "gitpush"))
	if penv["OMP_GITPUSH_AUTH"].Value != "ssh" || penv["OMP_GITPUSH_URL"].Value != "ssh://git@github.com/acme/app.git" {
		t.Errorf("gitpush env = auth %q url %q", penv["OMP_GITPUSH_AUTH"].Value, penv["OMP_GITPUSH_URL"].Value)
	}
}

func TestThreadPodPaths(t *testing.T) {
	tests := []struct {
		name          string
		paths         v1alpha1.ThreadPaths
		home, workdir string
	}{
		{"mirrored", v1alpha1.ThreadPaths{Home: "/Users/me", Workspace: "/Users/me/src/app"}, "/Users/me", "/Users/me/src/app"},
		{"empty uses defaults", v1alpha1.ThreadPaths{}, "/home/agent", "/workspace"},
		{"invalid path falls back for both", v1alpha1.ThreadPaths{Home: "/home/me", Workspace: "/etc/app"}, "/home/agent", "/workspace"},
		{"workspace above home falls back for both", v1alpha1.ThreadPaths{Home: "/home/me/x", Workspace: "/home/me"}, "/home/agent", "/workspace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			th := baseThread()
			th.Spec.Paths = tt.paths
			p := SessionPod(cloudAgent(nil), threadSession(), th, testOpts, 1)
			env := envOf(p.Spec.Containers[0])
			if env["HOME"].Value != tt.home || env["OMP_OPERATOR_WORKSPACE"].Value != tt.workdir {
				t.Errorf("HOME = %q workspace = %q", env["HOME"].Value, env["OMP_OPERATOR_WORKSPACE"].Value)
			}
		})
	}
}

func TestThreadPodNoMatchingHost(t *testing.T) {
	th := baseThread()
	th.Spec.Repo.RemoteURL = "git@gitlab.com:acme/app.git"
	th.Spec.SecretFiles, th.Spec.SecretEnv = nil, nil
	p := SessionPod(cloudAgent(nil), threadSession(), th, testOpts, 1)
	if len(p.Spec.InitContainers) != 1 {
		t.Errorf("init containers = %d, want prepare only", len(p.Spec.InitContainers))
	}
	for _, v := range p.Spec.Volumes {
		if v.Name == "auth-cloud-git" || v.Name == "thread-secrets" {
			t.Errorf("unexpected volume %s", v.Name)
		}
	}
}

func TestThreadPodDropsReservedSecretEnv(t *testing.T) {
	th := baseThread()
	th.Spec.SecretEnv = []string{"HOME", "OMP_GATEWAY_TOKEN", "KUBECONFIG", "OK_NAME", "9bad"}
	p := SessionPod(cloudAgent(nil), threadSession(), th, testOpts, 1)
	env := envOf(p.Spec.Containers[0])
	if env["HOME"].ValueFrom != nil || env["OMP_GATEWAY_TOKEN"].ValueFrom != nil || env["KUBECONFIG"].ValueFrom != nil {
		t.Errorf("a reserved variable came from the thread Secret")
	}
	if _, ok := env["9bad"]; ok {
		t.Error("an invalid variable name was passed through")
	}
	if env["OK_NAME"].ValueFrom == nil {
		t.Error("OK_NAME missing")
	}
}
