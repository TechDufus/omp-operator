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

package cloudspec

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

func TestKeysAndBranch(t *testing.T) {
	if got := ContinuityKey("dev", "coder-k3x9fa"); got != "thread/dev/coder-k3x9fa" {
		t.Errorf("ContinuityKey = %q", got)
	}
	if got := Branch("coder-k3x9fa"); got != "omp-cloud/coder-k3x9fa" {
		t.Errorf("Branch = %q", got)
	}
}

func TestValidPodPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/home/alice", true},
		{"/home/alice/dev/my-repo.git", true},
		{"/Users/al@x/work+1/_x", true},
		{"/srv/www", true},
		{"/optional", true}, // near a system dir, not under it
		{"/variable/x", true},
		{"", false},
		{"/", false},
		{"relative/path", false},
		{"home/alice", false},
		{"/home/alice/", false},
		{"/home//alice", false},
		{"/home/./alice", false},
		{"/home/alice/../bob", false},
		{"/home/al ice", false},
		{"/home/al;ice", false},
		{"/home/ünï", false},
		{"/home/a\nb", false},
		{"/" + strings.Repeat("a", 511), true},
		{"/" + strings.Repeat("a", 512), false},
		{"/bin", false},
		{"/boot/x", false},
		{"/dev", false},
		{"/etc", false},
		{"/etc/ssh", false},
		{"/lib64/x", false},
		{"/opt", false},
		{"/opt/mise/data", false},
		{"/proc/1", false},
		{"/run/user/1000", false},
		{"/sbin", false},
		{"/sys", false},
		{"/tmp", false},
		{"/tmp/work", false},
		{"/usr/local", false},
		{"/var/lib/x", false},
	}
	for _, tt := range tests {
		if got := ValidPodPath(tt.path); got != tt.want {
			t.Errorf("ValidPodPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestPodPaths(t *testing.T) {
	tests := []struct {
		name     string
		in       v1alpha1.ThreadPaths
		wantHome string
		wantWS   string
	}{
		{"mirrored", v1alpha1.ThreadPaths{Home: "/home/alice", Workspace: "/home/alice/dev/app"}, "/home/alice", "/home/alice/dev/app"},
		{"separate trees", v1alpha1.ThreadPaths{Home: "/home/alice", Workspace: "/srv/app"}, "/home/alice", "/srv/app"},
		{"empty", v1alpha1.ThreadPaths{}, DefaultHome, DefaultWorkspace},
		{"only home", v1alpha1.ThreadPaths{Home: "/home/alice"}, DefaultHome, DefaultWorkspace},
		{"only workspace", v1alpha1.ThreadPaths{Workspace: "/srv/app"}, DefaultHome, DefaultWorkspace},
		{"invalid home", v1alpha1.ThreadPaths{Home: "/etc/alice", Workspace: "/srv/app"}, DefaultHome, DefaultWorkspace},
		{"invalid workspace", v1alpha1.ThreadPaths{Home: "/home/alice", Workspace: "app"}, DefaultHome, DefaultWorkspace},
		{"same path", v1alpha1.ThreadPaths{Home: "/home/alice", Workspace: "/home/alice"}, DefaultHome, DefaultWorkspace},
		{"workspace parent of home", v1alpha1.ThreadPaths{Home: "/srv/app/home", Workspace: "/srv/app"}, DefaultHome, DefaultWorkspace},
		{"similar prefix is not a parent", v1alpha1.ThreadPaths{Home: "/srv/app2", Workspace: "/srv/app"}, "/srv/app2", "/srv/app"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, ws := PodPaths(tt.in)
			if home != tt.wantHome || ws != tt.wantWS {
				t.Errorf("PodPaths(%+v) = %q, %q; want %q, %q", tt.in, home, ws, tt.wantHome, tt.wantWS)
			}
		})
	}
}

func TestMatchGitHost(t *testing.T) {
	ref := &v1alpha1.SecretKeyRef{Name: "s", Key: "k"}
	hosts := []v1alpha1.CloudGitHost{
		{Host: "github.com", Auth: v1alpha1.GitAuth{TokenSecretRef: ref}},
		{Host: "git.example.com", Auth: v1alpha1.GitAuth{SSHKeySecretRef: ref, KnownHostsSecretRef: ref}},
		{Host: "gitserver.e2e.svc.cluster.local", Auth: v1alpha1.GitAuth{TokenSecretRef: ref}},
	}
	tests := []struct {
		name      string
		raw       string
		wantHost  string // "" = no match
		wantFetch string
	}{
		// Token auth.
		{"https token", "https://github.com/TechDufus/omp-operator.git", "github.com", "https://github.com/TechDufus/omp-operator.git"},
		{"https userinfo dropped", "https://x-access-token:secret@github.com/o/r.git", "github.com", "https://github.com/o/r.git"},
		{"scp to https", "git@github.com:TechDufus/omp-operator.git", "github.com", "https://github.com/TechDufus/omp-operator.git"},
		{"scp absolute path", "git@github.com:/o/r.git", "github.com", "https://github.com/o/r.git"},
		{"ssh url to https, port dropped", "ssh://git@github.com:22/o/r.git", "github.com", "https://github.com/o/r.git"},
		{"host case", "https://GitHub.COM/o/r", "github.com", "https://github.com/o/r"},
		{"http keeps scheme and port", "http://gitserver.e2e.svc.cluster.local:8080/app.git", "gitserver.e2e.svc.cluster.local", "http://gitserver.e2e.svc.cluster.local:8080/app.git"},
		{"https keeps port", "https://github.com:8443/o/r", "github.com", "https://github.com:8443/o/r"},
		// SSH auth.
		{"scp ssh", "git@git.example.com:team/repo.git", "git.example.com", "ssh://git@git.example.com/team/repo.git"},
		{"scp without user", "git.example.com:team/repo.git", "git.example.com", "ssh://git@git.example.com/team/repo.git"},
		{"ssh url other user and port", "ssh://alice@git.example.com:2222/team/repo.git", "git.example.com", "ssh://git@git.example.com:2222/team/repo.git"},
		{"https to ssh, port dropped", "https://git.example.com:8443/team/repo.git", "git.example.com", "ssh://git@git.example.com/team/repo.git"},
		// No match.
		{"unknown https is passed through", "https://gitlab.com/o/r.git", "", "https://gitlab.com/o/r.git"},
		{"unknown http is passed through", "http://localhost:3000/o/r.git", "", "http://localhost:3000/o/r.git"},
		{"unknown scp", "git@gitlab.com:o/r.git", "", ""},
		{"unknown ssh url", "ssh://git@gitlab.com/o/r.git", "", ""},
		{"host suffix is not a match", "https://evilgithub.com/o/r", "", "https://evilgithub.com/o/r"},
		{"host prefix is not a match", "https://github.com.evil.io/o/r", "", "https://github.com.evil.io/o/r"},
		{"userinfo host confusion", "https://github.com@evil.io/o/r", "", "https://github.com@evil.io/o/r"},
		{"empty", "", "", ""},
		{"local path", "/srv/git/repo.git", "", ""},
		{"file url", "file:///srv/git/repo.git", "", ""},
		{"garbage", "://", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, fetch, ok := MatchGitHost(hosts, tt.raw)
			if ok != (tt.wantHost != "") {
				t.Fatalf("ok = %v, want %v (host %v)", ok, tt.wantHost != "", host)
			}
			if ok && host.Host != tt.wantHost {
				t.Errorf("host = %q, want %q", host.Host, tt.wantHost)
			}
			if !ok && host != nil {
				t.Errorf("host = %+v without a match", host)
			}
			if fetch != tt.wantFetch {
				t.Errorf("fetchURL = %q, want %q", fetch, tt.wantFetch)
			}
		})
	}
	if _, fetch, ok := MatchGitHost(nil, "https://github.com/o/r"); ok || fetch != "https://github.com/o/r" {
		t.Errorf("no hosts: ok=%v fetch=%q", ok, fetch)
	}
}

func TestCloudDefaults(t *testing.T) {
	var c v1alpha1.CloudSpec
	if IdleTimeout(&c) != 10*time.Minute || MaxSitting(&c) != 4*time.Hour || ThreadRetention(&c) != 168*time.Hour ||
		QuestionTimeout(&c) != 30*time.Minute {
		t.Errorf("defaults = %s %s %s %s", IdleTimeout(&c), MaxSitting(&c), ThreadRetention(&c), QuestionTimeout(&c))
	}
	c = v1alpha1.CloudSpec{
		IdleTimeout:     metav1.Duration{Duration: time.Minute},
		MaxSitting:      metav1.Duration{Duration: time.Hour},
		ThreadRetention: metav1.Duration{Duration: 2 * time.Hour},
		QuestionTimeout: metav1.Duration{Duration: 5 * time.Minute},
	}
	if IdleTimeout(&c) != time.Minute || MaxSitting(&c) != time.Hour || ThreadRetention(&c) != 2*time.Hour ||
		QuestionTimeout(&c) != 5*time.Minute {
		t.Errorf("set values = %s %s %s %s", IdleTimeout(&c), MaxSitting(&c), ThreadRetention(&c), QuestionTimeout(&c))
	}
}
