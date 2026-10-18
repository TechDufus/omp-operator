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

// sessionPlan is the plan of a session pod's prepare container.
func (s *Server) sessionPlan(ctx context.Context, agent *v1alpha1.OmpAgent, sess *v1alpha1.OmpSession) (*Plan, error) {
	plan, err := s.agentPlan(ctx, agent)
	if err != nil {
		return nil, err
	}
	for i := range agent.Spec.Skills {
		src, err := s.skillPlan(ctx, i, &agent.Spec.Skills[i])
		if err != nil {
			return nil, err
		}
		plan.Skills = append(plan.Skills, src)
	}
	if ws := agent.Spec.Workspace; ws != nil {
		for _, l := range ws.Links {
			plan.Links = append(plan.Links, Link{Path: l.Path, Target: l.Target})
		}
	}
	if sess.Spec.ContinuityKey != "" {
		plan.Continuity = &ContinuityPlan{ID: store.ContinuityID(sess.Spec.ContinuityKey)}
	}
	plan.Kubeconfig = agent.Spec.ServiceAccountName != ""
	return plan, nil
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

// applyGitAuth sets the auth fields of g for the auth Secret mounted at AuthDir(id).
func applyGitAuth(g *GitSource, auth *v1alpha1.GitAuth, id string) {
	if auth == nil {
		return
	}
	switch {
	case auth.SSHKeySecretRef != nil:
		g.AuthDir, g.AuthType = AuthDir(id), authTypeSSH
	case auth.TokenSecretRef != nil:
		g.AuthDir, g.AuthType = AuthDir(id), authTypeToken
		g.Username = auth.Username
		if g.Username == "" {
			g.Username = defaultGitUsername
		}
	}
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
