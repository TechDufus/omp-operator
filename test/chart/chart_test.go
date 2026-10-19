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

package chart

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

const (
	defaultRuntimeImage = "ghcr.io/techdufus/omp-operator-runtime:v0.0.0"
	rolesName           = "omp-omp-operator-namespaced"
)

func TestDefaultsRBAC(t *testing.T) {
	r := render(t, nil)

	// Cluster-wide rules R, bound to the operator's ServiceAccount.
	var rRole string
	for name, cr := range r.clusterRoles {
		if equalRules(cr.Rules, namespacedRules()) {
			rRole = name
		}
	}
	if rRole == "" {
		t.Fatalf("no ClusterRole carries rules R; have %v", keys(r.clusterRoles))
	}
	crb := r.clusterRoleBindings[rRole]
	if crb == nil {
		t.Fatalf("no ClusterRoleBinding %s; have %v", rRole, keys(r.clusterRoleBindings))
	}
	eq(t, "binding roleRef kind", crb.RoleRef.Kind, "ClusterRole")
	eq(t, "binding roleRef name", crb.RoleRef.Name, rRole)
	eq(t, "binding subjects", len(crb.Subjects), 1)
	eq(t, "binding subject", crb.Subjects[0].Name, operatorName)
	eq(t, "binding subject namespace", crb.Subjects[0].Namespace, releaseNamespace)
	eq(t, "Roles", len(r.roles)+len(r.roleBindings), 0)

	// The manager ClusterRole is the content of the generated files/rules.yaml.
	raw, err := os.ReadFile(filepath.Join(chartDir, "files", "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var generated []rbacv1.PolicyRule
	if err := yaml.Unmarshal(raw, &generated); err != nil {
		t.Fatal(err)
	}
	eq(t, "ClusterRoles", len(r.clusterRoles), 2)
	eq(t, "ClusterRoleBindings", len(r.clusterRoleBindings), 2)
	for name, cr := range r.clusterRoles {
		if name != rRole {
			yes(t, "manager ClusterRole rules equal files/rules.yaml", equalRules(cr.Rules, generated))
		}
	}
}

func TestDefaultsDeployment(t *testing.T) {
	r := render(t, nil)
	d, ok := r.deployments[operatorName]
	if !ok {
		t.Fatal("Deployment omp-operator not rendered")
	}
	eq(t, "replicas", deref(d.Spec.Replicas), 1)
	eq(t, "strategy", d.Spec.Strategy.Type, appsv1.RecreateDeploymentStrategyType)
	eq(t, "name label", d.Labels[nameLabel], operatorName)
	eq(t, "instance label", d.Labels["app.kubernetes.io/instance"], releaseName)
	eq(t, "selector", d.Spec.Selector.MatchLabels[nameLabel], operatorName)

	ps := d.Spec.Template.Spec
	psc := deref(ps.SecurityContext)
	eq(t, "fsGroup", deref(psc.FSGroup), 65532)
	eq(t, "runAsNonRoot", deref(psc.RunAsNonRoot), true)
	eq(t, "fsGroupChangePolicy", deref(psc.FSGroupChangePolicy), corev1.FSGroupChangeOnRootMismatch)
	eq(t, "seccomp", deref(psc.SeccompProfile).Type, corev1.SeccompProfileTypeRuntimeDefault)

	c := ps.Containers[0]
	csc := deref(c.SecurityContext)
	eq(t, "allowPrivilegeEscalation", deref(csc.AllowPrivilegeEscalation), false)
	eq(t, "readOnlyRootFilesystem", deref(csc.ReadOnlyRootFilesystem), true)
	eq(t, "dropped capabilities", len(deref(csc.Capabilities).Drop), 1)
	eq(t, "dropped capability", deref(csc.Capabilities).Drop[0], "ALL")
	eq(t, "image", c.Image, "ghcr.io/techdufus/omp-operator:v0.0.0")
	eqList(t, "env", envPairs(c.Env), []string{"POD_NAMESPACE="})
	eq(t, "POD_NAMESPACE field", c.Env[0].ValueFrom.FieldRef.FieldPath, "metadata.namespace")
	eq(t, "liveness", c.LivenessProbe.HTTPGet.Path, "/healthz")
	eq(t, "readiness", c.ReadinessProbe.HTTPGet.Path, "/readyz")
	eq(t, "cpu request", c.Resources.Requests.Cpu().String(), "50m")
	eq(t, "memory request", c.Resources.Requests.Memory().String(), "128Mi")
	eq(t, "memory limit", c.Resources.Limits.Memory().String(), "512Mi")

	eqList(t, "args", c.Args, []string{
		"--metrics-bind-address=:9090", "--metrics-secure=false", "--health-probe-bind-address=:8081",
		"--web-bind-address=:8080", "--hooks-bind-address=:8082", "--agent-api-bind-address=:8083",
		"--service-name=omp-operator", "--data-dir=/data",
		"--default-runtime-image=" + defaultRuntimeImage,
		"--dashboard-url=", "--hooks-public-url=", "--access-config=/etc/omp-operator/access.yaml",
		"--cloud-max-upload-bytes=1073741824",
	})

	eq(t, "data claim", deref(volumeByName(ps.Volumes, "data").PersistentVolumeClaim).ClaimName, "omp-operator-data")
	yes(t, "tmp is an emptyDir", volumeByName(ps.Volumes, "tmp").EmptyDir != nil)
	mounts := mountPaths(c)
	eq(t, "/data mount", mounts["/data"], "data")
	eq(t, "/tmp mount", mounts["/tmp"], "tmp")
	eq(t, "access mount", mounts["/etc/omp-operator/access.yaml"], "access")
	eq(t, "oidc mount", mounts["/etc/omp-operator/oidc"], "")
	eq(t, "basic mount", mounts["/etc/omp-operator/basic"], "")
}

func TestDefaultsResources(t *testing.T) {
	r := render(t, nil)

	pvc := r.pvcs["omp-operator-data"]
	if pvc == nil {
		t.Fatal("PVC omp-operator-data not rendered")
	}
	want := resource.MustParse("20Gi")
	got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	eq(t, "PVC size", got.Cmp(want), 0)
	eq(t, "storageClassName", deref(pvc.Spec.StorageClassName), "")

	svc := r.services[operatorName]
	if svc == nil {
		t.Fatal("Service omp-operator not rendered")
	}
	ports := map[string]int32{}
	for _, p := range svc.Spec.Ports {
		ports[p.Name] = p.Port
	}
	eq(t, "service ports", len(ports), 4)
	eq(t, "web port", ports["web"], 8080)
	eq(t, "hooks port", ports["hooks"], 8082)
	eq(t, "agent port", ports["agent"], 8083)
	eq(t, "metrics port", ports["metrics"], 9090)
	eq(t, "service selector", svc.Spec.Selector[nameLabel], operatorName)

	yes(t, "access ConfigMap rendered", r.configMaps["omp-operator-access"] != nil)
	yes(t, "no omp-auth Deployment", r.deployments[authName] == nil)
	yes(t, "no omp-auth Service", r.services[authName] == nil)
	yes(t, "no omp-auth PVC", r.pvcs["omp-auth-data"] == nil)
	yes(t, "no NetworkPolicy", !slices.Contains(r.kinds, "NetworkPolicy"))
	yes(t, "no Ingress", !slices.Contains(r.kinds, "Ingress"))
}

func TestImageReferences(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	r := render(t, map[string]any{
		"image":        map[string]any{"repository": "example.com/op", "tag": "v9", "digest": digest},
		"runtimeImage": map[string]any{"repository": "example.com/rt", "tag": "v9"},
	})
	eq(t, "operator image", r.deployments[operatorName].Spec.Template.Spec.Containers[0].Image, "example.com/op@"+digest)
	yes(t, "runtime image flag", slices.Contains(managerArgs(t, r), "--default-runtime-image=example.com/rt:v9"))
}

func TestPersistenceAndScheduling(t *testing.T) {
	r := render(t, map[string]any{
		"persistence":       map[string]any{"storageClass": "fast", "size": "50Gi"},
		"priorityClassName": "important",
		"nodeSelector":      map[string]any{"disk": "ssd"},
		"tolerations":       []any{map[string]any{"key": "dedicated", "operator": "Exists"}},
	})
	pvc := r.pvcs["omp-operator-data"]
	eq(t, "storageClassName", deref(pvc.Spec.StorageClassName), "fast")
	size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	eq(t, "PVC size", size.String(), "50Gi")
	ps := r.deployments[operatorName].Spec.Template.Spec
	eq(t, "priorityClassName", ps.PriorityClassName, "important")
	eq(t, "nodeSelector", ps.NodeSelector["disk"], "ssd")
	eq(t, "tolerations", len(ps.Tolerations), 1)
	eq(t, "toleration key", ps.Tolerations[0].Key, "dedicated")
}

func TestAgentNamespacesRenderRoles(t *testing.T) {
	r := render(t, map[string]any{"rbac": map[string]any{"agentNamespaces": []string{"a", "b"}}})

	for _, ns := range []string{"a", "b", releaseNamespace} {
		role := r.roles[ns+"/"+rolesName]
		if role == nil {
			t.Fatalf("no Role in %s; have %v", ns, keys(r.roles))
		}
		yes(t, "Role rules in "+ns+" are R", equalRules(role.Rules, namespacedRules()))
		rb := r.roleBindings[ns+"/"+rolesName]
		if rb == nil {
			t.Fatalf("no RoleBinding in %s; have %v", ns, keys(r.roleBindings))
		}
		eq(t, "roleRef kind in "+ns, rb.RoleRef.Kind, "Role")
		eq(t, "roleRef name in "+ns, rb.RoleRef.Name, rolesName)
		eq(t, "subjects in "+ns, len(rb.Subjects), 1)
		eq(t, "subject in "+ns, rb.Subjects[0].Name, operatorName)
		eq(t, "subject namespace in "+ns, rb.Subjects[0].Namespace, releaseNamespace)
	}
	eq(t, "Roles", len(r.roles), 3)
	eq(t, "RoleBindings", len(r.roleBindings), 3)

	// No cluster-wide R: only the generated manager ClusterRole and its binding remain.
	for name, cr := range r.clusterRoles {
		yes(t, "ClusterRole "+name+" is not R", !equalRules(cr.Rules, namespacedRules()))
	}
	eq(t, "ClusterRoles", len(r.clusterRoles), 1)
	eq(t, "ClusterRoleBindings", len(r.clusterRoleBindings), 1)
	yes(t, "--agent-namespaces flag", slices.Contains(managerArgs(t, r), "--agent-namespaces=a,b"))
}

func TestAgentNamespacesIncludingReleaseNamespaceHaveNoDuplicates(t *testing.T) {
	r := render(t, map[string]any{"rbac": map[string]any{"agentNamespaces": []string{releaseNamespace, "a"}}})
	eq(t, "Roles", len(r.roles), 2)
	eq(t, "RoleBindings", len(r.roleBindings), 2)
}

func TestActionTargetNamespaces(t *testing.T) {
	wildcard := render(t, map[string]any{"actions": map[string]any{"targetNamespaces": []string{"*"}}})
	yes(t, "wildcard flag", slices.Contains(managerArgs(t, wildcard), "--action-target-namespaces=*"))

	list := render(t, map[string]any{"actions": map[string]any{"targetNamespaces": []string{"a", "b"}}})
	yes(t, "list flag", slices.Contains(managerArgs(t, list), "--action-target-namespaces=a,b"))

	yes(t, "flag absent by default", !hasArgPrefix(managerArgs(t, render(t, nil)), "--action-target-namespaces"))
}
