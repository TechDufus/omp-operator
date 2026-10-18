//go:build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/crypto/bcrypt"
)

const (
	systemNS  = "omp-system" // the operator's namespace
	agentNS   = "e2e"        // the only namespace the operator manages agents in
	release   = "omp"
	dashUser  = "e2e"
	dashPass  = "e2e-dashboard-pass"
	htpasswdS = "omp-dashboard-basic"
)

var (
	// Images and cluster are chosen by `mise run test-e2e`; the defaults match its own.
	operatorImage = envOr("IMG", "omp-operator:dev")
	e2eImage      = envOr("E2E_IMG", "omp-operator-e2e:dev")
	kindCluster   = envOr("KIND_CLUSTER", "omp-operator-e2e")

	projectDir string
	echoImage  string // hashicorp/http-echo pinned by digest
)

// TestE2E runs the e2e suite against the kind cluster that `mise run test-e2e` created.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting omp-operator e2e suite\n")
	RunSpecs(t, "omp-operator e2e suite")
}

var _ = BeforeSuite(func() {
	var err error
	projectDir, err = findProjectDir()
	Expect(err).NotTo(HaveOccurred())

	By("resolving the pinned http-echo image")
	echoImage, err = resolveEchoImage()
	Expect(err).NotTo(HaveOccurred())
	_, _ = fmt.Fprintf(GinkgoWriter, "echo image: %s\n", echoImage)

	By("loading the operator and e2e runtime images into kind")
	for _, img := range []string{operatorImage, e2eImage} {
		mustRun("kind", "load", "docker-image", img, "--name", kindCluster)
	}

	By("creating the namespaces")
	for _, ns := range []string{systemNS, agentNS} {
		mustApply(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", ns))
	}

	By("creating the dashboard basic-auth Secret")
	hash, err := bcrypt.GenerateFromPassword([]byte(dashPass), bcrypt.MinCost)
	Expect(err).NotTo(HaveOccurred())
	mustApply(secretYAML(systemNS, htpasswdS, map[string]string{"htpasswd": dashUser + ":" + string(hash) + "\n"}))

	By("installing the chart")
	values := map[string]any{
		"image":        imageValues(operatorImage),
		"runtimeImage": imageValues(e2eImage),
		"rbac":         map[string]any{"agentNamespaces": []string{agentNS}},
		"dashboard": map[string]any{
			"basic": map[string]any{"existingSecret": htpasswdS},
			"access": []map[string]any{{
				"users":      []string{"basic:" + dashUser},
				"namespaces": []string{"*"},
				"role":       "viewer",
			}},
		},
		"persistence": map[string]any{"size": "1Gi"},
	}
	raw, err := json.Marshal(values)
	Expect(err).NotTo(HaveOccurred())
	valuesFile := filepath.Join(GinkgoT().TempDir(), "values.json")
	Expect(os.WriteFile(valuesFile, raw, 0o600)).To(Succeed())
	mustRun("helm", "upgrade", "--install", release, filepath.Join(projectDir, "charts", "omp-operator"),
		"--namespace", systemNS, "--values", valuesFile, "--wait", "--timeout", "5m")

	By("deploying the echo server")
	mustApply(echoYAML(echoImage))
	mustRun("kubectl", "-n", agentNS, "rollout", "status", "deployment/echo", "--timeout=3m")
})

var _ = AfterSuite(func() {
	// The operator must still run while the namespace goes away so it can clear session finalizers.
	for _, step := range [][]string{
		{"kubectl", "delete", "namespace", agentNS, "--wait=true", "--timeout=3m", "--ignore-not-found"},
		{"helm", "uninstall", release, "--namespace", systemNS, "--wait", "--timeout", "3m"},
		{"kubectl", "delete", "namespace", systemNS, "--wait=false", "--ignore-not-found"},
	} {
		if out, err := run(step[0], step[1:]...); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "teardown %s: %v\n%s\n", strings.Join(step, " "), err, out)
		}
	}
})

// ReportAfterEach dumps what is needed to debug a failure: operator logs, pods, events and the CRs.
var _ = ReportAfterEach(func(report SpecReport) {
	if !report.Failed() {
		return
	}
	dumpDiagnostics()
})

func imageValues(ref string) map[string]any {
	repo, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	return map[string]any{"repository": repo, "tag": tag, "pullPolicy": "IfNotPresent"}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func findProjectDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}
