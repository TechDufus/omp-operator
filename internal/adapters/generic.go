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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

func parseGeneric(body []byte, cfg Config) ([]Event, Stats, error) {
	m := cfg.Mapping
	if m == nil || m.Key == nil {
		return nil, Stats{}, fmt.Errorf("%w: generic adapter needs mapping.key", ErrConfig)
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, Stats{}, fmt.Errorf("%w: generic body: %w", ErrPayload, err)
	}

	items := []any{payload}
	if m.Events != nil {
		var err error
		if items, err = m.Events.List(payload); err != nil {
			return nil, Stats{}, fmt.Errorf("%w: %w", ErrPayload, err)
		}
	}

	var stats Stats
	events := make([]Event, 0, len(items))
	for _, item := range items {
		ev, err := mapEvent(m, item)
		if err != nil {
			stats.EventErrors++
			stats.LastError = err
			continue
		}
		events = append(events, ev)
	}
	return events, stats, nil
}

// mapEvent applies the mapping to one element of the events list. Any failing
// expression or invalid value drops the event.
func mapEvent(m *Mapping, item any) (Event, error) {
	key, err := m.Key.String(item)
	if err != nil {
		return Event{}, err
	}
	if key == "" {
		return Event{}, errors.New("mapping.key: evaluated to an empty string")
	}

	title, severity, status := "", "info", StatusFiring
	if m.Title != nil {
		if title, err = m.Title.String(item); err != nil {
			return Event{}, err
		}
	}
	if m.Severity != nil {
		if severity, err = m.Severity.String(item); err != nil {
			return Event{}, err
		}
	}
	if m.Status != nil {
		if status, err = m.Status.String(item); err != nil {
			return Event{}, err
		}
	}
	if status != StatusFiring && status != StatusResolved {
		return Event{}, fmt.Errorf("mapping.status: %q is neither firing nor resolved", status)
	}

	payload, err := json.Marshal(item)
	if err != nil {
		return Event{}, fmt.Errorf("event is not JSON-representable: %w", err)
	}

	fingerprints := []string{}
	if status == StatusFiring {
		fp := ""
		if m.Fingerprint != nil {
			if fp, err = m.Fingerprint.String(item); err != nil {
				return Event{}, err
			}
		} else {
			sum := sha256.Sum256(payload)
			fp = hex.EncodeToString(sum[:])
		}
		if fp != "" {
			fingerprints = append(fingerprints, fp)
		}
	}

	if title == "" {
		title = key
	}
	return Event{
		Source:       KindGeneric,
		Key:          key,
		Name:         truncate(title, MaxNameLength),
		Status:       status,
		Severity:     severity,
		Labels:       map[string]string{},
		Alerts:       []Alert{},
		Fingerprints: fingerprints,
		Payload:      payload,
	}, nil
}

// truncate cuts s to at most n characters.
func truncate(s string, n int) string {
	if len(s) <= n { // fewer bytes than n implies fewer characters
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
