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

package podspec

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/agentspec"
	"github.com/TechDufus/omp-operator/internal/cloudspec"
)

const (
	// ThreadGracePeriodSeconds is the termination grace period of a thread sitting: the
	// driver saves the workspace and conversation on SIGTERM.
	ThreadGracePeriodSeconds int64 = 60

	// ContainerGitpush is the name of the native sidecar that pushes the result branch.
	ContainerGitpush = "gitpush"

	// GitpushAddr is where the gitpush sidecar listens, as the agent container sees it.
	GitpushAddr = "127.0.0.1:8090"

	// tmpDir is the writable scratch directory every container has.
	tmpDir = "/tmp"

	volThreadSecrets = "thread-secrets"
	volCloudGitAuth  = "auth-cloud-git"
	volGitpushTmp    = "gitpush-tmp"
)

// Environment variables of the gitpush sidecar; the driver's gitpush command reads them.
const (
	EnvGitpushAddr      = "OMP_GITPUSH_ADDR"
	EnvGitpushURL       = "OMP_GITPUSH_URL"
	EnvGitpushBranch    = "OMP_GITPUSH_BRANCH"
	EnvGitpushWorkspace = "OMP_GITPUSH_WORKSPACE"
	EnvGitpushAuth      = "OMP_GITPUSH_AUTH"
	EnvGitpushAuthDir   = "OMP_GITPUSH_AUTHDIR"
	EnvGitpushUsername  = "OMP_GITPUSH_USERNAME"
)

// emptyDir size limits of a pod: workspace, mise, home and tmp.
type sizes struct{ workspace, mise, home, tmp string }

var (
	sessionSizes = sizes{workspace: "2Gi", mise: "2Gi", home: "1Gi", tmp: "1Gi"}
	threadSizes  = sizes{workspace: "20Gi", mise: "10Gi", home: "10Gi", tmp: "5Gi"}
)

// threadParts is what a thread sitting adds to a pod: the shipped files and secrets, the matched
// git host's auth for prepare and the gitpush sidecar.
type threadParts struct {
	thread *v1alpha1.OmpThread

	// Volumes of the Secrets only prepare (and the sidecar) may read.
	vols       []corev1.Volume
	initMounts []corev1.VolumeMount

	// mainEnv is the thread's secret environment for the agent container.
	mainEnv []corev1.EnvVar

	// push is true when the matched host has push set; the sidecar then exists.
	push      bool
	fetchURL  string
	authMount corev1.VolumeMount // zero without auth
	hasAuth   bool
	authType  string
	username  string
}

func newThreadParts(agent *v1alpha1.OmpAgent, thread *v1alpha1.OmpThread) *threadParts {
	t := &threadParts{thread: thread}

	host, fetchURL, ok := cloudspec.MatchGitHost(cloudOf(agent).Git, thread.Spec.Repo.RemoteURL)
	if ok {
		t.fetchURL = fetchURL
		if v, m, has := authVolume(volCloudGitAuth, agentapi.AuthDir(agentapi.CloudGitAuthID), &host.Auth); has {
			t.vols = append(t.vols, v)
			t.initMounts = append(t.initMounts, m)
			t.authMount, t.hasAuth = m, true
			_, t.authType, t.username = agentapi.GitAuthFor(&host.Auth, agentapi.CloudGitAuthID)
		}
		t.push = host.Push && t.hasAuth && fetchURL != ""
	}

	secrets := agentapi.ThreadSecretsName(thread.Name)
	if n := len(thread.Spec.SecretFiles); n > 0 {
		items := make([]corev1.KeyToPath, n)
		for i := range items {
			key := agentapi.ThreadSecretFileKey(i)
			items[i] = corev1.KeyToPath{Key: key, Path: key}
		}
		t.vols = append(t.vols, corev1.Volume{Name: volThreadSecrets, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: secrets, DefaultMode: new(int32(0o440)), Items: items},
		}})
		t.initMounts = append(t.initMounts, corev1.VolumeMount{
			Name: volThreadSecrets, MountPath: agentapi.ThreadSecretsDir, ReadOnly: true,
		})
	}
	for _, name := range thread.Spec.SecretEnv {
		// The API validates names; a reserved or malformed one would override the operator's own
		// variables or break the pod, so it is dropped here too.
		if agentspec.CheckEnvName(name) != "" {
			continue
		}
		t.mainEnv = append(t.mainEnv, corev1.EnvVar{
			Name: name, ValueFrom: secretKeyRef(secrets, agentapi.ThreadSecretEnvKey(name)),
		})
	}
	if t.push {
		t.mainEnv = append(t.mainEnv, corev1.EnvVar{Name: EnvGitpushAddr, Value: GitpushAddr})
	}
	return t
}

// gitpush is the native sidecar that pushes the result branch to the thread's remote. It holds
// the git credential; the agent container has none.
func (t *threadParts) gitpush(image string, sec *corev1.SecurityContext, workspace string) corev1.Container {
	return corev1.Container{
		Name:          ContainerGitpush,
		Image:         image,
		Command:       []string{DriverBinary},
		Args:          []string{"gitpush"},
		RestartPolicy: new(corev1.ContainerRestartPolicyAlways),
		Env: []corev1.EnvVar{
			{Name: EnvGitpushURL, Value: t.fetchURL},
			{Name: EnvGitpushBranch, Value: cloudspec.Branch(t.thread.Name)},
			{Name: EnvGitpushWorkspace, Value: workspace},
			{Name: EnvGitpushAuth, Value: t.authType},
			{Name: EnvGitpushAuthDir, Value: agentapi.AuthDir(agentapi.CloudGitAuthID)},
			{Name: EnvGitpushUsername, Value: t.username},
			{Name: "HOME", Value: tmpDir},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: volWorkspace, MountPath: workspace, ReadOnly: true},
			t.authMount,
			{Name: volGitpushTmp, MountPath: tmpDir},
		},
		SecurityContext: sec,
	}
}

// gitpushVolume is the sidecar's scratch directory.
func gitpushVolume() corev1.Volume {
	return corev1.Volume{Name: volGitpushTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}
