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

// Package adapters turns webhook request bodies into normalized events.
//
// The alertmanager and grafana adapters produce one event per payload; the
// generic adapter produces events from a CEL mapping. Every adapter output is
// the same [Event], so the episode logic never sees a source-specific shape.
package adapters

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TechDufus/omp-operator/internal/celx"
)

// Adapter kinds, as in OmpTrigger spec.webhook.adapter.
const (
	KindAlertmanager = "alertmanager"
	KindGrafana      = "grafana"
	KindGeneric      = "generic"
)

// Computed defaults of the OmpTrigger CEL fields. An empty field in the spec
// means the default; the dashboard shows these constants.
const (
	// DefaultAlertFilter drops the always-firing heartbeat alerts.
	DefaultAlertFilter = `!(alert.labels.?alertname.orValue('') in ['Watchdog','InfoInhibitor'])`
	// DefaultFilter keeps every event.
	DefaultFilter = "true"
	// DefaultMappingEvents maps the payload to one event.
	DefaultMappingEvents = "[payload]"
	// DefaultMappingTitle leaves the event name to the key.
	DefaultMappingTitle = "''"
	// DefaultMappingSeverity is the severity of a generic event.
	DefaultMappingSeverity = "'info'"
	// DefaultMappingStatus makes every generic event firing.
	DefaultMappingStatus = "'firing'"
	// DefaultMappingFingerprint documents the computed default of
	// mapping.fingerprint: it is not a CEL expression but the hex SHA-256 of
	// the event's canonical JSON payload.
	DefaultMappingFingerprint = "sha256(event JSON)"
)

// Event statuses.
const (
	StatusFiring   = "firing"
	StatusResolved = "resolved"
)

// MaxNameLength is the longest generic event name, in characters.
const MaxNameLength = 120

// Errors callers can match with errors.Is. A hooks handler answers 400 for
// [ErrPayload] and treats [ErrConfig] as an operator-side failure.
var (
	// ErrPayload means the request body is not valid for the adapter.
	ErrPayload = errors.New("invalid payload")
	// ErrConfig means the adapter kind or its configuration is unusable.
	ErrConfig = errors.New("invalid adapter configuration")
)

// Alert is one alert of an alert-adapter event.
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Status      string            `json:"status"`
	Fingerprint string            `json:"fingerprint"`
	StartsAt    string            `json:"startsAt"`

	// Optional fields present in Alertmanager and Grafana payloads.
	EndsAt       string `json:"endsAt,omitempty"`
	GeneratorURL string `json:"generatorURL,omitempty"`
	// Grafana only.
	SilenceURL   string `json:"silenceURL,omitempty"`
	DashboardURL string `json:"dashboardURL,omitempty"`
	PanelURL     string `json:"panelURL,omitempty"`
	ImageURL     string `json:"imageURL,omitempty"`
}

// CELValue is the value of the CEL variable alert: labels, annotations,
// status, fingerprint and startsAt.
func (a Alert) CELValue() map[string]any {
	return map[string]any{
		"labels":      stringMap(a.Labels),
		"annotations": stringMap(a.Annotations),
		"status":      a.Status,
		"fingerprint": a.Fingerprint,
		"startsAt":    a.StartsAt,
	}
}

// Event is a normalized webhook event.
type Event struct {
	// Source is the adapter kind that produced the event.
	Source string
	// Key identifies the group the event belongs to (Alertmanager groupKey).
	Key string
	// Name is a short human-readable title.
	Name string
	// Status is StatusFiring or StatusResolved.
	Status string
	// Severity is the highest alert severity (alert adapters, lower-cased;
	// empty when no alert carries one) or the mapped severity (generic).
	Severity string
	// Labels are the common labels of an alert group; empty for generic events.
	Labels map[string]string
	// Alerts are the alerts that passed alertFilter; empty for generic events.
	Alerts []Alert
	// Fingerprints are the distinct fingerprints that are currently firing.
	Fingerprints []string
	// Payload is the JSON of the event: the request body for alert adapters
	// (aliasing the slice passed to Parse), the canonical JSON of the mapped
	// element for generic.
	Payload json.RawMessage
}

// CELValue is the value of the CEL variable event: source, key, name, status,
// severity, labels, alerts, fingerprints and payload (the decoded Payload).
func (e Event) CELValue() map[string]any {
	alerts := make([]any, len(e.Alerts))
	for i, a := range e.Alerts {
		alerts[i] = a.CELValue()
	}
	fps := make([]any, len(e.Fingerprints))
	for i, f := range e.Fingerprints {
		fps[i] = f
	}
	var payload any
	if len(e.Payload) > 0 {
		// A malformed Payload leaves payload null; adapters only store valid JSON.
		_ = json.Unmarshal(e.Payload, &payload)
	}
	return map[string]any{
		"source":       e.Source,
		"key":          e.Key,
		"name":         e.Name,
		"status":       e.Status,
		"severity":     e.Severity,
		"labels":       stringMap(e.Labels),
		"alerts":       alerts,
		"fingerprints": fps,
		"payload":      payload,
	}
}

func stringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Mapping holds the compiled generic-adapter mapping. Events, Title, Severity,
// Status and Fingerprint may be nil: Parse then applies the computed default
// (see the Default* constants; a nil Fingerprint is the SHA-256 of the event
// JSON). Key is required.
type Mapping struct {
	Events      *celx.Program
	Key         *celx.Program
	Title       *celx.Program
	Severity    *celx.Program
	Status      *celx.Program
	Fingerprint *celx.Program
}

// Config carries the compiled programs of one OmpTrigger.
type Config struct {
	// AlertFilter selects alerts for the alert adapters; nil keeps every alert.
	AlertFilter *celx.Program
	// Filter is applied per event by [Config.MatchEvent]; nil matches every event.
	Filter *celx.Program
	// Mapping is required by the generic adapter.
	Mapping *Mapping
}

// MatchEvent applies Filter to the event. A runtime error evaluates to false
// and is returned so the caller can count it as result="error".
func (c Config) MatchEvent(e Event) (bool, error) {
	if c.Filter == nil {
		return true, nil
	}
	return c.Filter.Bool(e.CELValue())
}

// Stats counts what Parse discarded.
type Stats struct {
	// AlertsFiltered counts alerts rejected by alertFilter.
	AlertsFiltered int
	// AlertErrors counts alerts dropped because alertFilter failed at run time.
	AlertErrors int
	// EventErrors counts generic events dropped because a mapping expression
	// failed or returned an invalid value.
	EventErrors int
	// LastError is the most recent runtime error, for logging.
	LastError error
}

// Errors reports the number of runtime errors Parse swallowed, for
// omp_operator_webhook_events_total{result="error"}.
func (s Stats) Errors() int { return s.AlertErrors + s.EventErrors }

// Parse decodes body with the adapter named by kind and returns its events.
// Runtime CEL errors never fail the parse; they are counted in Stats. A body
// that does not fit the adapter returns an error wrapping [ErrPayload]; an
// unknown kind or a generic adapter without a key mapping returns one wrapping
// [ErrConfig].
func Parse(kind string, body []byte, cfg Config) ([]Event, Stats, error) {
	switch kind {
	case KindAlertmanager, KindGrafana:
		return parseAlerts(kind, body, cfg)
	case KindGeneric:
		return parseGeneric(body, cfg)
	default:
		return nil, Stats{}, fmt.Errorf("%w: unknown adapter %q", ErrConfig, kind)
	}
}

// Expressions are the raw CEL strings of a trigger's webhook spec; empty means
// the computed default.
type Expressions struct {
	AlertFilter string
	Filter      string
	Mapping     *MappingExpressions
}

// MappingExpressions are the raw spec.webhook.mapping strings.
type MappingExpressions struct {
	Events      string
	Key         string
	Title       string
	Severity    string
	Status      string
	Fingerprint string
}

// Resolve compiles the expressions for kind into a Config, applying the
// computed defaults, and so doubles as the trigger's Ready check. Errors read
// "webhook.<role>: ..." (for example "webhook.mapping.key: ..."), naming the
// failing spec field. The alert filter is compiled for the alert adapters
// and the mapping for the generic adapter only. Use [celx.Compile] or
// [celx.Cache.Compiler] as compile.
func Resolve(kind string, x Expressions, compile celx.Compiler) (Config, error) {
	var cfg Config
	var err error
	if cfg.Filter, err = compileOr(compile, celx.RoleEventFilter, x.Filter, DefaultFilter); err != nil {
		return Config{}, fmt.Errorf("webhook.%w", err)
	}
	switch kind {
	case KindAlertmanager, KindGrafana:
		if cfg.AlertFilter, err = compileOr(compile, celx.RoleAlertFilter, x.AlertFilter, DefaultAlertFilter); err != nil {
			return Config{}, fmt.Errorf("webhook.%w", err)
		}
	case KindGeneric:
		if x.Mapping == nil || x.Mapping.Key == "" {
			return Config{}, errors.New("webhook.mapping.key: required for the generic adapter")
		}
		m := &Mapping{}
		fields := []struct {
			dst  **celx.Program
			role celx.Role
			expr string
			dflt string
		}{
			{&m.Events, celx.RoleMappingEvents, x.Mapping.Events, DefaultMappingEvents},
			{&m.Key, celx.RoleMappingKey, x.Mapping.Key, ""},
			{&m.Title, celx.RoleMappingTitle, x.Mapping.Title, DefaultMappingTitle},
			{&m.Severity, celx.RoleMappingSeverity, x.Mapping.Severity, DefaultMappingSeverity},
			{&m.Status, celx.RoleMappingStatus, x.Mapping.Status, DefaultMappingStatus},
			{&m.Fingerprint, celx.RoleMappingFingerprint, x.Mapping.Fingerprint, ""},
		}
		for _, f := range fields {
			if f.expr == "" && f.dflt == "" {
				continue // key is checked above; fingerprint's default is not CEL
			}
			if *f.dst, err = compileOr(compile, f.role, f.expr, f.dflt); err != nil {
				return Config{}, fmt.Errorf("webhook.%w", err)
			}
		}
		cfg.Mapping = m
	default:
		return Config{}, fmt.Errorf("%w: unknown adapter %q", ErrConfig, kind)
	}
	return cfg, nil
}

func compileOr(compile celx.Compiler, role celx.Role, expr, dflt string) (*celx.Program, error) {
	if expr == "" {
		expr = dflt
	}
	return compile(role, expr)
}
