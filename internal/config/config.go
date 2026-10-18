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

// Package config holds the operator-wide settings that come from manager flags and the
// downward API, shared by every reconciler and server.
package config

import (
	"fmt"
	"slices"
	"strings"
)

// Options are the operator-wide settings.
type Options struct {
	// OperatorNamespace is POD_NAMESPACE: where the operator runs.
	OperatorNamespace string
	// ServiceName is the operator Service; pods reach the agent API at
	// http://<ServiceName>.<OperatorNamespace>.svc:<AgentAPIPort>.
	ServiceName string
	// AgentAPIPort is the port of --agent-api-bind-address.
	AgentAPIPort int
	// DataDir is --data-dir.
	DataDir string
	// DefaultRuntimeImage is used when an agent sets no image.
	DefaultRuntimeImage string
	// DashboardURL is the dashboard's external base URL, for notification links. May be empty.
	DashboardURL string
	// HooksPublicURL is the external base URL of the hooks server. May be empty.
	HooksPublicURL string
	// GatewayClientsSecret enables managed gateway tokens when set.
	GatewayClientsSecret string
	// AgentNamespaces limits agents to these namespaces; empty means all.
	AgentNamespaces []string
	// ActionTargetNamespaces lists namespaces actions may target besides their own;
	// ["*"] means any namespace except the operator's.
	ActionTargetNamespaces []string
}

// SplitList parses a comma-separated flag value, dropping blanks.
func SplitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// NamespaceAllowed reports whether agents (and their triggers, sessions and actions) in ns
// are served.
func (o *Options) NamespaceAllowed(ns string) bool {
	return len(o.AgentNamespaces) == 0 || slices.Contains(o.AgentNamespaces, ns)
}

// AgentAPIURL is the base URL pods use for the agent API.
func (o *Options) AgentAPIURL() string {
	return fmt.Sprintf("http://%s.%s.svc:%d", o.ServiceName, o.OperatorNamespace, o.AgentAPIPort)
}

// GatewayManaged reports whether managed gateway tokens are enabled.
func (o *Options) GatewayManaged() bool { return o.GatewayClientsSecret != "" }
