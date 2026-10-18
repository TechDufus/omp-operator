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
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/prompt"
)

const samplesDir = "../../config/samples"

func readSample[T any](t *testing.T, file string) *T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(samplesDir, file))
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := yaml.UnmarshalStrict(raw, &v); err != nil {
		t.Fatalf("%s: strict decode: %v", file, err)
	}
	return &v
}

// TestSamplesDecodeStrictly decodes every sample into the API types, rejecting unknown fields,
// and checks the samples reference each other and agree with the kustomization.
func TestSamplesDecodeStrictly(t *testing.T) {
	agent := readSample[v1alpha1.OmpAgent](t, "omp_v1alpha1_ompagent.yaml")
	trigger := readSample[v1alpha1.OmpTrigger](t, "omp_v1alpha1_omptrigger.yaml")
	action := readSample[v1alpha1.OmpAction](t, "omp_v1alpha1_ompaction.yaml")
	session := readSample[v1alpha1.OmpSession](t, "omp_v1alpha1_ompsession.yaml")

	for kind, got := range map[string][2]string{
		"OmpAgent":   {agent.Kind, agent.APIVersion},
		"OmpTrigger": {trigger.Kind, trigger.APIVersion},
		"OmpAction":  {action.Kind, action.APIVersion},
		"OmpSession": {session.Kind, session.APIVersion},
	} {
		if got[0] != kind || got[1] != "omp.techdufus.com/v1alpha1" {
			t.Errorf("%s sample has kind %q apiVersion %q", kind, got[0], got[1])
		}
	}

	if trigger.Spec.AgentRef.Name != agent.Name || session.Spec.AgentRef.Name != agent.Name {
		t.Errorf("trigger and session samples must reference agent %q", agent.Name)
	}
	if !slices.ContainsFunc(agent.Spec.Actions, func(r v1alpha1.LocalObjectRef) bool { return r.Name == action.Name }) {
		t.Errorf("agent sample must list action %q", action.Name)
	}
	if trigger.Spec.Webhook == nil || trigger.Spec.Webhook.Adapter != v1alpha1.AdapterAlertmanager {
		t.Errorf("trigger sample must be an alertmanager webhook: %+v", trigger.Spec.Webhook)
	}
	if action.Spec.KubernetesRolloutRestart == nil {
		t.Error("action sample must be a kubernetesRolloutRestart")
	}
	if agent.Spec.Model.Gateway == nil || agent.Spec.Model.Gateway.TokenSecretRef != nil {
		t.Errorf("agent sample must use a managed gateway token: %+v", agent.Spec.Model.Gateway)
	}

	// The trigger prompt must render against the data a webhook prompt receives.
	if _, err := prompt.Render(trigger.Spec.Prompt, prompt.WebhookData{
		Agent: agent.Name, Trigger: trigger.Name, Session: "s",
		Event: prompt.Event{
			Name: "HighLoad/default", Key: "k", Severity: "warning", Status: "firing", Labels: map[string]string{},
		},
	}); err != nil {
		t.Errorf("trigger sample prompt: %v", err)
	}

	// kustomization.yaml lists exactly the sample files.
	var kz struct {
		Resources []string `json:"resources"`
	}
	raw, err := os.ReadFile(filepath.Join(samplesDir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &kz); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(samplesDir, "omp_v1alpha1_*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	onDisk := make([]string, 0, len(files))
	for _, f := range files {
		onDisk = append(onDisk, filepath.Base(f))
	}
	slices.Sort(onDisk)
	slices.Sort(kz.Resources)
	if !slices.Equal(onDisk, kz.Resources) {
		t.Errorf("kustomization resources %v, files on disk %v", kz.Resources, onDisk)
	}
}
