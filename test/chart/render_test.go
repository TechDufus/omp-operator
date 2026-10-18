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

// Package chart renders charts/omp-operator with `helm template` and asserts the output.
// helm must be on PATH (it is under `mise run test`).
package chart

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const (
	releaseNamespace = "omp-system"
	releaseName      = "omp"
	chartDir         = "../../charts/omp-operator"
	operatorName     = "omp-operator"
	authName         = "omp-auth"
	nameLabel        = "app.kubernetes.io/name"
	verbGet          = "get"
)

// rendered holds the typed objects of one `helm template` run. Namespaced objects are keyed by
// "<namespace>/<name>", cluster-scoped ones by name.
type rendered struct {
	deployments         map[string]*appsv1.Deployment
	services            map[string]*corev1.Service
	pvcs                map[string]*corev1.PersistentVolumeClaim
	configMaps          map[string]*corev1.ConfigMap
	clusterRoles        map[string]*rbacv1.ClusterRole
	clusterRoleBindings map[string]*rbacv1.ClusterRoleBinding
	roles               map[string]*rbacv1.Role
	roleBindings        map[string]*rbacv1.RoleBinding
	kinds               []string
}

type helmError struct {
	err    error
	stderr string
}

func (e *helmError) Error() string { return e.err.Error() + ": " + e.stderr }

func helm(t *testing.T, values map[string]any) (string, error) {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err != nil {
		t.Fatalf("helm is not on PATH (run under `mise exec`): %v", err)
	}
	args := []string{"template", releaseName, chartDir, "--namespace", releaseNamespace}
	if values != nil {
		raw, err := yaml.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(file, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-f", file)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(path, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", &helmError{err: err, stderr: stderr.String()}
	}
	return stdout.String(), nil
}

func render(t *testing.T, values map[string]any) *rendered {
	t.Helper()
	out, err := helm(t, values)
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	r := &rendered{
		deployments:         map[string]*appsv1.Deployment{},
		services:            map[string]*corev1.Service{},
		pvcs:                map[string]*corev1.PersistentVolumeClaim{},
		configMaps:          map[string]*corev1.ConfigMap{},
		clusterRoles:        map[string]*rbacv1.ClusterRole{},
		clusterRoleBindings: map[string]*rbacv1.ClusterRoleBinding{},
		roles:               map[string]*rbacv1.Role{},
		roleBindings:        map[string]*rbacv1.RoleBinding{},
	}
	for doc := range strings.SplitSeq(out, "\n---\n") {
		r.add(t, doc)
	}
	return r
}

func (r *rendered) add(t *testing.T, doc string) {
	t.Helper()
	var meta struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
		t.Fatalf("decode %q: %v", doc, err)
	}
	if meta.Kind == "" {
		return // a document holding only comments
	}
	r.kinds = append(r.kinds, meta.Kind)
	name := meta.Metadata.Name
	nsName := meta.Metadata.Namespace + "/" + name
	switch meta.Kind {
	case "Deployment":
		r.deployments[name] = decode[appsv1.Deployment](t, doc)
	case "Service":
		r.services[name] = decode[corev1.Service](t, doc)
	case "PersistentVolumeClaim":
		r.pvcs[name] = decode[corev1.PersistentVolumeClaim](t, doc)
	case "ConfigMap":
		r.configMaps[name] = decode[corev1.ConfigMap](t, doc)
	case "ClusterRole":
		r.clusterRoles[name] = decode[rbacv1.ClusterRole](t, doc)
	case "ClusterRoleBinding":
		r.clusterRoleBindings[name] = decode[rbacv1.ClusterRoleBinding](t, doc)
	case "Role":
		r.roles[nsName] = decode[rbacv1.Role](t, doc)
	case "RoleBinding":
		r.roleBindings[nsName] = decode[rbacv1.RoleBinding](t, doc)
	case "ServiceAccount":
	default:
		t.Errorf("unexpected rendered kind %s", meta.Kind)
	}
}

func decode[T any](t *testing.T, doc string) *T {
	t.Helper()
	var v T
	if err := yaml.UnmarshalStrict([]byte(doc), &v); err != nil {
		t.Fatalf("decode %T: %v\n%s", v, err, doc)
	}
	return &v
}

// eq fails the test when got differs from want.
func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// eqList fails the test when the lists differ.
func eqList[T comparable](t *testing.T, what string, got, want []T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// yes fails the test when ok is false.
func yes(t *testing.T, what string, ok bool) {
	t.Helper()
	if !ok {
		t.Errorf("%s: want true", what)
	}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func volumeByName(vols []corev1.Volume, name string) *corev1.Volume {
	for i := range vols {
		if vols[i].Name == name {
			return &vols[i]
		}
	}
	return &corev1.Volume{}
}

func mountPaths(c corev1.Container) map[string]string {
	out := map[string]string{}
	for _, m := range c.VolumeMounts {
		out[m.MountPath] = m.Name
	}
	return out
}

func envPairs(env []corev1.EnvVar) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		out = append(out, e.Name+"="+e.Value)
	}
	return out
}

func managerArgs(t *testing.T, r *rendered) []string {
	t.Helper()
	d, ok := r.deployments[operatorName]
	if !ok {
		t.Fatal("Deployment omp-operator not rendered")
	}
	eq(t, "operator containers", len(d.Spec.Template.Spec.Containers), 1)
	return d.Spec.Template.Spec.Containers[0].Args
}

func hasArgPrefix(args []string, prefix string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, prefix) })
}

// namespacedRules are the plan's rules R.
func namespacedRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods"},
			Verbs: []string{verbGet, "list", "watch", "create", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"},
			Verbs: []string{verbGet, "create", "update", "patch", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{verbGet}},
	}
}

func equalRules(a, b []rbacv1.PolicyRule) bool {
	return slices.EqualFunc(a, b, func(x, y rbacv1.PolicyRule) bool {
		return slices.Equal(x.APIGroups, y.APIGroups) && slices.Equal(x.Resources, y.Resources) &&
			slices.Equal(x.Verbs, y.Verbs) && slices.Equal(x.ResourceNames, y.ResourceNames)
	})
}
