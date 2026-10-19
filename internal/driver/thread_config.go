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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// writeThreadOmpConfig writes what omp reads at startup for a thread sitting. Unlike a plain
// session, the user's own config.yml (shipped by the handoff) is the base: every other setting
// of the user's local omp carries over, and only what cannot work in the pod is overridden.
func (d *Driver) writeThreadOmpConfig(cfg agentapi.SessionConfig) error {
	if err := os.MkdirAll(d.Paths.OmpAgentDir, 0o755); err != nil {
		return err
	}
	file := filepath.Join(d.Paths.OmpAgentDir, "config.yml")
	settings := map[string]any{}
	switch raw, err := os.ReadFile(file); {
	case errors.Is(err, fs.ErrNotExist):
		// No shipped settings: start from what a plain session would configure.
		settings = ompSettings(cfg.Model)
		settings["modelRoles"] = toAnyMap(settings["modelRoles"])
	case err != nil:
		return err
	default:
		if err := yaml.Unmarshal(raw, &settings); err != nil {
			d.Log.Warn("ignoring unreadable shipped config.yml", "error", err)
			settings = map[string]any{}
		}
		if settings == nil {
			settings = map[string]any{}
		}
	}
	mergeThreadSettings(settings, cfg)
	if err := writeYAML(file, settings); err != nil {
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

// mergeThreadSettings applies the pod's overrides to the user's settings:
//
//   - every model role whose provider the gateway does not serve is mapped to the thread's
//     model (the user's local roles may name providers this pod has no credentials for), and
//     the default role is always the thread's model;
//   - the thinking level of the thread wins over the shipped one;
//   - approvals are off, and the features that need a person or a desktop are disabled:
//     computer use, collab rooms, marketplace updates.
func mergeThreadSettings(settings map[string]any, cfg agentapi.SessionConfig) {
	model := cfg.Model.Default
	served := map[string]bool{}
	if cfg.Model.Gateway != nil {
		for _, p := range cfg.Model.Gateway.Providers {
			served[p] = true
		}
	}
	roles := toAnyMap(settings["modelRoles"])
	for role, v := range roles {
		value, ok := v.(string)
		if !ok {
			continue
		}
		if provider, _, found := strings.Cut(value, "/"); !found || !served[provider] {
			roles[role] = model
		}
	}
	roles[roleDefault] = model
	settings["modelRoles"] = roles

	switch {
	case cfg.Thread != nil && cfg.Thread.Thinking != "":
		settings["defaultThinkingLevel"] = cfg.Thread.Thinking
	case settings["defaultThinkingLevel"] == nil && cfg.Model.Thinking != "":
		settings["defaultThinkingLevel"] = cfg.Model.Thinking
	}
	setNested(settings, "tools", "approvalMode", approvalYolo)
	setNested(settings, "computer", "enabled", false)
	setNested(settings, "collab", "autoStart", "off")
	setNested(settings, "marketplace", "autoUpdate", "off")
}

// toAnyMap returns v as a map[string]any, converting a map[string]string; anything else yields an empty map.
func toAnyMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[string]string:
		out := make(map[string]any, len(m))
		for k, s := range m {
			out[k] = s
		}
		return out
	default:
		return map[string]any{}
	}
}

// setNested sets settings[group][key] = value, replacing a group that is not a map.
func setNested(settings map[string]any, group, key string, value any) {
	m, ok := settings[group].(map[string]any)
	if !ok {
		m = map[string]any{}
	}
	m[key] = value
	settings[group] = m
}

// threadOmpArgs is the command line of a thread sitting: no tool flag at all when the agent
// allows every built-in tool, and omp stops itself when the sitting's lifetime ends. The sitting
// runs in rpc-ui mode (still headless for extensions) because that is the mode in which omp
// sends an ask tool call to the host as an extension_ui_request.
func threadOmpArgs(cfg agentapi.SessionConfig, p Paths) []string {
	args := []string{"--mode", "rpc-ui", "--no-ui", "--approval-mode", approvalYolo}
	switch {
	case cfg.Tools == nil:
	case len(cfg.Tools) > 0:
		args = append(args, "--tools", strings.Join(cfg.Tools, ","))
	default:
		args = append(args, "--no-tools")
	}
	if cfg.Thread != nil && cfg.Thread.MaxSittingSeconds > 0 {
		args = append(args, "--max-time", strconv.FormatInt(cfg.Thread.MaxSittingSeconds, 10)+"s")
	}
	args = append(args, "--session-dir", p.SessionDir)
	if cfg.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", p.SystemPrompt)
	}
	return args
}
