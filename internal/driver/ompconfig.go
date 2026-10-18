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

package driver

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// roleDefault is the omp model role of the main conversation.
const roleDefault = "default"

// writeOmpConfig writes what omp reads at startup: config.yml (model roles, thinking level,
// tool approval), models.yml (only with a gateway), mcp.json, and the system prompt file.
func (d *Driver) writeOmpConfig(cfg agentapi.SessionConfig) error {
	if err := os.MkdirAll(d.Paths.OmpAgentDir, 0o755); err != nil {
		return err
	}
	if err := writeYAML(filepath.Join(d.Paths.OmpAgentDir, "config.yml"), ompSettings(cfg.Model)); err != nil {
		return err
	}
	if models := gatewayModels(cfg.Model); models != nil {
		if err := writeYAML(filepath.Join(d.Paths.OmpAgentDir, "models.yml"), models); err != nil {
			return err
		}
	}
	if mcp := bytes.TrimSpace(cfg.MCP); len(mcp) > 0 && string(mcp) != "null" {
		if err := os.WriteFile(filepath.Join(d.Paths.OmpAgentDir, "mcp.json"), mcp, 0o600); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(d.Paths.SystemPrompt), 0o755); err != nil {
		return err
	}
	return os.WriteFile(d.Paths.SystemPrompt, []byte(cfg.SystemPrompt), 0o600)
}

func writeYAML(path string, v any) error {
	raw, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// ompSettings is config.yml: the default and task roles use the default model, the smol
// and tiny roles the small one (or the default when none is set).
func ompSettings(m agentapi.ModelConfig) map[string]any {
	small := m.Small
	if small == "" {
		small = m.Default
	}
	settings := map[string]any{
		"modelRoles": map[string]string{
			roleDefault: m.Default,
			"task":      m.Default,
			"smol":      small,
			"tiny":      small,
		},
		"tools": map[string]any{"approvalMode": "yolo"},
	}
	if m.Thinking != "" {
		settings["defaultThinkingLevel"] = m.Thinking
	}
	return settings
}

// gatewayModels is models.yml: every provider routed through the gateway sends its requests
// to the gateway's pi-native endpoint, authenticated with $OMP_GATEWAY_TOKEN. It is nil
// without a gateway.
func gatewayModels(m agentapi.ModelConfig) map[string]any {
	if m.Gateway == nil || m.Gateway.URL == "" {
		return nil
	}
	providers := slices.Clone(m.Gateway.Providers)
	if len(providers) == 0 {
		for _, model := range []string{m.Default, m.Small} {
			if provider, _, ok := strings.Cut(model, "/"); ok && provider != "" {
				providers = append(providers, provider)
			}
		}
	}
	slices.Sort(providers)
	providers = slices.Compact(providers)
	out := make(map[string]any, len(providers))
	for _, p := range providers {
		out[p] = map[string]any{
			"baseUrl":   m.Gateway.URL,
			"apiKey":    agentapi.EnvGatewayToken,
			"transport": "pi-native",
		}
	}
	return map[string]any{"providers": out}
}

// splitModel splits "provider/id".
func splitModel(model string) (provider, id string, err error) {
	provider, id, ok := strings.Cut(model, "/")
	if !ok || provider == "" || id == "" {
		return "", "", fmt.Errorf("model %q is not provider/id", model)
	}
	return provider, id, nil
}
