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
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const apiGroup = "omp.techdufus.com"

// run executes a command in the project root and returns its combined output.
func run(name string, args ...string) (string, error) {
	return runStdin("", name, args...)
}

func runStdin(stdin, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = projectDir
	// Isolate from the user's kuberc so kubectl behaves the same everywhere.
	cmd.Env = append(cmd.Environ(), "KUBECTL_KUBERC=false")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %s %s\n", name, strings.Join(args, " "))
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return out.String(), err
}

func mustRun(name string, args ...string) string {
	GinkgoHelper()
	out, err := run(name, args...)
	Expect(err).NotTo(HaveOccurred())
	return out
}

func mustApply(manifest string) {
	GinkgoHelper()
	_, err := runStdin(manifest, "kubectl", "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
}

func secretYAML(ns, name string, data map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\ndata:\n",
		name, ns)
	for _, k := range slices.Sorted(maps.Keys(data)) {
		fmt.Fprintf(&b, "  %s: %s\n", k, base64.StdEncoding.EncodeToString([]byte(data[k])))
	}
	return b.String()
}

// get returns one object as unstructured, or nil when it does not exist.
func get(resource, name string) *unstructured.Unstructured {
	GinkgoHelper()
	out, err := run("kubectl", "-n", agentNS, "get", resource+"."+apiGroup, name, "-o", "json", "--ignore-not-found")
	Expect(err).NotTo(HaveOccurred())
	if strings.TrimSpace(out) == "" {
		return nil
	}
	return decode(out)
}

func decode(raw string) *unstructured.Unstructured {
	GinkgoHelper()
	u := &unstructured.Unstructured{}
	Expect(json.Unmarshal([]byte(raw), &u.Object)).To(Succeed())
	return u
}

// list returns the objects of a resource matching a label selector.
func list(resource, ns, selector string) []unstructured.Unstructured {
	GinkgoHelper()
	args := []string{"-n", ns, "get", resource + "." + apiGroup, "-o", "json"}
	if selector != "" {
		args = append(args, "-l", selector)
	}
	out, err := run("kubectl", args...)
	Expect(err).NotTo(HaveOccurred())
	l := &unstructured.UnstructuredList{}
	Expect(json.Unmarshal([]byte(out), &l.Object)).To(Succeed())
	items, _, _ := unstructured.NestedSlice(l.Object, "items")
	res := make([]unstructured.Unstructured, 0, len(items))
	for _, it := range items {
		res = append(res, unstructured.Unstructured{Object: it.(map[string]any)})
	}
	return res
}

func str(u *unstructured.Unstructured, fields ...string) string {
	if u == nil {
		return ""
	}
	s, _, _ := unstructured.NestedString(u.Object, fields...)
	return s
}

func conditionStatus(u *unstructured.Unstructured, condType string) string {
	if u == nil {
		return ""
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == condType {
			s, _ := m["status"].(string)
			return s
		}
	}
	return ""
}

func expectReady(resource, name string) {
	GinkgoHelper()
	Eventually(func() string {
		return conditionStatus(get(resource, name), "Ready")
	}, 2*time.Minute, 2*time.Second).Should(Equal("True"), "%s/%s never became Ready", resource, name)
}

// sessionsOf lists the sessions of one trigger (label omp.techdufus.com/trigger).
func sessionsOf(trigger string) []unstructured.Unstructured {
	GinkgoHelper()
	return list("ompsessions", agentNS, apiGroup+"/trigger="+trigger)
}

func waitSession(name string) *unstructured.Unstructured {
	GinkgoHelper()
	var s *unstructured.Unstructured
	Eventually(func() string {
		s = get("ompsessions", name)
		switch phase := str(s, "status", "phase"); phase {
		case "Failed":
			StopTrying(fmt.Sprintf("session %s Failed: %s", name, str(s, "status", "error"))).Now()
		default:
			return phase
		}
		return ""
	}, 4*time.Minute, 3*time.Second).Should(Equal("Succeeded"))
	return s
}

// portForward is a running `kubectl port-forward` to a Service port.
type portForward struct {
	cmd  *exec.Cmd
	Base string // http://127.0.0.1:<port>
}

var forwardLine = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

func startPortForward(svc string, port int) *portForward {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "-n", systemNS, "port-forward", "svc/"+svc, fmt.Sprintf(":%d", port))
	cmd.Dir = projectDir
	stdout, err := cmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	cmd.Stderr = GinkgoWriter
	Expect(cmd.Start()).To(Succeed())
	pf := &portForward{cmd: cmd}
	DeferCleanup(pf.stop)

	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := forwardLine.FindStringSubmatch(sc.Text()); m != nil {
				found <- m[1]
				break
			}
		}
		_, _ = io.Copy(io.Discard, stdout) // keep draining so kubectl never blocks
	}()
	select {
	case p := <-found:
		pf.Base = "http://127.0.0.1:" + p
	case <-time.After(30 * time.Second):
		Fail("port-forward to svc/" + svc + " did not come up")
	}
	return pf
}

func (p *portForward) stop() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	}
}

// httpDo sends a request without following redirects and returns status and body.
func httpDo(method, url string, body []byte, headers map[string]string) (int, string, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// resolveEchoImage resolves hashicorp/http-echo:1.0 to its manifest-list digest on Docker Hub,
// so the deployed reference is pinned by digest.
func resolveEchoImage() (string, error) {
	const repo, tag = "hashicorp/http-echo", "1.0"
	code, body, err := httpDo(http.MethodGet,
		"https://auth.docker.io/token?service=registry.docker.io&scope=repository:"+repo+":pull", nil, nil)
	if err != nil || code != http.StatusOK {
		return "", fmt.Errorf("registry token: status %d: %w", code, err)
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &tok); err != nil || tok.Token == "" {
		return "", fmt.Errorf("registry token response: %w", err)
	}
	req, err := http.NewRequest(http.MethodHead, "https://registry-1.docker.io/v2/"+repo+"/manifests/"+tag, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
	}, ", "))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	digest := resp.Header.Get("Docker-Content-Digest")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("resolving %s:%s: status %d digest %q", repo, tag, resp.StatusCode, digest)
	}
	return repo + "@" + digest, nil
}

// dumpDiagnostics prints operator logs, pod descriptions, events and the custom resources.
func dumpDiagnostics() {
	section := func(title, out string, err error) {
		_, _ = fmt.Fprintf(GinkgoWriter, "\n===== %s =====\n%s\n", title, out)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "(error: %v)\n", err)
		}
	}
	out, err := run("kubectl", "-n", systemNS, "logs", "deployment/omp-operator", "--tail=500")
	section("operator logs", out, err)
	out, err = run("kubectl", "-n", systemNS, "describe", "pods")
	section("pods in "+systemNS, out, err)
	out, err = run("kubectl", "-n", agentNS, "describe", "pods")
	section("pods in "+agentNS, out, err)
	out, err = run("kubectl", "-n", agentNS, "logs", "deployment/echo", "--tail=200")
	section("echo server logs", out, err)
	out, err = run("kubectl", "-n", agentNS, "logs", "-l", "app.kubernetes.io/managed-by=omp-operator",
		"--all-containers", "--prefix", "--tail=100", "--ignore-errors")
	section("operator-managed pod logs", out, err)
	out, err = run("kubectl", "-n", agentNS, "get", "events", "--sort-by=.lastTimestamp")
	section("events in "+agentNS, out, err)
	for _, r := range []string{"ompagents", "omptriggers", "ompactions", "ompsessions"} {
		out, err = run("kubectl", "-n", agentNS, "get", r+"."+apiGroup, "-o", "yaml")
		section(r, out, err)
	}
}
