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
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func gatewayValues() map[string]any {
	return map[string]any{"authGateway": map[string]any{
		"enabled": true, "clientsSecret": "my-clients",
		"persistence": map[string]any{"storageClass": "slow", "size": "2Gi"},
	}}
}

func renderGateway(t *testing.T) (*rendered, corev1.PodSpec) {
	t.Helper()
	r := render(t, gatewayValues())
	d := r.deployments[authName]
	if d == nil {
		t.Fatal("Deployment omp-auth not rendered")
	}
	eq(t, "strategy", d.Spec.Strategy.Type, appsv1.RecreateDeploymentStrategyType)
	eq(t, "name label", d.Labels[nameLabel], authName)
	eq(t, "selector", d.Spec.Selector.MatchLabels[nameLabel], authName)
	eq(t, "pod label", d.Spec.Template.Labels[nameLabel], authName)
	return r, d.Spec.Template.Spec
}

func commandLine(c corev1.Container) string {
	return strings.Join(append(slices.Clone(c.Command), c.Args...), " ")
}

func TestAuthGatewayPod(t *testing.T) {
	_, ps := renderGateway(t)
	psc := deref(ps.SecurityContext)
	eq(t, "fsGroup", deref(psc.FSGroup), 10001)
	eq(t, "runAsUser", deref(psc.RunAsUser), 10001)
	eq(t, "runAsNonRoot", deref(psc.RunAsNonRoot), true)

	eq(t, "initContainers", len(ps.InitContainers), 1)
	eq(t, "containers", len(ps.Containers), 1)
	broker, gw := ps.InitContainers[0], ps.Containers[0]
	eq(t, "gateway image", gw.Image, defaultRuntimeImage)
	eq(t, "broker image", broker.Image, defaultRuntimeImage)
	for _, c := range []corev1.Container{broker, gw} {
		sc := deref(c.SecurityContext)
		eq(t, c.Name+" allowPrivilegeEscalation", deref(sc.AllowPrivilegeEscalation), false)
		eq(t, c.Name+" readOnlyRootFilesystem", deref(sc.ReadOnlyRootFilesystem), true)
		mounts := mountPaths(c)
		eq(t, c.Name+" /data mount", mounts["/data"], "data")
		eq(t, c.Name+" /tmp mount", mounts["/tmp"], "tmp")
		yes(t, c.Name+" HOME", slices.Contains(envPairs(c.Env), "HOME=/data"))
	}
	eq(t, "data claim", deref(volumeByName(ps.Volumes, "data").PersistentVolumeClaim).ClaimName, "omp-auth-data")
	yes(t, "tmp is an emptyDir", volumeByName(ps.Volumes, "tmp").EmptyDir != nil)
}

func TestAuthGatewayNativeSidecarBroker(t *testing.T) {
	_, ps := renderGateway(t)
	broker := ps.InitContainers[0]
	eq(t, "restartPolicy", deref(broker.RestartPolicy), corev1.ContainerRestartPolicyAlways)
	eq(t, "command", commandLine(broker), "omp auth-broker serve --bind=127.0.0.1:8765")

	probe := deref(broker.StartupProbe)
	eq(t, "startupProbe period", probe.PeriodSeconds, 2)
	eq(t, "startupProbe failureThreshold", probe.FailureThreshold, 30)
	eqList(t, "startupProbe command", deref(probe.Exec).Command,
		[]string{"sh", "-c", "find /data -name auth-broker.token -size +0 | grep -q ."})
}

func TestAuthGatewayContainer(t *testing.T) {
	r, ps := renderGateway(t)
	gw := ps.Containers[0]
	eq(t, "name", gw.Name, "gateway")
	eq(t, "command", commandLine(gw),
		"omp auth-gateway serve --bind=0.0.0.0:4000 --clients=/etc/omp-auth/clients.json")
	yes(t, "broker URL env", slices.Contains(envPairs(gw.Env), "OMP_AUTH_BROKER_URL=http://127.0.0.1:8765"))
	eq(t, "readiness path", deref(gw.ReadinessProbe).HTTPGet.Path, "/healthz")
	eq(t, "readiness port", deref(gw.ReadinessProbe).HTTPGet.Port.String(), "gateway")
	eq(t, "ports", len(gw.Ports), 1)
	eq(t, "containerPort", gw.Ports[0].ContainerPort, 4000)

	// The clients Secret is mounted as a whole directory and must exist.
	clients := volumeByName(ps.Volumes, "clients")
	if clients.Secret == nil {
		t.Fatalf("clients volume: %+v", clients)
	}
	eq(t, "clients secret", clients.Secret.SecretName, "my-clients")
	if clients.Secret.Optional == nil {
		t.Fatal("clients Secret must set optional: false explicitly")
	}
	eq(t, "clients optional", *clients.Secret.Optional, false)
	for _, m := range gw.VolumeMounts {
		if m.Name == "clients" {
			eq(t, "clients mountPath", m.MountPath, "/etc/omp-auth")
			eq(t, "clients subPath", m.SubPath+m.SubPathExpr, "")
		}
	}
	eq(t, "clients mount", mountPaths(gw)["/etc/omp-auth"], "clients")

	svc := r.services[authName]
	if svc == nil {
		t.Fatal("Service omp-auth not rendered")
	}
	eq(t, "service ports", len(svc.Spec.Ports), 1)
	eq(t, "service port", svc.Spec.Ports[0].Port, 4000)
	eq(t, "service selector", svc.Spec.Selector[nameLabel], authName)
}

func TestAuthGatewayStorageAndOperatorFlag(t *testing.T) {
	r := render(t, gatewayValues())
	pvc := r.pvcs["omp-auth-data"]
	if pvc == nil {
		t.Fatal("PVC omp-auth-data not rendered")
	}
	eq(t, "storageClassName", deref(pvc.Spec.StorageClassName), "slow")
	size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	eq(t, "size", size.String(), "2Gi")

	yes(t, "--gateway-clients-secret", slices.Contains(managerArgs(t, r), "--gateway-clients-secret=my-clients"))
	yes(t, "flag absent without the gateway",
		!hasArgPrefix(managerArgs(t, render(t, nil)), "--gateway-clients-secret"))
}
