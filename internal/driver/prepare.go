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
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// defaultSkillsPath is the directory of <skill>/SKILL.md entries inside a skill source.
const defaultSkillsPath = "skills"

// kubeName names the cluster, user and context of the generated kubeconfig.
const kubeName = "default"

// Prepare is the init container: it builds the workspace, installs skills, restores the
// toolchain and continuity state, writes the kubeconfig and the links, and (for sessions)
// reports the workspace revision and toolchain hash back to the operator.
func (d *Driver) Prepare(ctx context.Context) error {
	session := d.API.Target.Kind == KindSessions

	cctx, cancel := child(ctx, controlTimeout)
	plan, err := d.API.Plan(cctx)
	cancel()
	if err != nil {
		return fmt.Errorf("fetch plan: %w", err)
	}
	if plan.Thread != nil {
		if !session {
			return errors.New("a thread plan needs a session identity")
		}
		return d.prepareThread(ctx, plan)
	}

	revision, err := d.prepareWorkspace(ctx, plan.Workspace)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if err := d.installSkills(ctx, plan.Skills); err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	if session {
		if plan.Toolchain != nil && plan.Toolchain.Hash != "" {
			found, err := d.restoreTarball(ctx, "/toolchain", d.Paths.MiseDir)
			if err != nil {
				return fmt.Errorf("toolchain: %w", err)
			}
			if !found {
				d.Log.Warn("operator has no stored toolchain tarball; continuing without", "hash", plan.Toolchain.Hash)
			}
		}
		if plan.Continuity != nil {
			if err := d.restoreContinuity(ctx); err != nil {
				return fmt.Errorf("continuity: %w", err)
			}
		}
	}
	if plan.Kubeconfig {
		if err := d.writeKubeconfig(); err != nil {
			return fmt.Errorf("kubeconfig: %w", err)
		}
	}
	if err := d.createLinks(plan.Links); err != nil {
		return fmt.Errorf("links: %w", err)
	}
	if !session {
		return nil
	}

	var disable []string
	if plan.Toolchain != nil {
		disable = plan.Toolchain.Disable
	}
	hash, err := ToolchainHash(d.Paths.Workspace, disable)
	if err != nil {
		return fmt.Errorf("hash toolchain: %w", err)
	}
	cctx, cancel = child(ctx, controlTimeout)
	defer cancel()
	if err := d.API.Prepared(cctx, agentapi.Prepared{WorkspaceRevision: revision, ToolchainHash: hash}); err != nil {
		return fmt.Errorf("report prepared: %w", err)
	}
	return nil
}

// prepareWorkspace fills the workspace directory and returns its revision.
func (d *Driver) prepareWorkspace(ctx context.Context, ws *agentapi.WorkspacePlan) (string, error) {
	if err := os.MkdirAll(d.Paths.Workspace, 0o755); err != nil {
		return "", err
	}
	switch {
	case ws == nil:
		return "", nil
	case ws.Artifact != nil:
		if err := d.fetchArtifact(ctx, *ws.Artifact, d.Paths.Workspace); err != nil {
			return "", err
		}
		return ws.Artifact.Revision, nil
	case ws.Git != nil:
		if err := d.gitClone(ctx, *ws.Git, d.Paths.Workspace); err != nil {
			return "", err
		}
		rev, err := d.gitOutput(ctx, d.Paths.Workspace, nil, "rev-parse", "HEAD")
		if err != nil {
			return "", err
		}
		return rev, nil
	default:
		return "", errors.New("workspace plan has neither an artifact nor a git source")
	}
}

// fetchArtifact downloads a Flux artifact, checks its sha256 digest and extracts it into dest.
func (d *Driver) fetchArtifact(ctx context.Context, a agentapi.Artifact, dest string) error {
	algo, want, ok := strings.Cut(a.Digest, ":")
	if !ok || algo != "sha256" || want == "" {
		return fmt.Errorf("artifact digest %q is not sha256:<hex>", a.Digest)
	}
	dctx, cancel := child(ctx, d.DownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", a.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", a.URL, resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "artifact-*.tar.gz")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(resp.Body, d.DownloadMax+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", a.URL, err)
	}
	if n > d.DownloadMax {
		return fmt.Errorf("artifact %s exceeds the %d byte limit", a.URL, d.DownloadMax)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("artifact digest mismatch: got sha256:%s, want sha256:%s", got, want)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return ExtractTarGz(tmp, dest, extractMax)
}

// installSkills installs every skill source, in order, into the skills directory; a later
// skill with the same name replaces an earlier one.
func (d *Driver) installSkills(ctx context.Context, skills []agentapi.SkillSource) error {
	if len(skills) == 0 {
		return nil
	}
	if err := os.MkdirAll(d.Paths.SkillsDir, 0o755); err != nil {
		return err
	}
	for i, s := range skills {
		if err := d.installSkillSource(ctx, s); err != nil {
			return fmt.Errorf("source %d: %w", i, err)
		}
	}
	return nil
}

func (d *Driver) installSkillSource(ctx context.Context, s agentapi.SkillSource) error {
	if s.ConfigMapDir != "" {
		return d.installConfigMapSkills(s.ConfigMapDir)
	}
	sub := s.Path
	if sub == "" {
		sub = defaultSkillsPath
	}
	rel, err := memberName(sub)
	if err != nil {
		return fmt.Errorf("skills path: %w", err)
	}
	tmp, err := os.MkdirTemp("", "skills-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	switch {
	case s.Git != nil:
		src := filepath.Join(tmp, "repo")
		if err := d.gitClone(ctx, *s.Git, src); err != nil {
			return err
		}
		return d.copySkills(filepath.Join(src, filepath.FromSlash(rel)))
	case s.Artifact != nil:
		src := filepath.Join(tmp, "artifact")
		if err := d.fetchArtifact(ctx, *s.Artifact, src); err != nil {
			return err
		}
		return d.copySkills(filepath.Join(src, filepath.FromSlash(rel)))
	default:
		return errors.New("skill source has no git, artifact or configMap")
	}
}

// copySkills copies every <dir>/*/SKILL.md directory into the skills directory.
func (d *Driver) copySkills(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read skills directory: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		src := filepath.Join(dir, e.Name())
		if st, err := os.Lstat(filepath.Join(src, "SKILL.md")); err != nil || !st.Mode().IsRegular() {
			continue
		}
		dst := filepath.Join(d.Paths.SkillsDir, e.Name())
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := copyTree(src, dst); err != nil {
			return fmt.Errorf("copy skill %q: %w", e.Name(), err)
		}
	}
	return nil
}

// copyTree copies the directories and regular files of src to dst; symlinks and special files are skipped.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := e.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode().IsRegular():
			return copyFile(p, target, info.Mode().Perm()|0o600)
		default:
			return nil
		}
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

// installConfigMapSkills turns each "<skill>.md" file of a mounted ConfigMap into <skill>/SKILL.md.
func (d *Driver) installConfigMapSkills(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read ConfigMap skills: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		skill, ok := strings.CutSuffix(name, ".md")
		// Kubernetes volumes add "..data" and timestamped directories next to the keys.
		if !ok || skill == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(skill, `/\`) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name)) // follows the ..data symlink
		if err != nil {
			return fmt.Errorf("read ConfigMap skill %q: %w", name, err)
		}
		dst := filepath.Join(d.Paths.SkillsDir, skill)
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, "SKILL.md"), body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// restoreTarball downloads GET <suffix> and extracts it into dest. found is false on HTTP 404.
func (d *Driver) restoreTarball(ctx context.Context, suffix, dest string) (bool, error) {
	tctx, cancel := child(ctx, tarballTimeout)
	defer cancel()
	body, found, err := d.API.Tarball(tctx, suffix)
	if err != nil || !found {
		return false, err
	}
	defer func() { _ = body.Close() }()
	if err := ExtractTarGz(body, dest, extractMax); err != nil {
		return true, err
	}
	return true, nil
}

// restoreContinuity unpacks the stored omp history into the session directory. It is best
// effort: an unreadable tarball is discarded so the session starts empty instead of
// failing every attempt forever.
func (d *Driver) restoreContinuity(ctx context.Context) error {
	found, err := d.restoreTarball(ctx, "/continuity", d.Paths.SessionDir)
	if err == nil {
		if !found {
			d.Log.Info("no stored continuity; starting empty")
		}
		return nil
	}
	var se *StatusError
	if errors.As(err, &se) || errors.Is(err, context.Canceled) {
		return err
	}
	d.Log.Warn("discarding unreadable continuity tarball", "error", err)
	if err := os.RemoveAll(d.Paths.SessionDir); err != nil {
		return err
	}
	return os.MkdirAll(d.Paths.SessionDir, 0o755)
}

// writeKubeconfig writes a kubeconfig that uses the pod's service account.
func (d *Driver) writeKubeconfig() error {
	ns := d.API.Target.Namespace
	if raw, err := os.ReadFile(d.Paths.SANamespace); err == nil && strings.TrimSpace(string(raw)) != "" {
		ns = strings.TrimSpace(string(raw))
	}
	cfg := map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters": []map[string]any{named(kubeName, "cluster", map[string]any{
			"server":                "https://kubernetes.default.svc",
			"certificate-authority": d.Paths.SACA,
		})},
		"users": []map[string]any{named(kubeName, "user", map[string]any{"tokenFile": d.Paths.SAToken})},
		"contexts": []map[string]any{named(kubeName, "context", map[string]any{
			"cluster": kubeName, "user": kubeName, "namespace": ns,
		})},
		"current-context": kubeName,
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.Paths.Kubeconfig), 0o755); err != nil {
		return err
	}
	return os.WriteFile(d.Paths.Kubeconfig, raw, 0o600)
}

// createLinks creates the symlinks of the plan inside the workspace. The workspace may hold
// repository-controlled symlinks, so every operation goes through os.Root.
func (d *Driver) createLinks(links []agentapi.Link) error {
	if len(links) == 0 {
		return nil
	}
	root, err := os.OpenRoot(d.Paths.Workspace)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, l := range links {
		rel, err := memberName(l.Path)
		if err != nil || rel == "" {
			return fmt.Errorf("link path %q must be relative and stay inside the workspace", l.Path)
		}
		if !filepath.IsAbs(l.Target) {
			return fmt.Errorf("link %q: target %q must be absolute", l.Path, l.Target)
		}
		if dir := path.Dir(rel); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("link %q: %w", l.Path, err)
			}
		}
		if st, err := root.Lstat(rel); err == nil {
			if st.Mode()&fs.ModeSymlink == 0 {
				return fmt.Errorf("link %q: path already exists and is not a symlink", l.Path)
			}
			if err := root.Remove(rel); err != nil {
				return fmt.Errorf("link %q: %w", l.Path, err)
			}
		}
		if err := root.Symlink(l.Target, rel); err != nil {
			return fmt.Errorf("link %q: %w", l.Path, err)
		}
	}
	return nil
}

// named is a kubeconfig list entry: {name: <name>, <key>: <value>}.
func named(name, key string, value map[string]any) map[string]any {
	return map[string]any{"name": name, key: value}
}
