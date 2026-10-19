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

package agentapi

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/cloudspec"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	// defaultSkillsPath is the skills directory of a source when spec.path is empty.
	defaultSkillsPath = "skills"
	// defaultGitDepth is the workspace clone depth when spec.depth is unset.
	defaultGitDepth = 1
	// defaultGitUsername pairs with a git access token.
	defaultGitUsername = "x-access-token"

	authTypeSSH   = "ssh"
	authTypeToken = "token"

	// Files of a thread in the store.
	fileSource = "source.bundle"
	fileResult = "result.bundle"
	fileSetup  = "setup.tar.gz"
)

// errNoArtifact reports a Flux source that has no artifact yet.
type errNoArtifact struct{ kind, ns, name string }

func (e errNoArtifact) Error() string {
	return fmt.Sprintf("%s %s/%s has no artifact", e.kind, e.ns, e.name)
}

// agentPlan is the plan of a toolsync pod: workspace and toolchain only.
func (s *Server) agentPlan(ctx context.Context, agent *v1alpha1.OmpAgent) (*Plan, error) {
	plan := &Plan{Toolchain: toolchainPlan(agent)}
	ws, err := s.workspacePlan(ctx, agent)
	if err != nil {
		return nil, err
	}
	plan.Workspace = ws
	return plan, nil
}

// sessionPlan is the plan of a session pod's prepare container. thread is the OmpThread of a
// thread sitting, nil for other sessions.
func (s *Server) sessionPlan(ctx context.Context, agent *v1alpha1.OmpAgent, sess *v1alpha1.OmpSession, thread *v1alpha1.OmpThread) (*Plan, error) {
	var plan *Plan
	if thread != nil {
		// A sitting rebuilds the repository from the thread's files: no agent workspace, links or
		// stored toolchain; the toolchain is cached per repository by the driver.
		plan = &Plan{Thread: s.threadPlan(agent, thread)}
	} else {
		var err error
		if plan, err = s.agentPlan(ctx, agent); err != nil {
			return nil, err
		}
		if ws := agent.Spec.Workspace; ws != nil {
			for _, l := range ws.Links {
				plan.Links = append(plan.Links, Link{Path: l.Path, Target: l.Target})
			}
		}
	}
	for i := range agent.Spec.Skills {
		src, err := s.skillPlan(ctx, i, &agent.Spec.Skills[i])
		if err != nil {
			return nil, err
		}
		plan.Skills = append(plan.Skills, src)
	}
	if sess.Spec.ContinuityKey != "" {
		plan.Continuity = &ContinuityPlan{ID: store.ContinuityID(sess.Spec.ContinuityKey)}
	}
	plan.Kubeconfig = agent.Spec.ServiceAccountName != ""
	return plan, nil
}

// threadPlan describes how prepare restores the workspace and home of a thread.
func (s *Server) threadPlan(agent *v1alpha1.OmpAgent, thread *v1alpha1.OmpThread) *ThreadPlan {
	spec := &thread.Spec
	tp := &ThreadPlan{
		Name:          thread.Name,
		Branch:        cloudspec.Branch(thread.Name),
		RemoteURL:     spec.Repo.RemoteURL,
		BaseRef:       spec.Repo.BaseRef,
		BaseCommit:    spec.Repo.BaseCommit,
		HandoffCommit: spec.Repo.HandoffCommit,
		HasSource:     s.hasThreadFile(thread, fileSource),
		HasResult:     s.hasThreadFile(thread, fileResult),
		HasSetup:      s.hasThreadFile(thread, fileSetup),
		Author:        ThreadAuthor{Name: spec.Author.Name, Email: spec.Author.Email},
	}
	var hosts []v1alpha1.CloudGitHost
	if agent.Spec.Cloud != nil {
		hosts = agent.Spec.Cloud.Git
	}
	host, fetchURL, _ := cloudspec.MatchGitHost(hosts, spec.Repo.RemoteURL)
	tp.FetchURL = fetchURL
	if host != nil {
		var g GitSource
		applyGitAuth(&g, &host.Auth, CloudGitAuthID)
		tp.AuthDir, tp.AuthType, tp.Username = g.AuthDir, g.AuthType, g.Username
	}
	for i, p := range spec.SecretFiles {
		tp.SecretFiles = append(tp.SecretFiles, ThreadSecretFile{Key: ThreadSecretFileKey(i), Path: p})
	}
	if agent.Spec.Toolchain != nil {
		tp.Toolchain = &ToolchainPlan{Disable: agent.Spec.Toolchain.Disable}
	}
	return tp
}

// hasThreadFile reports whether the thread's stored file exists.
func (s *Server) hasThreadFile(thread *v1alpha1.OmpThread, file string) bool {
	f, err := s.Store.OpenThreadFile(thread.Namespace, thread.Name, file)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func toolchainPlan(agent *v1alpha1.OmpAgent) *ToolchainPlan {
	if agent.Spec.Toolchain == nil {
		return nil
	}
	return &ToolchainPlan{
		Hash:    agent.Status.Toolchain.Hash,
		Disable: agent.Spec.Toolchain.Disable,
	}
}

func (s *Server) workspacePlan(ctx context.Context, agent *v1alpha1.OmpAgent) (*WorkspacePlan, error) {
	ws := agent.Spec.Workspace
	switch {
	case ws == nil:
		return nil, nil
	case ws.FluxSource != nil:
		art, err := s.fluxArtifact(ctx, *ws.FluxSource)
		if err != nil {
			return nil, err
		}
		return &WorkspacePlan{Artifact: art}, nil
	case ws.Git != nil:
		depth := int(ws.Git.Depth)
		if depth <= 0 {
			depth = defaultGitDepth
		}
		g := &GitSource{URL: ws.Git.URL, Ref: ws.Git.Ref, Depth: depth}
		applyGitAuth(g, ws.Git.Auth, "workspace")
		return &WorkspacePlan{Git: g}, nil
	}
	return nil, nil
}

func (s *Server) skillPlan(ctx context.Context, i int, sk *v1alpha1.SkillSource) (SkillSource, error) {
	switch {
	case sk.Git != nil:
		g := &GitSource{URL: sk.Git.URL, Ref: sk.Git.Ref, Depth: defaultGitDepth}
		applyGitAuth(g, sk.Git.Auth, SkillIndexID(i))
		return SkillSource{Git: g, Path: pathOrDefault(sk.Git.Path)}, nil
	case sk.ConfigMap != nil:
		return SkillSource{ConfigMapDir: ConfigMapSkillDir(i)}, nil
	case sk.FluxSource != nil:
		art, err := s.fluxArtifact(ctx, sk.FluxSource.FluxSourceRef)
		if err != nil {
			return SkillSource{}, err
		}
		return SkillSource{Artifact: art, Path: pathOrDefault(sk.FluxSource.Path)}, nil
	}
	return SkillSource{}, fmt.Errorf("skills[%d]: no source set", i)
}

func pathOrDefault(p string) string {
	if p == "" {
		return defaultSkillsPath
	}
	return p
}

// GitAuthFor describes the auth Secret of a git source mounted at AuthDir(id): its directory,
// "ssh" or "token", and the username for token auth. All are empty when auth has no usable keys.
func GitAuthFor(auth *v1alpha1.GitAuth, id string) (authDir, authType, username string) {
	if auth == nil {
		return "", "", ""
	}
	switch {
	case auth.SSHKeySecretRef != nil:
		return AuthDir(id), authTypeSSH, ""
	case auth.TokenSecretRef != nil:
		username = auth.Username
		if username == "" {
			username = defaultGitUsername
		}
		return AuthDir(id), authTypeToken, username
	}
	return "", "", ""
}

// applyGitAuth sets the auth fields of g for the auth Secret mounted at AuthDir(id).
func applyGitAuth(g *GitSource, auth *v1alpha1.GitAuth, id string) {
	g.AuthDir, g.AuthType, g.Username = GitAuthFor(auth, id)
}

// fluxArtifact returns the artifact of a Flux GitRepository or OCIRepository.
func (s *Server) fluxArtifact(ctx context.Context, ref v1alpha1.FluxSourceRef) (*Artifact, error) {
	key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	var (
		obj client.Object
		get func() (url, digest, revision string, ok bool)
	)
	switch ref.Kind {
	case v1alpha1.FluxSourceGitRepository:
		repo := &sourcev1.GitRepository{}
		obj = repo
		get = func() (string, string, string, bool) {
			a := repo.Status.Artifact
			if a == nil {
				return "", "", "", false
			}
			return a.URL, a.Digest, a.Revision, true
		}
	case v1alpha1.FluxSourceOCIRepository:
		repo := &sourcev1.OCIRepository{}
		obj = repo
		get = func() (string, string, string, bool) {
			a := repo.Status.Artifact
			if a == nil {
				return "", "", "", false
			}
			return a.URL, a.Digest, a.Revision, true
		}
	default:
		return nil, fmt.Errorf("unsupported Flux source kind %q", ref.Kind)
	}
	if err := s.APIReader.Get(ctx, key, obj); err != nil {
		return nil, fmt.Errorf("get %s %s: %w", ref.Kind, key, err)
	}
	url, digest, revision, ok := get()
	if !ok || url == "" {
		return nil, errNoArtifact{kind: string(ref.Kind), ns: ref.Namespace, name: ref.Name}
	}
	return &Artifact{URL: url, Digest: digest, Revision: revision}, nil
}
