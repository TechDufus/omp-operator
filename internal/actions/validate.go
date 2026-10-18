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

package actions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"text/template"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

// Defaults applied in code when the CRD defaults are absent (for example in unit tests).
const (
	defaultMaxPerSession    = 3
	defaultCooldown         = 6 * time.Hour
	defaultWaitTimeout      = 5 * time.Minute
	defaultMaxSilence       = 24 * time.Hour
	defaultResponseMaxBytes = 65536
)

func maxPerSession(s *v1alpha1.OmpActionSpec) int {
	if s.MaxPerSession > 0 {
		return int(s.MaxPerSession)
	}
	return defaultMaxPerSession
}

func durationOr(d metav1.Duration, def time.Duration) time.Duration {
	if d.Duration > 0 {
		return d.Duration
	}
	return def
}

func restartKinds(a *v1alpha1.KubernetesRolloutRestartAction) []v1alpha1.WorkloadKind {
	if len(a.Kinds) > 0 {
		return a.Kinds
	}
	return []v1alpha1.WorkloadKind{v1alpha1.WorkloadDeployment}
}

func fluxKinds(a *v1alpha1.FluxReconcileAction) []v1alpha1.FluxKind {
	if len(a.Kinds) > 0 {
		return a.Kinds
	}
	return []v1alpha1.FluxKind{
		v1alpha1.FluxKustomization, v1alpha1.FluxHelmRelease,
		v1alpha1.FluxGitRepository, v1alpha1.FluxOCIRepository,
	}
}

func protectedAlertnames(a *v1alpha1.AlertmanagerSilenceAction) []string {
	if len(a.ProtectedAlertnames) > 0 {
		return a.ProtectedAlertnames
	}
	return []string{"Watchdog", "InfoInhibitor"}
}

func responseMaxBytes(a *v1alpha1.HTTPRequestAction) int {
	if a.ResponseMaxBytes > 0 {
		return int(a.ResponseMaxBytes)
	}
	return defaultResponseMaxBytes
}

// Type returns the action type that is set in the spec, or false when none or more than one is set.
func Type(s *v1alpha1.OmpActionSpec) (v1alpha1.ActionType, bool) {
	var types []v1alpha1.ActionType
	if s.KubernetesRolloutRestart != nil {
		types = append(types, v1alpha1.ActionKubernetesRolloutRestart)
	}
	if s.FluxReconcile != nil {
		types = append(types, v1alpha1.ActionFluxReconcile)
	}
	if s.AlertmanagerSilence != nil {
		types = append(types, v1alpha1.ActionAlertmanagerSilence)
	}
	if s.HTTPRequest != nil {
		types = append(types, v1alpha1.ActionHTTPRequest)
	}
	if len(types) != 1 {
		return "", false
	}
	return types[0], true
}

// ValidateSpec checks everything about an OmpAction spec that needs no cluster access:
// exactly one type, valid kinds, parsable templates, a compilable parameter schema and
// absolute http(s) URLs without userinfo, query or fragment. A non-nil error is the
// message of an InvalidSpec condition.
func ValidateSpec(s *v1alpha1.OmpActionSpec) error {
	t, ok := Type(s)
	if !ok {
		return errors.New("exactly one of kubernetesRolloutRestart, fluxReconcile, alertmanagerSilence or httpRequest must be set")
	}
	if strings.TrimSpace(s.Description) == "" {
		return errors.New("description is required")
	}
	switch t {
	case v1alpha1.ActionKubernetesRolloutRestart:
		for _, k := range restartKinds(s.KubernetesRolloutRestart) {
			if newWorkload(k) == nil {
				return fmt.Errorf("unsupported workload kind %q", k)
			}
		}
	case v1alpha1.ActionFluxReconcile:
		for _, k := range fluxKinds(s.FluxReconcile) {
			if _, ok := fluxGVKs[k]; !ok {
				return fmt.Errorf("unsupported flux kind %q", k)
			}
		}
	case v1alpha1.ActionAlertmanagerSilence:
		if err := ValidateBaseURL(s.AlertmanagerSilence.URL); err != nil {
			return fmt.Errorf("alertmanagerSilence.url: %w", err)
		}
	case v1alpha1.ActionHTTPRequest:
		return validateHTTPRequest(s.HTTPRequest)
	}
	return nil
}

func validateHTTPRequest(h *v1alpha1.HTTPRequestAction) error {
	if err := ValidateBaseURL(h.AllowedURLPrefix); err != nil {
		return fmt.Errorf("httpRequest.allowedURLPrefix: %w", err)
	}
	if _, err := parseTemplate("url", h.URL); err != nil {
		return fmt.Errorf("httpRequest.url: %w", err)
	}
	if _, err := parseTemplate("body", h.Body); err != nil {
		return fmt.Errorf("httpRequest.body: %w", err)
	}
	if _, err := compileParameters(h.Parameters.Raw); err != nil {
		return fmt.Errorf("httpRequest.parameters: %w", err)
	}
	return nil
}

// ValidateBaseURL checks that s is an absolute http(s) URL with a host and without
// userinfo, query or fragment.
func ValidateBaseURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must be an absolute http or https URL")
	}
	if u.Hostname() == "" {
		return errors.New("must have a host")
	}
	if u.User != nil {
		return errors.New("must not contain userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(s, "#") {
		return errors.New("must not contain a query or fragment")
	}
	if strings.Contains(s, "\\") {
		return errors.New("must not contain backslashes")
	}
	return nil
}

func parseTemplate(name, text string) (*template.Template, error) {
	return template.New(name).Option("missingkey=error").Funcs(template.FuncMap{
		"json": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
	}).Parse(text)
}

// denyLoader refuses every $ref to an external resource: parameter schemas are self-contained.
type denyLoader struct{}

func (denyLoader) Load(u string) (any, error) {
	return nil, fmt.Errorf("external schema reference %q is not allowed", u)
}

// compileParameters compiles a JSON Schema whose root type is object.
func compileParameters(raw []byte) (*jsonschema.Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("is required")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	m, ok := doc.(map[string]any)
	if !ok || m["type"] != "object" {
		return nil, errors.New(`must be a JSON Schema with "type": "object"`)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(denyLoader{})
	const res = "mem://parameters.json"
	if err := c.AddResource(res, doc); err != nil {
		return nil, err
	}
	return c.Compile(res)
}
