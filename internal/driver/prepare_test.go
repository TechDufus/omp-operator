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

package driver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// runGit runs git in dir and returns trimmed stdout.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitFixture is a bare repository with two branches and a skills directory; it returns its file:// URL
// and the commit ids of main and feature.
type gitFixture struct {
	url     string
	mainRev string
	featRev string
}

func newGitFixture(t *testing.T) gitFixture {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "bare.git")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "init", "-b", "main")
	writeFile(t, filepath.Join(work, "README.md"), readmeMain)
	writeFile(t, filepath.Join(work, "mise.toml"), "[tools]\njq = \"1.7.1\"\n")
	writeFile(t, filepath.Join(work, "skills", "alpha", "SKILL.md"), "---\nname: alpha\n---\nalpha from git")
	writeFile(t, filepath.Join(work, "skills", "alpha", "extra.txt"), "extra")
	writeFile(t, filepath.Join(work, "skills", "beta", "SKILL.md"), "---\nname: beta\n---\nbeta from git")
	writeFile(t, filepath.Join(work, "skills", "notaskill", "README.md"), "no SKILL.md here")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "main")
	mainRev := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, work, "checkout", "-b", "feature")
	writeFile(t, filepath.Join(work, "README.md"), "feature readme")
	runGit(t, work, "commit", "-am", "feature")
	featRev := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, work, "checkout", "main")
	runGit(t, root, "clone", "--bare", work, bare)
	return gitFixture{url: "file://" + bare, mainRev: mainRev, featRev: featRev}
}

func TestPrepareGitWorkspaceAndSkills(t *testing.T) {
	fx := newGitFixture(t)
	cmDir := filepath.Join(t.TempDir(), "cm")
	// A mounted ConfigMap: keys are symlinks into ..data, which is a symlink to a timestamped directory.
	writeFile(t, filepath.Join(cmDir, "..2026_10_18", "alpha.md"), "---\nname: alpha\n---\nalpha from configmap")
	writeFile(t, filepath.Join(cmDir, "..2026_10_18", "gamma.md"), "---\nname: gamma\n---\ngamma")
	if err := os.Symlink("..2026_10_18", filepath.Join(cmDir, "..data")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"alpha.md", "gamma.md"} {
		if err := os.Symlink(filepath.Join("..data", key), filepath.Join(cmDir, key)); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name    string
		ref     string
		wantRev string
		wantMD  string
	}{
		{"default branch", "", fx.mainRev, readmeMain},
		{"branch ref", "feature", fx.featRev, "feature readme"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t, KindSessions)
			api.plan = agentapi.Plan{
				Workspace: &agentapi.WorkspacePlan{Git: &agentapi.GitSource{URL: fx.url, Ref: tc.ref, Depth: 1}},
				Skills: []agentapi.SkillSource{
					{Git: &agentapi.GitSource{URL: fx.url}, Path: "skills"},
					{ConfigMapDir: cmDir},
				},
				Toolchain: &agentapi.ToolchainPlan{Disable: []string{toolGitleaks, "check-jsonschema"}},
			}
			paths := testPaths(t)
			if err := api.driver(paths).Prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(paths.Workspace, "README.md")); got != tc.wantMD {
				t.Fatalf("README = %q, want %q", got, tc.wantMD)
			}

			// Skills: git alpha and beta, then the ConfigMap's alpha replaces alpha (and its extra file) and adds gamma.
			alpha := readFile(t, filepath.Join(paths.SkillsDir, "alpha", "SKILL.md"))
			if !strings.Contains(alpha, "alpha from configmap") {
				t.Fatalf("later alpha must replace the earlier one: %q", alpha)
			}
			if _, err := os.Stat(filepath.Join(paths.SkillsDir, "alpha", "extra.txt")); err == nil {
				t.Fatal("replaced skill must not keep files of the earlier one")
			}
			if got := readFile(t, filepath.Join(paths.SkillsDir, "beta", "SKILL.md")); !strings.Contains(got, "beta from git") {
				t.Fatalf("beta = %q", got)
			}
			if got := readFile(t, filepath.Join(paths.SkillsDir, "gamma", "SKILL.md")); !strings.Contains(got, "gamma") {
				t.Fatalf("gamma = %q", got)
			}
			for _, absent := range []string{"notaskill", "..data", "..2026_10_18"} {
				if _, err := os.Stat(filepath.Join(paths.SkillsDir, absent)); err == nil {
					t.Fatalf("%s must not be installed as a skill", absent)
				}
			}

			// The prepared report carries HEAD and the toolchain hash (mise.toml is in both branches).
			unlock := api.lock()
			defer unlock()
			if len(api.prepared) != 1 {
				t.Fatalf("prepared reports = %v", api.prepared)
			}
			wantHash, err := ToolchainHash(paths.Workspace, []string{"check-jsonschema", toolGitleaks})
			if err != nil || wantHash == "" {
				t.Fatalf("hash = %q, %v", wantHash, err)
			}
			got := api.prepared[0]
			if got.WorkspaceRevision != tc.wantRev || got.ToolchainHash != wantHash {
				t.Fatalf("prepared = %+v, want revision %s hash %s", got, tc.wantRev, wantHash)
			}
		})
	}
}

func TestToolchainHash(t *testing.T) {
	ws := t.TempDir()
	if h, err := ToolchainHash(ws, []string{"x"}); err != nil || h != "" {
		t.Fatalf("no mise.toml: %q, %v", h, err)
	}
	writeFile(t, filepath.Join(ws, "mise.toml"), "TOML")
	sum := sha256.Sum256([]byte("TOML\n\na,b"))
	want := hex.EncodeToString(sum[:])
	if h, _ := ToolchainHash(ws, []string{"b", "a"}); h != want {
		t.Fatalf("without lock: %q, want %q", h, want)
	}
	writeFile(t, filepath.Join(ws, "mise.lock"), "LOCK")
	sum = sha256.Sum256([]byte("TOML\nLOCK\n"))
	if h, _ := ToolchainHash(ws, nil); h != hex.EncodeToString(sum[:]) {
		t.Fatalf("with lock and no disable list: %q", h)
	}
}

func artifactPlan(a *fakeAPI, name string, body []byte, digest string) agentapi.Plan {
	a.artifacts[name] = body
	return agentapi.Plan{Workspace: &agentapi.WorkspacePlan{Artifact: &agentapi.Artifact{
		URL: a.artifactURL(name), Digest: digest, Revision: "main@sha1:abc",
	}}}
}

func TestPrepareArtifact(t *testing.T) {
	api := newFakeAPI(t, KindSessions)
	body := buildTarGz(t,
		tarEntry{Name: "./README.md", Body: readmeArtifact},
		tarEntry{Name: "./mise.toml", Body: "TOML"},
	)
	api.plan = artifactPlan(api, "ws.tgz", body, sha256Digest(body))
	paths := testPaths(t)
	if err := api.driver(paths).Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(paths.Workspace, "README.md")); got != readmeArtifact {
		t.Fatalf("README = %q", got)
	}
	unlock := api.lock()
	defer unlock()
	sum := sha256.Sum256([]byte("TOML\n\n"))
	want := agentapi.Prepared{WorkspaceRevision: "main@sha1:abc", ToolchainHash: hex.EncodeToString(sum[:])}
	if len(api.prepared) != 1 || api.prepared[0] != want {
		t.Fatalf("prepared = %+v, want %+v", api.prepared, want)
	}
}

func TestPrepareArtifactDigestMismatch(t *testing.T) {
	api := newFakeAPI(t, KindSessions)
	body := buildTarGz(t, tarEntry{Name: "README.md", Body: readmeArtifact})
	api.plan = artifactPlan(api, "ws.tgz", body, sha256Digest([]byte("something else")))
	paths := testPaths(t)
	err := api.driver(paths).Prepare(context.Background())
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("err = %v, want digest mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(paths.Workspace, "README.md")); err == nil {
		t.Fatal("a corrupt artifact must not be extracted")
	}
	unlock := api.lock()
	defer unlock()
	if len(api.prepared) != 0 {
		t.Fatalf("prepared must not be reported: %v", api.prepared)
	}
}

func TestPrepareArtifactRejectsUnsafeArchiveAndBadDigest(t *testing.T) {
	api := newFakeAPI(t, KindSessions)
	evil := buildTarGz(t, tarEntry{Name: "../../escape", Body: "x"})
	api.plan = artifactPlan(api, "evil.tgz", evil, sha256Digest(evil))
	if err := api.driver(testPaths(t)).Prepare(context.Background()); err == nil || !strings.Contains(err.Error(), "..") {
		t.Fatalf("unsafe archive: %v", err)
	}

	api.plan = artifactPlan(api, "ok.tgz", evil, "md5:abc")
	if err := api.driver(testPaths(t)).Prepare(context.Background()); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("unsupported digest: %v", err)
	}

	api.plan = artifactPlan(api, "big.tgz", buildTarGz(t, tarEntry{Name: "a", Body: "x"}), "sha256:00")
	d := api.driver(testPaths(t))
	d.DownloadMax = 4
	if err := d.Prepare(context.Background()); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized artifact: %v", err)
	}
}

func TestPrepareAgentPlanRoute(t *testing.T) {
	fx := newGitFixture(t)
	api := newFakeAPI(t, KindAgents)
	api.plan = agentapi.Plan{
		Workspace: &agentapi.WorkspacePlan{Git: &agentapi.GitSource{URL: fx.url}},
		Toolchain: &agentapi.ToolchainPlan{Hash: "stored"},
	}
	paths := testPaths(t)
	if err := api.driver(paths).Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(paths.Workspace, "README.md")); got != readmeMain {
		t.Fatalf("README = %q", got)
	}
	unlock := api.lock()
	defer unlock()
	// Toolsync pods use the agent plan route, never download the stored toolchain and report nothing back.
	if want := []string{"GET /v1/agents/ns/a1/plan"}; !slices.Equal(api.requests, want) {
		t.Fatalf("requests = %v, want %v", api.requests, want)
	}
}

func TestFromEnvPrepareUsesAgentRouteWhenOnlyAgentIsSet(t *testing.T) {
	env := map[string]string{agentapi.EnvAPI: "http://x:8083", agentapi.EnvAgent: "ns/a1"}
	d, err := FromEnv(CmdPrepare, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if d.API.Target != (Target{Kind: KindAgents, Namespace: "ns", Name: "a1"}) {
		t.Fatalf("target = %+v", d.API.Target)
	}
	env[agentapi.EnvSession] = "ns/s1"
	if d, err = FromEnv(CmdPrepare, func(k string) string { return env[k] }); err != nil || d.API.Target.Kind != KindSessions {
		t.Fatalf("session identity wins for prepare: %+v %v", d, err)
	}
	if _, err = FromEnv(CmdToolsync, func(k string) string { return map[string]string{agentapi.EnvAPI: "x"}[k] }); err == nil {
		t.Fatal("toolsync without an agent identity must fail")
	}
	if _, err = FromEnv(CmdRun, func(string) string { return "" }); err == nil {
		t.Fatal("no API URL must fail")
	}
	env[agentapi.EnvSession] = "no-slash"
	if _, err = FromEnv(CmdRun, func(k string) string { return env[k] }); err == nil {
		t.Fatal("malformed identity must fail")
	}
}

func TestPrepareRestoresStateKubeconfigAndLinks(t *testing.T) {
	api := newFakeAPI(t, KindSessions)
	api.plan = agentapi.Plan{
		Toolchain:  &agentapi.ToolchainPlan{Hash: "h1"},
		Continuity: &agentapi.ContinuityPlan{ID: continuityID},
		Kubeconfig: true,
		Links:      []agentapi.Link{{Path: "state/cache", Target: "/data/cache"}},
	}
	api.tarballs["/toolchain"] = buildTarGz(t,
		tarEntry{Name: "data/installs/jq/1.7.1/bin/jq", Body: "jq", Mode: 0o755})
	api.tarballs["/continuity"] = buildTarGz(t, tarEntry{Name: "fake.jsonl", Body: historyText})
	paths := testPaths(t)
	writeFile(t, paths.SANamespace, "agents-ns\n")

	if err := api.driver(paths).Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(paths.MiseDir, "data", "installs", "jq", "1.7.1", "bin", "jq")); got != "jq" {
		t.Fatalf("toolchain = %q", got)
	}
	if got := readFile(t, filepath.Join(paths.SessionDir, "fake.jsonl")); got != historyText {
		t.Fatalf("continuity = %q", got)
	}
	if target, err := os.Readlink(filepath.Join(paths.Workspace, "state", "cache")); err != nil || target != "/data/cache" {
		t.Fatalf("link = %q, %v", target, err)
	}

	var kc struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CA     string `json:"certificate-authority"`
			} `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct {
				TokenFile string `json:"tokenFile"`
			} `json:"user"`
		} `json:"users"`
		Contexts []struct {
			Context struct {
				Namespace string `json:"namespace"`
			} `json:"context"`
		} `json:"contexts"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, paths.Kubeconfig)), &kc); err != nil {
		t.Fatal(err)
	}
	if kc.Clusters[0].Cluster.Server != "https://kubernetes.default.svc" || kc.Clusters[0].Cluster.CA != paths.SACA ||
		kc.Users[0].User.TokenFile != paths.SAToken || kc.Contexts[0].Context.Namespace != "agents-ns" {
		t.Fatalf("kubeconfig = %+v", kc)
	}
}

func TestPrepareMissingStateStartsEmpty(t *testing.T) {
	api := newFakeAPI(t, KindSessions)
	api.plan = agentapi.Plan{
		Toolchain:  &agentapi.ToolchainPlan{Hash: "h1"},
		Continuity: &agentapi.ContinuityPlan{ID: continuityID},
	}
	paths := testPaths(t)
	if err := api.driver(paths).Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(paths.SessionDir); len(ents) != 0 {
		t.Fatalf("session dir must start empty: %v", ents)
	}
	unlock := api.lock()
	defer unlock()
	if len(api.prepared) != 1 || api.prepared[0].ToolchainHash != "" {
		t.Fatalf("prepared = %+v", api.prepared)
	}
}

func TestPrepareCorruptContinuityStartsEmpty(t *testing.T) {
	api := newFakeAPI(t, KindSessions)
	api.plan = agentapi.Plan{Continuity: &agentapi.ContinuityPlan{ID: continuityID}}
	api.tarballs["/continuity"] = []byte("this is not gzip")
	paths := testPaths(t)
	writeFile(t, filepath.Join(paths.SessionDir, "stale.jsonl"), "partial")
	if err := api.driver(paths).Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(paths.SessionDir); len(ents) != 0 {
		t.Fatalf("a corrupt continuity tarball must be discarded: %v", ents)
	}
}

func TestPrepareLinkSafety(t *testing.T) {
	for name, link := range map[string]agentapi.Link{
		"absolute path":     {Path: "/etc/x", Target: dataTarget},
		"escaping path":     {Path: "../x", Target: dataTarget},
		"relative target":   {Path: "x", Target: "data"},
		"empty path":        {Path: ".", Target: dataTarget},
		"through a symlink": {Path: "out/x", Target: dataTarget},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t, KindSessions)
			api.plan = agentapi.Plan{Links: []agentapi.Link{link}}
			paths := testPaths(t)
			outside := t.TempDir()
			if err := os.MkdirAll(paths.Workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(paths.Workspace, "out")); err != nil {
				t.Fatal(err)
			}
			if err := api.driver(paths).Prepare(context.Background()); err == nil {
				t.Fatal("expected an error")
			}
			if _, err := os.Lstat(filepath.Join(outside, "x")); err == nil {
				t.Fatal("link escaped the workspace")
			}
		})
	}
}

func TestGitEnvKeepsCredentialsOutOfArgv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, agentapi.AuthTokenFile), "s3cr3t-token\n")
	env, cleanup, err := gitEnv(agentapi.GitSource{AuthType: authToken, AuthDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:s3cr3t-token"))
	want := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=" + header}
	if !slices.Equal(env, want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
	env, cleanup, err = gitEnv(agentapi.GitSource{AuthType: authToken, AuthDir: dir, Username: "bot"})
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if !strings.Contains(env[2], base64.StdEncoding.EncodeToString([]byte("bot:s3cr3t-token"))) {
		t.Fatalf("custom username ignored: %v", env)
	}

	ssh := t.TempDir()
	writeFile(t, filepath.Join(ssh, agentapi.AuthSSHKeyFile), "KEY")
	writeFile(t, filepath.Join(ssh, agentapi.AuthKnownHostsFile), "github.com ssh-ed25519 AAAA")
	env, cleanup, err = gitEnv(agentapi.GitSource{AuthType: "ssh", AuthDir: ssh})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cmd := strings.TrimPrefix(env[0], "GIT_SSH_COMMAND=")
	for _, want := range []string{
		"UserKnownHostsFile='" + filepath.Join(ssh, agentapi.AuthKnownHostsFile) + "'",
		"-o StrictHostKeyChecking=yes", "-o IdentitiesOnly=yes", "ssh -i '",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("GIT_SSH_COMMAND = %q, missing %q", cmd, want)
		}
	}
	keyFile, _, _ := strings.Cut(strings.TrimPrefix(cmd, "ssh -i '"), "'")
	st, err := os.Stat(keyFile)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("private key copy: %v %v", st, err)
	}
	if got := readFile(t, keyFile); got != "KEY\n" {
		t.Fatalf("key = %q", got)
	}

	if _, _, err := gitEnv(agentapi.GitSource{AuthType: "kerberos"}); err == nil {
		t.Fatal("unknown auth types must fail")
	}
}

func TestGitCloneWithTokenAuthRuns(t *testing.T) {
	fx := newGitFixture(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, agentapi.AuthTokenFile), "s3cr3t-token")
	api := newFakeAPI(t, KindSessions)
	d := api.driver(testPaths(t))
	dest := filepath.Join(t.TempDir(), "clone")
	src := agentapi.GitSource{URL: fx.url, AuthType: authToken, AuthDir: dir}
	if err := d.gitClone(context.Background(), src, dest); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dest, "README.md")); got != "main readme" {
		t.Fatalf("README = %q", got)
	}
}

// The workspace volume is owned by root while the agent is not: git must be told to trust it.
func TestTrustWorkspace(t *testing.T) {
	paths := testPaths(t)
	d := &Driver{Paths: paths}
	if err := d.trustWorkspace(); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(paths.Home, ".gitconfig")
	out, err := exec.Command("git", "config", "--file", cfg, "--get-all", "safe.directory").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != paths.Workspace {
		t.Fatalf("safe.directory in %s = %q, %v; want %q", cfg, out, err, paths.Workspace)
	}
}
