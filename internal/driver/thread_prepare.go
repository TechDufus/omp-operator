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
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

const (
	// Refs prepare keeps next to the branch so the objects stay reachable.
	refBase    = "refs/omp-cloud/base"
	refResult  = "refs/omp-cloud/result"
	refHandoff = "refs/omp-cloud/handoff"

	// emptyTree is git's well-known empty tree: the diff base of a thread without a base commit.
	emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

	// threadSetupMax bounds what the shipped omp setup may expand to.
	threadSetupMax int64 = 512 << 20
	// threadMiseTimeout bounds `mise install` for a toolchain that is not cached yet.
	threadMiseTimeout = 15 * time.Minute

	defaultAuthorName  = "omp-cloud"
	defaultAuthorEmail = "omp-cloud@localhost"
	maxEventError      = 300
)

// prepareThread is Prepare for a sitting of an OmpThread. It rebuilds the user's workspace
// from the base commit, the uploaded handoff (or the previous sitting's result), then
// restores the shipped omp setup, the toolchain and the conversation.
func (d *Driver) prepareThread(ctx context.Context, plan agentapi.Plan) error {
	t := plan.Thread
	ws := d.Paths.Workspace
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return err
	}
	if err := d.git(ctx, "", nil, "init", "-q", ws); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	tip, err := d.restoreRepo(ctx, t)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if err := d.configureRepo(ctx, t, tip); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if err := d.copyThreadSecrets(t.SecretFiles); err != nil {
		return fmt.Errorf("secret files: %w", err)
	}
	if t.HasSetup {
		if err := d.restoreSetup(ctx); err != nil {
			return fmt.Errorf("omp setup: %w", err)
		}
	}
	if err := d.installSkills(ctx, plan.Skills); err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	hash, err := d.restoreThreadToolchain(ctx, t.Toolchain)
	if err != nil {
		return fmt.Errorf("toolchain: %w", err)
	}
	if plan.Continuity != nil {
		if err := d.restoreContinuity(ctx); err != nil {
			return fmt.Errorf("continuity: %w", err)
		}
	}
	if plan.Kubeconfig {
		if err := d.writeKubeconfig(); err != nil {
			return fmt.Errorf("kubeconfig: %w", err)
		}
	}
	cctx, cancel := child(ctx, controlTimeout)
	defer cancel()
	if err := d.API.Prepared(cctx, agentapi.Prepared{WorkspaceRevision: tip, ToolchainHash: hash}); err != nil {
		return fmt.Errorf("report prepared: %w", err)
	}
	return nil
}

// restoreRepo fetches the base commit and the uploaded objects into the fresh repository and
// returns the commit the thread branch starts from: the previous sitting's result, else the
// handoff, else the base itself.
func (d *Driver) restoreRepo(ctx context.Context, t *agentapi.ThreadPlan) (string, error) {
	ws := d.Paths.Workspace
	if t.BaseCommit != "" {
		if err := d.fetchBase(ctx, t); err != nil {
			return "", err
		}
	}
	switch {
	case t.HasResult:
		if err := d.fetchBundle(ctx, "/thread/result", "+refs/heads/"+t.Branch+":"+refResult); err != nil {
			return "", fmt.Errorf("fetch the stored result: %w", err)
		}
		return d.gitOutput(ctx, ws, nil, "rev-parse", "--verify", refResult+"^{commit}")
	case t.HasSource:
		if err := d.fetchBundle(ctx, "/thread/source", "+"+refHandoff+":"+refHandoff); err != nil {
			return "", fmt.Errorf("fetch the uploaded source: %w", err)
		}
		got, err := d.gitOutput(ctx, ws, nil, "rev-parse", "--verify", refHandoff+"^{commit}")
		if err != nil {
			return "", err
		}
		if got != t.HandoffCommit {
			return "", fmt.Errorf("the uploaded source is at %s, not the handoff commit %s", got, t.HandoffCommit)
		}
		return got, nil
	case t.BaseCommit != "":
		return t.BaseCommit, nil
	default:
		return "", errors.New("the thread has neither an uploaded source nor a base commit")
	}
}

// fetchBase makes BaseCommit available: from the remote branch it came from, else by its id.
// The remote's own answers only reach the pod log; the user gets the actionable message.
func (d *Driver) fetchBase(ctx context.Context, t *agentapi.ThreadPlan) error {
	ws := d.Paths.Workspace
	if t.FetchURL == "" {
		return fmt.Errorf("no cloud.git host of the agent matches %s; add it to the agent, then hand off again", t.RemoteURL)
	}
	env, cleanup, err := gitEnv(agentapi.GitSource{AuthDir: t.AuthDir, AuthType: t.AuthType, Username: t.Username})
	if err != nil {
		return err
	}
	defer cleanup()
	have := func() bool {
		return d.git(ctx, ws, nil, "cat-file", "-e", t.BaseCommit+"^{commit}") == nil
	}
	if t.BaseRef != "" {
		if err := d.git(ctx, ws, env, "fetch", "--no-tags", "-q", t.FetchURL, "+refs/heads/"+t.BaseRef+":"+refBase); err != nil {
			d.Log.Warn("fetch base branch", "error", err)
		}
	}
	if !have() {
		if err := d.git(ctx, ws, env, "fetch", "--no-tags", "-q", t.FetchURL, t.BaseCommit); err != nil {
			d.Log.Warn("fetch base commit", "error", err)
		}
	}
	if !have() {
		return fmt.Errorf("base commit %s is not on %s %s; fetch and push, then hand off again", t.BaseCommit, t.RemoteURL, t.BaseRef)
	}
	return nil
}

// fetchBundle downloads GET <suffix> (a git bundle) and fetches refspec from it.
func (d *Driver) fetchBundle(ctx context.Context, suffix, refspec string) error {
	file, err := d.download(ctx, suffix)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file) }()
	return d.git(ctx, d.Paths.Workspace, nil, "fetch", "-q", file, refspec)
}

// download saves GET <suffix> to a temporary file; a 404 is an error here, because the plan said the content exists.
func (d *Driver) download(ctx context.Context, suffix string) (string, error) {
	tctx, cancel := child(ctx, tarballTimeout)
	defer cancel()
	body, found, err := d.API.Tarball(tctx, suffix)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("GET %s: not found", suffix)
	}
	defer func() { _ = body.Close() }()
	f, err := os.CreateTemp("", "thread-*.bundle")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// configureRepo checks out the thread branch at tip and sets the repository's identity. The
// remote is added without credentials: the agent container never holds any.
func (d *Driver) configureRepo(ctx context.Context, t *agentapi.ThreadPlan, tip string) error {
	ws := d.Paths.Workspace
	if err := d.git(ctx, ws, nil, "checkout", "-q", "-f", "-B", t.Branch, tip); err != nil {
		return err
	}
	name, email := t.Author.Name, t.Author.Email
	if name == "" {
		name = defaultAuthorName
	}
	if email == "" {
		email = defaultAuthorEmail
	}
	for _, kv := range [][2]string{
		{"user.name", name}, {"user.email", email}, {"commit.gpgsign", "false"}, {"tag.gpgsign", "false"},
	} {
		if err := d.git(ctx, ws, nil, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	if t.RemoteURL == "" {
		return nil
	}
	remote := sanitizeRemote(t.RemoteURL)
	if err := d.git(ctx, ws, nil, "config", "remote.origin.url", remote); err != nil {
		return err
	}
	return d.git(ctx, ws, nil, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
}

// sanitizeRemote drops credentials embedded in a remote URL: the whole userinfo of http(s) URLs
// (it may be a token), the password of the others. scp-like remotes carry none.
func sanitizeRemote(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	switch {
	case u.Scheme == "http" || u.Scheme == "https":
		u.User = nil
	case u.User != nil:
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

// copyThreadSecrets copies the shipped secret files into the workspace with mode 0600 and
// keeps them out of the agent's commits with .git/info/exclude. Every write goes through
// os.Root: the checked-out repository may hold symlinks.
func (d *Driver) copyThreadSecrets(files []agentapi.ThreadSecretFile) error {
	if len(files) == 0 {
		return nil
	}
	root, err := os.OpenRoot(d.Paths.Workspace)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	exclude := make([]string, 0, len(files))
	for _, f := range files {
		rel, err := memberName(f.Path)
		if err != nil || rel == "" || rel == ".git" || strings.HasPrefix(rel, ".git/") {
			return fmt.Errorf("path %q must be relative and stay inside the workspace, outside .git", f.Path)
		}
		if strings.ContainsAny(f.Key, `/\`) || f.Key == "" || f.Key == "." || f.Key == ".." {
			return fmt.Errorf("invalid secret file key %q", f.Key)
		}
		src, err := os.Open(filepath.Join(d.Paths.ThreadSecrets, f.Key))
		if err != nil {
			return err
		}
		err = writeRooted(root, rel, src)
		_ = src.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		exclude = append(exclude, "/"+escapeIgnore(rel))
	}
	infoDir := filepath.Join(d.Paths.Workspace, ".git", "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(infoDir, "exclude"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, err = out.WriteString(strings.Join(exclude, "\n") + "\n")
	return errors.Join(err, out.Close())
}

// writeRooted writes r to rel inside root with mode 0600, creating parent directories.
func writeRooted(root *os.Root, rel string, r io.Reader) error {
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Chmod(0o600)
	}
	return errors.Join(err, f.Close())
}

// escapeIgnore escapes the characters that are special in a gitignore pattern.
func escapeIgnore(p string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`).Replace(p)
}

// restoreSetup extracts the user's shipped omp setup (settings, extensions, memory, skills,
// context files) into the agent directory.
func (d *Driver) restoreSetup(ctx context.Context) error {
	tctx, cancel := child(ctx, tarballTimeout)
	defer cancel()
	body, found, err := d.API.Tarball(tctx, "/thread/setup")
	if err != nil {
		return err
	}
	if !found {
		return errors.New("GET thread/setup: not found")
	}
	defer func() { _ = body.Close() }()
	return ExtractTarGz(body, d.Paths.OmpAgentDir, threadSetupMax)
}

// restoreThreadToolchain makes the workspace's mise tools available from the agent's shared
// cache, installing and caching them on a miss. It returns the toolchain hash, empty when the
// agent has no toolchain or the workspace no mise.toml. An install that fails is an event, not
// an error: the agent can still work without those tools.
func (d *Driver) restoreThreadToolchain(ctx context.Context, tc *agentapi.ToolchainPlan) (string, error) {
	if tc == nil {
		return "", nil
	}
	hash, err := ToolchainHash(d.Paths.Workspace, tc.Disable)
	if err != nil {
		return "", fmt.Errorf("hash toolchain: %w", err)
	}
	if hash == "" {
		return "", nil
	}
	found, err := d.restoreTarball(ctx, "/toolchains/"+hash, d.Paths.MiseDir)
	if err == nil && found {
		err = d.reshimMise(ctx)
	}
	if err != nil {
		d.postEvent(ctx, eventToolchain, "toolchain restore failed: "+truncate(err.Error(), maxEventError))
	}
	if found && err == nil {
		return hash, nil
	}
	if err := d.miseInstall(ctx, threadMiseTimeout, "MISE_TRUSTED_CONFIG_PATHS="+d.Paths.Workspace); err != nil {
		d.postEvent(ctx, eventToolchain, "toolchain install failed: "+truncate(err.Error(), maxEventError))
		return hash, nil
	}
	if err := d.cacheToolchain(ctx, hash); err != nil {
		d.postEvent(ctx, eventToolchain, "toolchain not cached: "+truncate(err.Error(), maxEventError))
	}
	return hash, nil
}

// cacheToolchain uploads the mise data directory to the agent's toolchain cache under hash.
func (d *Driver) cacheToolchain(ctx context.Context, hash string) error {
	tmp, err := os.CreateTemp("", "toolchain-*.tar.gz")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := PackMiseData(tmp, d.Paths.MiseDataDir); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pack %s: %w", d.Paths.MiseDataDir, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	uctx, cancel := child(ctx, tarballTimeout)
	defer cancel()
	return d.API.PutFile(uctx, "/toolchains/"+hash, tmp.Name())
}

// postEvent posts one progress line outside a run's event sink (prepare has none).
func (d *Driver) postEvent(ctx context.Context, kind, text string) {
	cctx, cancel := child(ctx, controlTimeout)
	defer cancel()
	ev := agentapi.Event{At: time.Now().UTC(), Kind: kind, Text: truncate(text, maxEventText)}
	if err := d.API.Events(cctx, []agentapi.Event{ev}); err != nil {
		d.Log.Warn("post event", "error", err)
	}
}
