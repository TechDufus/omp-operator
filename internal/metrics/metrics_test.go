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

package metrics

import (
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// TestExposedNamesAndLabels pins the names and label sets that dashboards and
// alerts depend on, as served from controller-runtime's registry.
func TestExposedNamesAndLabels(t *testing.T) {
	Register() // idempotent: a second call must not panic on duplicate registration

	SessionsTotal.WithLabelValues("ns", "a", "t", "webhook", "ok").Inc()
	SessionDuration.WithLabelValues("ns", "a").Observe(42)
	SessionsActive.WithLabelValues("ns", "a", "Running").Set(1)
	TriggerGroupsActive.WithLabelValues("ns", "t").Set(2)
	WebhookEventsTotal.WithLabelValues("ns", "t", "session").Inc()
	ActionCallsTotal.WithLabelValues("ns", "a", "restart", "ok").Inc()
	ModelAuthFailuresTotal.WithLabelValues("ns", "a").Inc()
	NotificationsTotal.WithLabelValues("ntfy", ResultOK).Inc()
	DataFreeBytes.Set(1)
	DataCapacityBytes.Set(2)
	SetBuildInfo("v0.0.0-test")

	// Label names, sorted and space separated.
	want := map[string]string{
		"omp_operator_sessions_total":            "agent kind namespace trigger verdict",
		"omp_operator_session_duration_seconds":  "agent namespace",
		"omp_operator_sessions_active":           "agent namespace phase",
		"omp_operator_trigger_groups_active":     "namespace trigger",
		"omp_operator_webhook_events_total":      "namespace result trigger",
		"omp_operator_action_calls_total":        "action agent namespace result",
		"omp_operator_model_auth_failures_total": "agent namespace",
		"omp_operator_notifications_total":       "result type",
		"omp_operator_data_free_bytes":           "",
		"omp_operator_data_capacity_bytes":       "",
		"omp_operator_build_info":                "version",
	}

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range families {
		if len(f.GetMetric()) == 0 {
			continue
		}
		got[f.GetName()] = strings.Join(labelNames(f.GetMetric()[0]), " ")
	}
	for name, labels := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s not exposed", name)
			continue
		}
		if g != labels {
			t.Errorf("%s labels = %v, want %v", name, g, labels)
		}
	}
}

func labelNames(m *dto.Metric) []string {
	names := make([]string, 0, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		names = append(names, l.GetName())
	}
	slices.Sort(names)
	return names
}

func TestSetBuildInfoReplacesPreviousVersion(t *testing.T) {
	SetBuildInfo("v1")
	SetBuildInfo("v2")
	if n := testutil.CollectAndCount(BuildInfo); n != 1 {
		t.Fatalf("build_info series = %d, want 1", n)
	}
	if v := testutil.ToFloat64(BuildInfo.WithLabelValues("v2")); v != 1 {
		t.Fatalf("build_info{version=v2} = %v, want 1", v)
	}
}
