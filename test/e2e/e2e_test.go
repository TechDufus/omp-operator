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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	bearerToken = "e2e-am-bearer-token"
	hmacKey     = "e2e-gen-hmac-key"
	echoHost    = "http://echo.e2e.svc:5678"

	// alertPayload is an Alertmanager v4 payload; %s lists the alerts.
	alertPayload = `{"version":"4","groupKey":"{}:{alertname=\"E2EHighLoad\"}","status":"firing","receiver":"omp",` +
		`"groupLabels":{"alertname":"E2EHighLoad"},"commonLabels":{"alertname":"E2EHighLoad","severity":"warning"},` +
		`"commonAnnotations":{},"externalURL":"http://alertmanager.example","alerts":[%s]}`
	alertFiring = `{"status":"firing","labels":{"alertname":"E2EHighLoad","severity":"warning","instance":"%s"},` +
		`"annotations":{"summary":"load is high on %[1]s"},"startsAt":"2026-10-18T10:00:00Z","fingerprint":"%[1]s"}`
)

func alert(fingerprint string) string { return fmt.Sprintf(alertFiring, fingerprint) }

var _ = Describe("omp-operator", Ordered, func() {
	var hooks, web, metricsPF *portForward
	var amFirst string // the first alertmanager session

	BeforeAll(func() {
		By("creating the agent's Secrets, action, agent and triggers")
		mustApply(secretYAML(agentNS, "fake-gateway", map[string]string{"token": "e2e-dummy-gateway-token"}))
		mustApply(secretYAML(agentNS, "fake-notify", map[string]string{"url": echoHost + "/sink"}))
		mustApply(secretYAML(agentNS, "am-bearer", map[string]string{"token": bearerToken}))
		mustApply(secretYAML(agentNS, "gen-hmac", map[string]string{"key": hmacKey}))
		mustApply(echoActionYAML)
		mustApply(agentYAML)
		mustApply(amTriggerYAML)
		mustApply(genTriggerYAML)

		expectReady("ompactions", "echo")
		expectReady("ompagents", "fake")
		expectReady("omptriggers", "am")
		expectReady("omptriggers", "gen")
		Expect(str(get("omptriggers", "am"), "status", "hookPath")).To(Equal("/hooks/e2e/am"))

		hooks = startPortForward("omp-operator", 8082)
		web = startPortForward("omp-operator", 8080)
		metricsPF = startPortForward("omp-operator", 9090)
	})

	It("runs a manual session to ok, calls the echo action and notifies the webhook sink", func() {
		mustApply(manualSessionYAML)
		s := waitSession("manual-1")
		Expect(str(s, "status", "verdict")).To(Equal("ok"))

		actions, _, _ := unstructured.NestedSlice(s.Object, "status", "actions")
		Expect(actions).NotTo(BeEmpty())
		first, _ := actions[0].(map[string]any)
		Expect(first["action"]).To(Equal("echo"))
		Expect(first["ok"]).To(BeTrue())

		Eventually(func() string {
			return conditionStatus(get("ompsessions", "manual-1"), "Notified")
		}, time.Minute, 2*time.Second).Should(Equal("True"))
		Eventually(func() string {
			out, _ := run("kubectl", "-n", agentNS, "logs", "deployment/echo")
			return out
		}, time.Minute, 2*time.Second).Should(
			And(ContainSubstring("POST"), ContainSubstring("/echo"), ContainSubstring("/sink")))
	})

	It("creates one session from an alertmanager payload and dedupes the repeat", func() {
		body := fmt.Sprintf(alertPayload, alert("fp-1"))
		hdr := map[string]string{"Authorization": "Bearer " + bearerToken, "Content-Type": "application/json"}

		code, resp, err := httpDo(http.MethodPost, hooks.Base+"/hooks/e2e/am", []byte(body), hdr)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusAccepted), resp)
		Eventually(func() []unstructured.Unstructured { return sessionsOf("am") }, 30*time.Second, time.Second).
			Should(HaveLen(1))

		code, resp, err = httpDo(http.MethodPost, hooks.Base+"/hooks/e2e/am", []byte(body), hdr)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusNoContent), resp)
		Consistently(func() []unstructured.Unstructured { return sessionsOf("am") }, 3*time.Second, time.Second).
			Should(HaveLen(1))

		code, _, err = httpDo(http.MethodPost, hooks.Base+"/hooks/e2e/am", []byte(body), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusUnauthorized))

		amFirst = sessionsOf("am")[0].GetName()
		s := waitSession(amFirst)
		Expect(str(s, "status", "verdict")).To(Equal("ok"))
		Expect(s.GetLabels()["omp.techdufus.com/kind"]).To(Equal("webhook"))
		restored, _, _ := unstructured.NestedBool(s.Object, "status", "continuity", "restored")
		Expect(restored).To(BeFalse())
		stored, _, _ := unstructured.NestedBool(s.Object, "status", "continuity", "stored")
		Expect(stored).To(BeTrue(), "the first episode session must store its continuity")
	})

	It("restores continuity in a later session of the same episode", func() {
		// retriageAfter is 1s and the first session ran for longer, so one new alert opens a session.
		body := fmt.Sprintf(alertPayload, alert("fp-1")+","+alert("fp-2"))
		hdr := map[string]string{"Authorization": "Bearer " + bearerToken, "Content-Type": "application/json"}
		code, resp, err := httpDo(http.MethodPost, hooks.Base+"/hooks/e2e/am", []byte(body), hdr)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusAccepted), resp)

		Eventually(func() []unstructured.Unstructured { return sessionsOf("am") }, 30*time.Second, time.Second).
			Should(HaveLen(2))
		var second string
		for _, s := range sessionsOf("am") {
			if s.GetName() != amFirst {
				second = s.GetName()
			}
		}
		Expect(second).NotTo(BeEmpty())
		s := waitSession(second)
		restored, _, _ := unstructured.NestedBool(s.Object, "status", "continuity", "restored")
		Expect(restored).To(BeTrue(), "session %s should have restored the episode's history", second)
	})

	It("accepts a correctly signed generic webhook and rejects a bad signature", func() {
		body := []byte(`{"id":"gen-1","title":"generic e2e event"}`)
		mac := hmac.New(sha256.New, []byte(hmacKey))
		mac.Write(body)
		sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

		code, resp, err := httpDo(http.MethodPost, hooks.Base+"/hooks/e2e/gen", body,
			map[string]string{"X-Hub-Signature-256": sig, "Content-Type": "application/json"})
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusAccepted), resp)

		code, _, err = httpDo(http.MethodPost, hooks.Base+"/hooks/e2e/gen", body,
			map[string]string{"X-Hub-Signature-256": "sha256=" + strings.Repeat("0", 64)})
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusUnauthorized))

		Eventually(func() []unstructured.Unstructured { return sessionsOf("gen") }, 30*time.Second, time.Second).
			Should(HaveLen(1))
		s := waitSession(sessionsOf("gen")[0].GetName())
		Expect(str(s, "status", "verdict")).To(Equal("ok"))
	})

	It("fires a schedule trigger within two minutes", func() {
		mustApply(cronTriggerYAML)
		expectReady("omptriggers", "cron")
		Eventually(func() []unstructured.Unstructured { return sessionsOf("cron") }, 2*time.Minute, 2*time.Second).
			ShouldNot(BeEmpty())
		first := sessionsOf("cron")[0]
		Expect(first.GetLabels()["omp.techdufus.com/kind"]).To(Equal("schedule"))
		// Stop the schedule so it does not keep creating sessions during the remaining checks.
		mustRun("kubectl", "-n", agentNS, "patch", "omptriggers.omp.techdufus.com", "cron",
			"--type=merge", "-p", `{"spec":{"enabled":false}}`)
		waitSession(first.GetName())
	})

	It("serves the dashboard behind basic auth", func() {
		code, body, err := httpDo(http.MethodGet, web.Base+"/healthz", nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK), body)

		code, _, err = httpDo(http.MethodGet, web.Base+"/", nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusUnauthorized))

		creds := base64.StdEncoding.EncodeToString([]byte(dashUser + ":" + dashPass))
		auth := map[string]string{"Authorization": "Basic " + creds}
		code, body, err = httpDo(http.MethodGet, web.Base+"/", nil, auth)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK), body)
		Expect(body).To(ContainSubstring("manual-1"))

		code, body, err = httpDo(http.MethodGet, web.Base+"/sessions/e2e/manual-1", nil, auth)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK), body)
		Expect(body).To(ContainSubstring("ok"))

		code, _, err = httpDo(http.MethodGet, web.Base+"/sessions/e2e/manual-1/transcript", nil, auth)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK))
	})

	It("exposes session metrics", func() {
		Eventually(func() string {
			_, body, err := httpDo(http.MethodGet, metricsPF.Base+"/metrics", nil, nil)
			if err != nil {
				return err.Error()
			}
			var hits []string
			for line := range strings.SplitSeq(body, "\n") {
				if strings.HasPrefix(line, "omp_operator_sessions_total{") && strings.Contains(line, `verdict="ok"`) {
					hits = append(hits, line)
				}
			}
			return strings.Join(hits, "\n")
		}, time.Minute, 2*time.Second).ShouldNot(BeEmpty())
	})
})

const echoActionYAML = `apiVersion: omp.techdufus.com/v1alpha1
kind: OmpAction
metadata:
  name: echo
  namespace: e2e
spec:
  description: Send a message to the echo server.
  httpRequest:
    method: POST
    url: ` + echoHost + `/echo
    allowedURLPrefix: ` + echoHost + `/
    body: '{{ json .Args }}'
    parameters:
      type: object
      required: [msg]
      additionalProperties: false
      properties:
        msg: {type: string}
`

// fakeompScript calls the echo action, then finishes ok.
const fakeompScript = `{"calls":[{"tool":"echo","args":{"msg":"hello from fakeomp"}}],` +
	`"finish":{"verdict":"ok","title":"e2e session","summary":"fakeomp finished","report":"fakeomp report"}}`

const agentYAML = `apiVersion: omp.techdufus.com/v1alpha1
kind: OmpAgent
metadata:
  name: fake
  namespace: e2e
spec:
  model:
    default: fake/fake-model
    gateway:
      url: http://omp-auth.omp-system.svc:4000
      tokenSecretRef: {name: fake-gateway, key: token}
  actions:
    - name: echo
  notify:
    - type: webhook
      webhook:
        urlSecretRef: {name: fake-notify, key: url}
  pod:
    env:
      - name: FAKEOMP_SCRIPT
        value: '` + fakeompScript + `'
`

const amTriggerYAML = `apiVersion: omp.techdufus.com/v1alpha1
kind: OmpTrigger
metadata:
  name: am
  namespace: e2e
spec:
  agentRef: {name: fake}
  session:
    resume: episode
  webhook:
    adapter: alertmanager
    auth:
      bearer:
        secretRef: {name: am-bearer, key: token}
    grouping:
      retriageAfter: 1s
  prompt: |
    Alert group {{ .Event.Name }} is {{ .Event.Status }} ({{ .Event.Severity }}); reason: {{ .Reason }}.
    Previous: {{ .Previous }}
    {{ .Alerts }}
`

const genTriggerYAML = `apiVersion: omp.techdufus.com/v1alpha1
kind: OmpTrigger
metadata:
  name: gen
  namespace: e2e
spec:
  agentRef: {name: fake}
  webhook:
    adapter: generic
    auth:
      hmac:
        secretRef: {name: gen-hmac, key: key}
    mapping:
      key: payload.id
      title: payload.title
  prompt: |
    Event {{ .Event.Name }} ({{ .Event.Key }}).
    {{ .Payload }}
`

const cronTriggerYAML = `apiVersion: omp.techdufus.com/v1alpha1
kind: OmpTrigger
metadata:
  name: cron
  namespace: e2e
spec:
  agentRef: {name: fake}
  schedule:
    cron: '* * * * *'
  prompt: |
    Scheduled run at {{ .Now }}. Input: {{ .Input }}
`

const manualSessionYAML = `apiVersion: omp.techdufus.com/v1alpha1
kind: OmpSession
metadata:
  name: manual-1
  namespace: e2e
spec:
  agentRef: {name: fake}
  prompt: Say hello through the echo action, then call finish with verdict ok.
`

func echoYAML(image string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
  namespace: e2e
spec:
  replicas: 1
  selector:
    matchLabels: {app: echo}
  template:
    metadata:
      labels: {app: echo}
    spec:
      containers:
        - name: echo
          image: ` + image + `
          args: ["-listen=:5678", "-text=ok"]
          ports:
            - containerPort: 5678
          readinessProbe:
            httpGet: {path: /, port: 5678}
---
apiVersion: v1
kind: Service
metadata:
  name: echo
  namespace: e2e
spec:
  selector: {app: echo}
  ports:
    - port: 5678
      targetPort: 5678
`
}
