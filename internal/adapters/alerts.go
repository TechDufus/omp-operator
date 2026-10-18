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

package adapters

import (
	"encoding/json"
	"fmt"
	"strings"
)

// alertGroup is the Alertmanager webhook v4 payload. Grafana's webhook has the
// same shape plus orgId, state, title, message and per-alert URLs, which are
// either ignored or carried on [Alert]; the full body stays in Event.Payload.
type alertGroup struct {
	Status       string            `json:"status"`
	GroupKey     string            `json:"groupKey"`
	GroupLabels  map[string]string `json:"groupLabels"`
	CommonLabels map[string]string `json:"commonLabels"`
	Alerts       []Alert           `json:"alerts"`
}

// severityRank orders alert severities; unknown values rank 0.
var severityRank = map[string]int{"info": 1, "warning": 2, "error": 3, "critical": 4}

func parseAlerts(source string, body []byte, cfg Config) ([]Event, Stats, error) {
	var g alertGroup
	if err := json.Unmarshal(body, &g); err != nil {
		return nil, Stats{}, fmt.Errorf("%w: %s body: %w", ErrPayload, source, err)
	}
	if g.Status != StatusFiring && g.Status != StatusResolved {
		return nil, Stats{}, fmt.Errorf("%w: %s status %q is neither firing nor resolved", ErrPayload, source, g.Status)
	}
	if g.GroupKey == "" {
		return nil, Stats{}, fmt.Errorf("%w: %s payload has no groupKey", ErrPayload, source)
	}

	var stats Stats
	kept := make([]Alert, 0, len(g.Alerts))
	for _, a := range g.Alerts {
		if cfg.AlertFilter != nil {
			ok, err := cfg.AlertFilter.Bool(a.CELValue())
			if err != nil {
				stats.AlertErrors++
				stats.LastError = err
				continue
			}
			if !ok {
				stats.AlertsFiltered++
				continue
			}
		}
		kept = append(kept, a)
	}

	ev := Event{
		Source:       source,
		Key:          g.GroupKey,
		Name:         alertName(g, kept),
		Status:       g.Status,
		Severity:     highestSeverity(kept),
		Labels:       g.CommonLabels,
		Alerts:       kept,
		Fingerprints: firingFingerprints(kept),
		Payload:      body,
	}
	if ev.Labels == nil {
		ev.Labels = map[string]string{}
	}
	return []Event{ev}, stats, nil
}

// alertName is "<alertname>/<namespace or ->", each part taken from groupLabels,
// then commonLabels, then the first kept alert (the first alert when none kept).
func alertName(g alertGroup, kept []Alert) string {
	sources := []map[string]string{g.GroupLabels, g.CommonLabels}
	switch {
	case len(kept) > 0:
		sources = append(sources, kept[0].Labels)
	case len(g.Alerts) > 0:
		sources = append(sources, g.Alerts[0].Labels)
	}
	lookup := func(label, fallback string) string {
		for _, m := range sources {
			if v := m[label]; v != "" {
				return v
			}
		}
		return fallback
	}
	return lookup("alertname", "unknown") + "/" + lookup("namespace", "-")
}

// highestSeverity returns the highest-ranked labels.severity (lower-cased) among
// the alerts, or "" when none has one. Unknown values rank below info; the
// first of them wins.
func highestSeverity(alerts []Alert) string {
	best, bestRank := "", -1
	for _, a := range alerts {
		s := strings.ToLower(strings.TrimSpace(a.Labels["severity"]))
		if s == "" {
			continue
		}
		if r := severityRank[s]; r > bestRank {
			best, bestRank = s, r
		}
	}
	return best
}

// firingFingerprints returns the distinct, non-empty fingerprints of firing
// alerts in order of appearance.
func firingFingerprints(alerts []Alert) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, a := range alerts {
		if a.Status != StatusFiring || a.Fingerprint == "" {
			continue
		}
		if _, dup := seen[a.Fingerprint]; dup {
			continue
		}
		seen[a.Fingerprint] = struct{}{}
		out = append(out, a.Fingerprint)
	}
	return out
}
