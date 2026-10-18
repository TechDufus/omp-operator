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

// Package metrics holds the operator's Prometheus collectors. They are
// registered on controller-runtime's metrics registry, so they are served by
// the manager's metrics endpoint next to the built-in controller metrics.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "omp_operator"

// Label names used by more than one collector.
const (
	lNamespace = "namespace"
	lAgent     = "agent"
	lResult    = "result"
	lTrigger   = "trigger"
)

// Label values for omp_operator_notifications_total's result label.
const (
	ResultOK    = "ok"
	ResultError = "error"
)

// Collectors. Counters and gauges with labels are vectors; callers use
// WithLabelValues in the label order given in each comment.
var (
	// SessionsTotal counts finished sessions: namespace, agent, trigger, kind
	// (webhook|schedule|manual), verdict (ok|fixed|needs_owner|inconclusive|failed).
	SessionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "sessions_total",
		Help:      "Finished sessions by verdict; failed sessions use verdict=\"failed\".",
	}, []string{lNamespace, lAgent, lTrigger, "kind", "verdict"})

	// SessionDuration observes the wall time of finished sessions: namespace, agent.
	SessionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "session_duration_seconds",
		Help:      "Time from session start to completion.",
		Buckets:   []float64{15, 30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600, 5400, 7200},
	}, []string{lNamespace, lAgent})

	// SessionsActive is the current number of sessions per phase: namespace, agent, phase.
	SessionsActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "sessions_active",
		Help:      "Sessions currently in each phase.",
	}, []string{lNamespace, lAgent, "phase"})

	// TriggerGroupsActive is the number of tracked groups of a trigger: namespace, trigger.
	TriggerGroupsActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "trigger_groups_active",
		Help:      "Groups currently tracked in a trigger's status.",
	}, []string{lNamespace, lTrigger})

	// WebhookEventsTotal counts handled webhook events: namespace, trigger, result
	// (session|filtered|deduped|busy|capped|resolved|error).
	WebhookEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "webhook_events_total",
		Help:      "Webhook events by processing result.",
	}, []string{lNamespace, lTrigger, lResult})

	// ActionCallsTotal counts agent action calls: namespace, agent, action, result.
	ActionCallsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "action_calls_total",
		Help:      "Agent action calls by result.",
	}, []string{lNamespace, lAgent, "action", lResult})

	// ModelAuthFailuresTotal counts sessions that ended with an auth outcome: namespace, agent.
	ModelAuthFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "model_auth_failures_total",
		Help:      "Session attempts that failed because model authentication was rejected.",
	}, []string{lNamespace, lAgent})

	// NotificationsTotal counts notification deliveries: type (ntfy|slack|discord|webhook),
	// result (ResultOK|ResultError).
	NotificationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "notifications_total",
		Help:      "Notification deliveries by sink type and final result.",
	}, []string{"type", lResult})

	// DataFreeBytes is the free space of the data directory.
	DataFreeBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "data_free_bytes",
		Help:      "Free bytes on the operator data volume.",
	})

	// DataCapacityBytes is the capacity of the data directory's volume.
	DataCapacityBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "data_capacity_bytes",
		Help:      "Capacity in bytes of the operator data volume.",
	})

	// BuildInfo is 1 for the running version; set it with SetBuildInfo.
	BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Operator build information; the value is always 1.",
	}, []string{"version"})
)

var registerOnce sync.Once

func init() { Register() }

// Register adds every collector to controller-runtime's metrics registry. It
// is idempotent and runs from init, so importing the package is enough.
func Register() {
	registerOnce.Do(func() {
		ctrlmetrics.Registry.MustRegister(
			SessionsTotal,
			SessionDuration,
			SessionsActive,
			TriggerGroupsActive,
			WebhookEventsTotal,
			ActionCallsTotal,
			ModelAuthFailuresTotal,
			NotificationsTotal,
			DataFreeBytes,
			DataCapacityBytes,
			BuildInfo,
		)
	})
}

// SetBuildInfo publishes version as the only build_info series, with value 1.
func SetBuildInfo(version string) {
	BuildInfo.Reset()
	BuildInfo.WithLabelValues(version).Set(1)
}
