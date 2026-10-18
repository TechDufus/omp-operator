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

// Package actions defines the OmpAction types: validation, the target namespace rule, the
// agent-facing tool definitions and the execution of agent tool calls.
package actions

import (
	"regexp"
	"slices"
	"strings"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
)

// FinishTool is the built-in tool every session gets to report its result.
const FinishTool = "finish"

// Ready reasons reported by the OmpAction reconciler and returned by ValidateName.
const (
	ReasonInvalidName   = "InvalidName"
	ReasonReservedName  = "ReservedName"
	ReasonInvalidSpec   = "InvalidSpec"
	ReasonMissingSecret = "MissingSecret"
)

// ReservedToolNames are the tool names omp v18.12.1 registers itself (the built-in and
// hidden tools of its tool registry, plus names of internal tools) and the operator's
// finish. An OmpAction whose tool name is in this list is not Ready. test/ompcompat checks
// that every tool omp reports is in this list.
var ReservedToolNames = []string{
	"ask",
	"ast_edit",
	"ast_grep",
	"bash",
	"browser",
	"checkpoint",
	"context_notes",
	"debug",
	"edit",
	"eval",
	"exit_plan_mode",
	"find",
	FinishTool,
	"generate_image",
	"github",
	"glob",
	"goal",
	"grep",
	"ida",
	"irc",
	"learn",
	"lsp",
	"manage_skill",
	"memory_edit",
	"new_context",
	"read",
	"recall",
	"reflect",
	"report_finding",
	"resolve",
	"retain",
	"rewind",
	"security_scan",
	"submit_result",
	"task",
	"think",
	"todo",
	"wait",
	"web_search",
	"write",
	"yield",
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ToolName is the name the agent sees for an action: the action name with '-' replaced by '_'.
func ToolName(action string) string { return strings.ReplaceAll(action, "-", "_") }

// ValidateName checks an OmpAction name. On failure reason is ReasonInvalidName or ReasonReservedName.
func ValidateName(name string) (reason string, ok bool) {
	if !nameRE.MatchString(name) {
		return ReasonInvalidName, false
	}
	if slices.Contains(ReservedToolNames, ToolName(name)) {
		return ReasonReservedName, false
	}
	return "", true
}

// TargetNamespaceAllowed implements the target namespace rule of Kubernetes and Flux
// actions: target is allowed only when it is in rule.Allow (empty Allow means the action's
// own namespace; "*" permits any), is not in rule.Deny, is not the operator's namespace,
// and is the action's own namespace, in opts.ActionTargetNamespaces, or opts allows "*".
func TargetNamespaceAllowed(opts *config.Options, actionNamespace string, rule v1alpha1.NamespaceRule, target string) bool {
	if target == "" || target == opts.OperatorNamespace || slices.Contains(rule.Deny, target) {
		return false
	}
	allow := rule.Allow
	if len(allow) == 0 {
		allow = []string{actionNamespace}
	}
	if !slices.Contains(allow, "*") && !slices.Contains(allow, target) {
		return false
	}
	return target == actionNamespace ||
		slices.Contains(opts.ActionTargetNamespaces, "*") ||
		slices.Contains(opts.ActionTargetNamespaces, target)
}
