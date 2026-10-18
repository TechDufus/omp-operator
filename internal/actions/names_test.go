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

//nolint:goconst // test fixtures repeat literals on purpose
package actions

import (
	"strings"
	"testing"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/config"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name   string
		reason string
	}{
		{"restart-web", ""},
		{"a", ""},
		{"silence-alerts-2", ""},
		{"Restart", ReasonInvalidName},
		{"1restart", ReasonInvalidName},
		{"re_start", ReasonInvalidName},
		{"", ReasonInvalidName},
		{"-x", ReasonInvalidName},
		{strings.Repeat("a", 64), ReasonInvalidName},
		{strings.Repeat("a", 63), ""},
		{"bash", ReasonReservedName},
		{"finish", ReasonReservedName},
		{"web-search", ReasonReservedName},
		{"ast-grep", ReasonReservedName},
		{"read", ReasonReservedName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, ok := ValidateName(tt.name)
			if reason != tt.reason || ok != (tt.reason == "") {
				t.Fatalf("ValidateName(%q) = %q, %v; want reason %q", tt.name, reason, ok, tt.reason)
			}
		})
	}
}

func TestToolName(t *testing.T) {
	if got := ToolName("restart-web-app"); got != "restart_web_app" {
		t.Fatalf("ToolName = %q", got)
	}
}

func TestTargetNamespaceAllowed(t *testing.T) {
	tests := []struct {
		name    string
		targets []string // --action-target-namespaces
		rule    v1alpha1.NamespaceRule
		target  string
		want    bool
	}{
		{"own namespace by default", nil, v1alpha1.NamespaceRule{}, "apps", true},
		{"other namespace by default", nil, v1alpha1.NamespaceRule{}, "other", false},
		{"allow star without the flag", nil, v1alpha1.NamespaceRule{Allow: []string{"*"}}, "other", false},
		{"allow star without the flag, own", nil, v1alpha1.NamespaceRule{Allow: []string{"*"}}, "apps", true},
		{"allow star with flag star", []string{"*"}, v1alpha1.NamespaceRule{Allow: []string{"*"}}, "other", true},
		{"allow list with flag list", []string{"other"}, v1alpha1.NamespaceRule{Allow: []string{"other"}}, "other", true},
		{"allow list, namespace not flagged", []string{"another"}, v1alpha1.NamespaceRule{Allow: []string{"other"}}, "other", false},
		{"flag star but not in allow", []string{"*"}, v1alpha1.NamespaceRule{Allow: []string{"x"}}, "other", false},
		{"deny wins", []string{"*"}, v1alpha1.NamespaceRule{Allow: []string{"*"}, Deny: []string{"other"}}, "other", false},
		{"deny own namespace", nil, v1alpha1.NamespaceRule{Deny: []string{"apps"}}, "apps", false},
		{"operator namespace denied despite star", []string{"*"}, v1alpha1.NamespaceRule{Allow: []string{"*"}}, testOperNS, false},
		{"operator namespace denied when listed", []string{testOperNS}, v1alpha1.NamespaceRule{Allow: []string{testOperNS}}, testOperNS, false},
		{"empty target", []string{"*"}, v1alpha1.NamespaceRule{Allow: []string{"*"}}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &config.Options{OperatorNamespace: testOperNS, ActionTargetNamespaces: tt.targets}
			if got := TargetNamespaceAllowed(opts, "apps", tt.rule, tt.target); got != tt.want {
				t.Fatalf("TargetNamespaceAllowed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOperatorNamespaceDeniedForOwnAction(t *testing.T) {
	opts := &config.Options{OperatorNamespace: testOperNS}
	if TargetNamespaceAllowed(opts, testOperNS, v1alpha1.NamespaceRule{}, testOperNS) {
		t.Fatal("an action in the operator namespace may target the operator namespace")
	}
}
