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

// Package podspec builds the pods the operator runs for an OmpAgent: one session pod per
// OmpSession attempt, and the toolsync pod that installs an agent's mise toolchain, together
// with the API token Secrets they authenticate with.
package podspec

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/agentspec"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/gateway"
)

const (
	// UserID is the UID, GID and fsGroup every operator pod runs as.
	UserID int64 = 10001

	// DriverBinary is the image entrypoint every container runs.
	DriverBinary = "omp-operator-driver"

	// ToolsyncDeadlineSeconds is the activeDeadlineSeconds of a toolsync pod.
	ToolsyncDeadlineSeconds int64 = 900

	// Computed defaults of agent limits when the field is unset.
	DefaultSessionTimeout   = 30 * time.Minute
	DefaultMaxAttempts      = int32(4)
	DefaultRetryDelay       = 15 * time.Minute
	DefaultSessionRetention = 720 * time.Hour

	// deadlineSlack is added to the session timeout to form the pod's activeDeadlineSeconds.
	deadlineSlack = 10 * time.Minute

	containerPrepare = "prepare"
	containerAgent   = "agent"

	miseOff = "false"

	volWorkspace = "workspace"
	volMise      = "mise"
	volHome      = "home"
	volTmp       = "tmp"
	volRun       = "run"
	volToken     = "token"
)

// SessionTimeout is the agent's per-attempt timeout, defaulting to 30 minutes.
func SessionTimeout(agent *v1alpha1.OmpAgent) time.Duration {
	if d := agent.Spec.Limits.SessionTimeout.Duration; d > 0 {
		return d
	}
	return DefaultSessionTimeout
}

// MaxAttempts is the attempts a session gets, defaulting to 4.
func MaxAttempts(agent *v1alpha1.OmpAgent) int32 {
	if n := agent.Spec.Limits.MaxAttempts; n > 0 {
		return n
	}
	return DefaultMaxAttempts
}

// RetryDelay is the wait before a retryable attempt runs again, defaulting to 15 minutes.
func RetryDelay(agent *v1alpha1.OmpAgent) time.Duration {
	if d := agent.Spec.Limits.RetryDelay.Duration; d > 0 {
		return d
	}
	return DefaultRetryDelay
}

// SessionRetention is how long a finished session is kept, defaulting to 720 hours.
func SessionRetention(agent *v1alpha1.OmpAgent) time.Duration {
	if d := agent.Spec.SessionRetention.Duration; d > 0 {
		return d
	}
	return DefaultSessionRetention
}

// Image is the agent's runtime image: spec.image, else the operator default.
func Image(agent *v1alpha1.OmpAgent, opts *config.Options) string {
	if agent.Spec.Image != "" {
		return agent.Spec.Image
	}
	return opts.DefaultRuntimeImage
}

// SessionPodName is the name of the pod of the session's attempt (1-based).
func SessionPodName(session string, attempt int32) string {
	return session + "-" + strconv.Itoa(int(attempt))
}

// ToolsyncPodName is the name of a toolsync pod started at t.
func ToolsyncPodName(agent string, t time.Time) string {
	return agent + "-toolsync-" + strconv.FormatInt(t.Unix(), 10)
}

// NewToken returns 32 random bytes, hex-encoded: the value of an API token Secret.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// TokenSecret returns a Secret named name with a freshly generated token under
// agentapi.TokenKey, labelled for agent. The caller sets the owner reference.
func TokenSecret(namespace, name, agent string) (*corev1.Secret, error) {
	tok, err := NewToken()
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
				v1alpha1.LabelAgent:     agent,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{agentapi.TokenKey: []byte(tok)},
	}, nil
}

// SessionPod builds the pod of the given attempt (1-based) of session s. Owner references
// are left to the caller.
func SessionPod(agent *v1alpha1.OmpAgent, s *v1alpha1.OmpSession, opts *config.Options, attempt int32) *corev1.Pod {
	return build(buildParams{
		agent:     agent,
		opts:      opts,
		namespace: s.Namespace,
		name:      SessionPodName(s.Name, attempt),
		session:   true,
		labels: map[string]string{
			v1alpha1.LabelSession: labelValue(s.Name),
			v1alpha1.LabelRole:    string(v1alpha1.PodRoleSession),
		},
		envName:     agentapi.EnvSession,
		envValue:    s.Namespace + "/" + s.Name,
		tokenSecret: agentapi.SessionTokenSecretName(s.Name),
		deadline:    int64((SessionTimeout(agent) + deadlineSlack) / time.Second),
		mainArg:     "run",
	})
}

// ToolsyncPod builds the pod that installs the agent's toolchain, named for time t. Owner
// references are left to the caller.
func ToolsyncPod(agent *v1alpha1.OmpAgent, opts *config.Options, t time.Time) *corev1.Pod {
	return build(buildParams{
		agent:       agent,
		opts:        opts,
		namespace:   agent.Namespace,
		name:        ToolsyncPodName(agent.Name, t),
		labels:      map[string]string{v1alpha1.LabelRole: string(v1alpha1.PodRoleToolsync)},
		envName:     agentapi.EnvAgent,
		envValue:    agent.Namespace + "/" + agent.Name,
		tokenSecret: agentapi.ToolsyncTokenSecretName(agent.Name),
		deadline:    ToolsyncDeadlineSeconds,
		mainArg:     "toolsync",
	})
}

type buildParams struct {
	agent     *v1alpha1.OmpAgent
	opts      *config.Options
	namespace string
	name      string
	// session is true for session pods: they get skills, credentials, MCP secrets, the gateway
	// token, the kubeconfig, the service account token and the agent's pod overrides. Toolsync
	// pods get only the workspace and toolchain parts.
	session     bool
	labels      map[string]string
	envName     string // OMP_OPERATOR_SESSION or OMP_OPERATOR_AGENT
	envValue    string // "<ns>/<name>"
	tokenSecret string
	deadline    int64
	mainArg     string
}

func build(p buildParams) *corev1.Pod {
	agent := p.agent
	saName := agent.Spec.ServiceAccountName
	automount := saName != "" && p.session
	if saName == "" {
		saName = "default"
	}

	labels := map[string]string{}
	if p.session {
		maps.Copy(labels, agent.Spec.Pod.Labels)
	}
	labels[v1alpha1.LabelManagedBy] = v1alpha1.ManagedByValue
	labels[v1alpha1.LabelAgent] = agent.Name
	maps.Copy(labels, p.labels)

	vols := baseVolumes(p.tokenSecret)
	shared := []corev1.VolumeMount{
		{Name: volWorkspace, MountPath: agentapi.WorkspaceDir},
		{Name: volMise, MountPath: agentapi.MiseDir},
		{Name: volHome, MountPath: agentapi.HomeDir},
		{Name: volTmp, MountPath: "/tmp"},
		{Name: volRun, MountPath: agentapi.RunDir},
		{Name: volToken, MountPath: path.Dir(agentapi.TokenFile), ReadOnly: true},
	}
	initMounts := slices.Clone(shared)
	mainMounts := slices.Clone(shared)

	// Init-only: git auth Secrets and ConfigMap skills.
	if ws := agent.Spec.Workspace; ws != nil && ws.Git != nil && ws.Git.Auth != nil {
		if v, m, ok := authVolume("auth-workspace", agentapi.AuthDir("workspace"), ws.Git.Auth); ok {
			vols = append(vols, v)
			initMounts = append(initMounts, m)
		}
	}
	if p.session {
		for i, sk := range agent.Spec.Skills {
			switch {
			case sk.Git != nil && sk.Git.Auth != nil:
				id := agentapi.SkillIndexID(i)
				if v, m, ok := authVolume("auth-skill-"+id, agentapi.AuthDir(id), sk.Git.Auth); ok {
					vols = append(vols, v)
					initMounts = append(initMounts, m)
				}
			case sk.ConfigMap != nil:
				vn := "skills-cm-" + strconv.Itoa(i)
				vols = append(vols, corev1.Volume{Name: vn, VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: sk.ConfigMap.Name},
						DefaultMode:          new(int32(0o444)),
					},
				}})
				initMounts = append(initMounts, corev1.VolumeMount{
					Name: vn, MountPath: agentapi.ConfigMapSkillDir(i), ReadOnly: true,
				})
			}
		}
	}

	commonEnv := []corev1.EnvVar{
		{Name: p.envName, Value: p.envValue},
		{Name: agentapi.EnvAPI, Value: p.opts.AgentAPIURL()},
		{Name: agentapi.EnvWorkspace, Value: agentapi.WorkspaceDir},
		{Name: "HOME", Value: agentapi.HomeDir},
		{Name: "MISE_DATA_DIR", Value: agentapi.MiseDataDir},
		{Name: "MISE_CACHE_DIR", Value: "/tmp/mise-cache"},
		{Name: "MISE_STATE_DIR", Value: "/tmp/mise-state"},
		{Name: "MISE_TRUSTED_CONFIG_PATHS", Value: agentapi.WorkspaceDir},
		{Name: "MISE_DISABLE_TOOLS", Value: disabledTools(agent)},
		{Name: "MISE_AUTO_INSTALL", Value: miseOff},
		{Name: "MISE_EXEC_AUTO_INSTALL", Value: miseOff},
		{Name: "MISE_NOT_FOUND_AUTO_INSTALL", Value: miseOff},
		{Name: "MISE_YES", Value: "1"},
	}

	mainEnv := slices.Clone(commonEnv)
	var resources corev1.ResourceRequirements
	if p.session {
		// User overrides come first so the operator's variables win on a name clash.
		mainEnv = append(slices.Clone(agent.Spec.Pod.Env), commonEnv...)
		resources = agent.Spec.Pod.Resources

		credEnv, credVols, credMounts := credentials(agent)
		mainEnv = append(mainEnv, credEnv...)
		vols = append(vols, credVols...)
		mainMounts = append(mainMounts, credMounts...)

		mainEnv = append(mainEnv, mcpEnv(agent)...)

		if name, key, ok := gateway.TokenRef(agent, p.opts); ok {
			mainEnv = append(mainEnv, corev1.EnvVar{Name: agentapi.EnvGatewayToken, ValueFrom: secretKeyRef(name, key)})
		}
		if agent.Spec.ServiceAccountName != "" {
			mainEnv = append(mainEnv, corev1.EnvVar{Name: "KUBECONFIG", Value: agentapi.KubeconfigFile})
		}

		vols = append(vols, agent.Spec.Pod.Volumes...)
		mainMounts = append(mainMounts, agent.Spec.Pod.VolumeMounts...)
	}

	image := Image(agent, p.opts)
	secCtx := func() *corev1.SecurityContext {
		return &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: p.namespace, Name: p.name, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        new(p.deadline),
			EnableServiceLinks:           new(false),
			ServiceAccountName:           saName,
			AutomountServiceAccountToken: new(automount),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   new(true),
				RunAsUser:      new(UserID),
				RunAsGroup:     new(UserID),
				FSGroup:        new(UserID),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			InitContainers: []corev1.Container{{
				Name:            containerPrepare,
				Image:           image,
				Command:         []string{DriverBinary},
				Args:            []string{"prepare"},
				WorkingDir:      agentapi.WorkspaceDir,
				Env:             slices.Clone(commonEnv),
				VolumeMounts:    initMounts,
				SecurityContext: secCtx(),
			}},
			Containers: []corev1.Container{{
				Name:            containerAgent,
				Image:           image,
				Command:         []string{DriverBinary},
				Args:            []string{p.mainArg},
				WorkingDir:      agentapi.WorkspaceDir,
				Env:             mainEnv,
				Resources:       resources,
				VolumeMounts:    mainMounts,
				SecurityContext: secCtx(),
			}},
			Volumes: vols,
		},
	}
	if p.session {
		pod.Spec.NodeSelector = maps.Clone(agent.Spec.Pod.NodeSelector)
		pod.Spec.Tolerations = slices.Clone(agent.Spec.Pod.Tolerations)
	}
	return pod
}

func baseVolumes(tokenSecret string) []corev1.Volume {
	empty := func(name, size string) corev1.Volume {
		v := corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
		if size != "" {
			q := resource.MustParse(size)
			v.EmptyDir.SizeLimit = &q
		}
		return v
	}
	return []corev1.Volume{
		empty(volWorkspace, "2Gi"),
		empty(volMise, "2Gi"),
		empty(volHome, "1Gi"),
		empty(volTmp, "1Gi"),
		empty(volRun, ""),
		{Name: volToken, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  tokenSecret,
			DefaultMode: new(int32(0o440)),
			Items:       []corev1.KeyToPath{{Key: agentapi.TokenKey, Path: path.Base(agentapi.TokenFile)}},
		}}},
	}
}

// authVolume mounts a git source's auth keys at dir as one projected volume, one file per
// key. ok is false when auth names no keys.
func authVolume(volName, dir string, auth *v1alpha1.GitAuth) (corev1.Volume, corev1.VolumeMount, bool) {
	var sources []corev1.VolumeProjection
	for _, e := range []struct {
		ref  *v1alpha1.SecretKeyRef
		file string
	}{
		{auth.SSHKeySecretRef, agentapi.AuthSSHKeyFile},
		{auth.KnownHostsSecretRef, agentapi.AuthKnownHostsFile},
		{auth.TokenSecretRef, agentapi.AuthTokenFile},
	} {
		if e.ref != nil {
			sources = append(sources, secretProjection(e.ref, e.file))
		}
	}
	if len(sources) == 0 {
		return corev1.Volume{}, corev1.VolumeMount{}, false
	}
	vol := corev1.Volume{Name: volName, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		Sources:     sources,
		DefaultMode: new(int32(0o440)),
	}}}
	return vol, corev1.VolumeMount{Name: volName, MountPath: dir, ReadOnly: true}, true
}

func secretProjection(ref *v1alpha1.SecretKeyRef, file string) corev1.VolumeProjection {
	return corev1.VolumeProjection{Secret: &corev1.SecretProjection{
		LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
		Items:                []corev1.KeyToPath{{Key: ref.Key, Path: file}},
	}}
}

// credentials renders the agent's credentials: env ones as secretKeyRef variables, file
// ones as one projected volume per directory (items map each key to its basename),
// mounted at that directory.
func credentials(agent *v1alpha1.OmpAgent) (env []corev1.EnvVar, vols []corev1.Volume, mounts []corev1.VolumeMount) {
	var dirs []string
	byDir := map[string][]corev1.VolumeProjection{}
	for i := range agent.Spec.Credentials {
		c := &agent.Spec.Credentials[i]
		switch {
		case c.Env != "":
			env = append(env, corev1.EnvVar{Name: c.Env, ValueFrom: secretKeyRef(c.SecretRef.Name, c.SecretRef.Key)})
		case c.File != "":
			dir, base := path.Split(path.Clean(c.File))
			dir = path.Clean(dir)
			if _, ok := byDir[dir]; !ok {
				dirs = append(dirs, dir)
			}
			byDir[dir] = append(byDir[dir], secretProjection(&c.SecretRef, base))
		}
	}
	for n, dir := range dirs {
		name := "cred-" + strconv.Itoa(n)
		vols = append(vols, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources:     byDir[dir],
			DefaultMode: new(int32(0o440)),
		}}})
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: dir, ReadOnly: true})
	}
	return env, vols, mounts
}

// mcpEnv exposes MCP secret values as agentapi.MCPEnvName(n, m): env entries for stdio
// servers, header entries for http and sse.
func mcpEnv(agent *v1alpha1.OmpAgent) []corev1.EnvVar {
	var out []corev1.EnvVar
	for n := range agent.Spec.MCPServers {
		srv := &agent.Spec.MCPServers[n]
		entries := srv.Headers
		if agentspec.MCPType(srv) == v1alpha1.MCPStdio {
			entries = srv.Env
		}
		for m := range entries {
			if ref := entries[m].SecretRef; ref != nil {
				out = append(out, corev1.EnvVar{Name: agentapi.MCPEnvName(n, m), ValueFrom: secretKeyRef(ref.Name, ref.Key)})
			}
		}
	}
	return out
}

func secretKeyRef(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
		Key:                  key,
	}}
}

func disabledTools(agent *v1alpha1.OmpAgent) string {
	if agent.Spec.Toolchain == nil {
		return ""
	}
	return strings.Join(agent.Spec.Toolchain.Disable, ",")
}

// labelValue fits s into a label value: names over 63 characters are cut and trimmed to end
// alphanumeric.
func labelValue(s string) string {
	if len(s) <= validation.LabelValueMaxLength {
		return s
	}
	return strings.TrimRight(s[:validation.LabelValueMaxLength], "-_.")
}
