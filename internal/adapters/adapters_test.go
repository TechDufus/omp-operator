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

package adapters_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/TechDufus/omp-operator/internal/adapters"
	"github.com/TechDufus/omp-operator/internal/celx"
)

const (
	keyExpr     = "payload.id"
	missingExpr = "payload.nope"
	bodyA       = `{"id":"a"}`
	nameNoNS    = "A/-"
)

func resolve(t *testing.T, kind string, x adapters.Expressions) adapters.Config {
	t.Helper()
	cfg, err := adapters.Resolve(kind, x, celx.Compile)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", kind, err)
	}
	return cfg
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseOne(t *testing.T, kind string, body []byte, cfg adapters.Config) (adapters.Event, adapters.Stats) {
	t.Helper()
	events, stats, err := adapters.Parse(kind, body, cfg)
	if err != nil {
		t.Fatalf("Parse(%s): %v", kind, err)
	}
	if len(events) != 1 {
		t.Fatalf("Parse(%s) returned %d events, want 1", kind, len(events))
	}
	return events[0], stats
}

func TestAlertmanagerV4Sample(t *testing.T) {
	body := readTestdata(t, "alertmanager_v4.json")
	ev, stats := parseOne(t, adapters.KindAlertmanager, body, resolve(t, adapters.KindAlertmanager, adapters.Expressions{}))

	if ev.Source != adapters.KindAlertmanager || ev.Status != adapters.StatusFiring {
		t.Fatalf("source/status = %q/%q", ev.Source, ev.Status)
	}
	if want := `{}/{team="core"}:{alertname="HighCPU", namespace="prod"}`; ev.Key != want {
		t.Fatalf("key = %q, want %q", ev.Key, want)
	}
	if ev.Name != "HighCPU/prod" {
		t.Fatalf("name = %q, want HighCPU/prod", ev.Name)
	}
	if ev.Severity != "critical" {
		t.Fatalf("severity = %q, want critical (warning, critical, error among kept alerts)", ev.Severity)
	}
	if want := map[string]string{"team": "core", "namespace": "prod"}; !reflect.DeepEqual(ev.Labels, want) {
		t.Fatalf("labels = %v, want %v", ev.Labels, want)
	}
	// Firing only: the resolved alert and the duplicate fingerprint do not count,
	// and the filtered Watchdog does not appear.
	if want := []string{"aaaa111122223333", "bbbb111122223333"}; !reflect.DeepEqual(ev.Fingerprints, want) {
		t.Fatalf("fingerprints = %v, want %v", ev.Fingerprints, want)
	}
	// Alerts keep firing, resolved and duplicate entries; only Watchdog is gone.
	if len(ev.Alerts) != 4 {
		t.Fatalf("kept %d alerts, want 4", len(ev.Alerts))
	}
	for _, a := range ev.Alerts {
		if a.Labels["alertname"] == "Watchdog" {
			t.Fatal("Watchdog must be filtered by the default alertFilter")
		}
	}
	if got := ev.Alerts[0]; got.Annotations["summary"] != "CPU is high" || got.StartsAt != "2026-10-18T10:00:00.000Z" ||
		got.GeneratorURL == "" || got.EndsAt == "" {
		t.Fatalf("alert fields not decoded: %+v", got)
	}
	if stats.AlertsFiltered != 1 || stats.Errors() != 0 {
		t.Fatalf("stats = %+v, want 1 filtered and no errors", stats)
	}
	if string(ev.Payload) != string(body) {
		t.Fatal("payload must be the request body")
	}
}

func TestAlertmanagerResolvedAndFilters(t *testing.T) {
	body := func(status string, alerts ...string) []byte {
		return fmt.Appendf(nil, `{"status":%q,"groupKey":"gk","alerts":[%s]}`, status, strings.Join(alerts, ","))
	}
	alert := func(status, fp, labels string) string {
		return fmt.Sprintf(`{"status":%q,"fingerprint":%q,"labels":%s}`, status, fp, labels)
	}

	t.Run("resolved payload has no fingerprints but is still an event", func(t *testing.T) {
		ev, _ := parseOne(t, adapters.KindAlertmanager,
			body("resolved", alert("resolved", "f1", `{"alertname":"A"}`)),
			resolve(t, adapters.KindAlertmanager, adapters.Expressions{}))
		if ev.Status != "resolved" || len(ev.Fingerprints) != 0 || ev.Fingerprints == nil {
			t.Fatalf("event = %+v, want resolved with empty non-nil fingerprints", ev)
		}
	})
	t.Run("all alerts filtered still yields one event with no fingerprints", func(t *testing.T) {
		ev, stats := parseOne(t, adapters.KindAlertmanager,
			body("firing", alert("firing", "w", `{"alertname":"Watchdog"}`), alert("firing", "i", `{"alertname":"InfoInhibitor"}`)),
			resolve(t, adapters.KindAlertmanager, adapters.Expressions{}))
		if len(ev.Fingerprints) != 0 || len(ev.Alerts) != 0 || stats.AlertsFiltered != 2 {
			t.Fatalf("event = %+v stats = %+v", ev, stats)
		}
	})
	t.Run("nil alertFilter keeps everything", func(t *testing.T) {
		ev, _ := parseOne(t, adapters.KindAlertmanager,
			body("firing", alert("firing", "w", `{"alertname":"Watchdog"}`)), adapters.Config{})
		if !reflect.DeepEqual(ev.Fingerprints, []string{"w"}) {
			t.Fatalf("fingerprints = %v", ev.Fingerprints)
		}
	})
	t.Run("custom alertFilter replaces the default", func(t *testing.T) {
		cfg := resolve(t, adapters.KindAlertmanager, adapters.Expressions{AlertFilter: `alert.labels.?severity.orValue('') == 'critical'`})
		ev, stats := parseOne(t, adapters.KindAlertmanager,
			body("firing",
				alert("firing", "w", `{"alertname":"Watchdog","severity":"critical"}`),
				alert("firing", "n", `{"alertname":"Noisy","severity":"info"}`)), cfg)
		if !reflect.DeepEqual(ev.Fingerprints, []string{"w"}) || stats.AlertsFiltered != 1 {
			t.Fatalf("fingerprints = %v stats = %+v", ev.Fingerprints, stats)
		}
	})
	t.Run("alertFilter runtime error drops the alert and is counted", func(t *testing.T) {
		cfg := resolve(t, adapters.KindAlertmanager, adapters.Expressions{AlertFilter: `alert.labels['team'] == 'a'`})
		ev, stats := parseOne(t, adapters.KindAlertmanager,
			body("firing",
				alert("firing", "ok", `{"alertname":"A","team":"a"}`),
				alert("firing", "boom", `{"alertname":"B"}`)), cfg)
		if !reflect.DeepEqual(ev.Fingerprints, []string{"ok"}) || stats.AlertErrors != 1 || stats.Errors() != 1 || stats.LastError == nil {
			t.Fatalf("fingerprints = %v stats = %+v", ev.Fingerprints, stats)
		}
	})
}

func TestAlertNameAndSeverity(t *testing.T) {
	tests := []struct {
		name         string
		payload      string
		wantName     string
		wantSeverity string
	}{
		{
			"group labels win",
			`{"status":"firing","groupKey":"k","groupLabels":{"alertname":"G","namespace":"gns"},"commonLabels":{"alertname":"C","namespace":"cns"},
			  "alerts":[{"status":"firing","fingerprint":"f","labels":{"alertname":"A","namespace":"ans","severity":"warning"}}]}`,
			"G/gns", "warning",
		},
		{
			"common labels then first alert per part",
			`{"status":"firing","groupKey":"k","groupLabels":{},"commonLabels":{"alertname":"C"},
			  "alerts":[{"status":"firing","fingerprint":"f","labels":{"alertname":"A","namespace":"ans"}}]}`,
			"C/ans", "",
		},
		{
			"first alert when no group or common labels",
			`{"status":"firing","groupKey":"k","alerts":[{"status":"firing","fingerprint":"f","labels":{"alertname":"A"}}]}`,
			nameNoNS, "",
		},
		{
			"nothing at all",
			`{"status":"resolved","groupKey":"k","alerts":[]}`,
			"unknown/-", "",
		},
		{
			"error outranks warning and info",
			`{"status":"firing","groupKey":"k","alerts":[
			  {"status":"firing","fingerprint":"1","labels":{"alertname":"A","severity":"info"}},
			  {"status":"firing","fingerprint":"2","labels":{"alertname":"A","severity":"warning"}},
			  {"status":"firing","fingerprint":"3","labels":{"alertname":"A","severity":"Error"}}]}`,
			nameNoNS, "error",
		},
		{
			"unknown severity ranks below info",
			`{"status":"firing","groupKey":"k","alerts":[
			  {"status":"firing","fingerprint":"1","labels":{"alertname":"A","severity":"page"}},
			  {"status":"firing","fingerprint":"2","labels":{"alertname":"A","severity":"info"}}]}`,
			nameNoNS, "info",
		},
		{
			"only an unknown severity",
			`{"status":"firing","groupKey":"k","alerts":[{"status":"firing","fingerprint":"1","labels":{"alertname":"A","severity":"page"}}]}`,
			nameNoNS, "page",
		},
		{
			"filtered alerts do not set severity or name",
			`{"status":"firing","groupKey":"k","alerts":[
			  {"status":"firing","fingerprint":"w","labels":{"alertname":"Watchdog","severity":"critical"}},
			  {"status":"firing","fingerprint":"2","labels":{"alertname":"Real","severity":"warning"}}]}`,
			"Real/-", "warning",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, _ := parseOne(t, adapters.KindAlertmanager, []byte(tt.payload), resolve(t, adapters.KindAlertmanager, adapters.Expressions{}))
			if ev.Name != tt.wantName || ev.Severity != tt.wantSeverity {
				t.Fatalf("name/severity = %q/%q, want %q/%q", ev.Name, ev.Severity, tt.wantName, tt.wantSeverity)
			}
		})
	}
}

func TestGrafanaDocumentedSample(t *testing.T) {
	body := readTestdata(t, "grafana_webhook.json")
	ev, stats := parseOne(t, adapters.KindGrafana, body, resolve(t, adapters.KindGrafana, adapters.Expressions{}))

	if ev.Source != adapters.KindGrafana || ev.Status != adapters.StatusFiring || ev.Key != "{}:{}" {
		t.Fatalf("source/status/key = %q/%q/%q", ev.Source, ev.Status, ev.Key)
	}
	// groupLabels is empty and commonLabels has no alertname: the first alert names the group.
	if ev.Name != "High memory usage/-" {
		t.Fatalf("name = %q", ev.Name)
	}
	if ev.Severity != "" {
		t.Fatalf("severity = %q, want empty (no severity labels)", ev.Severity)
	}
	if want := map[string]string{"team": "blue"}; !reflect.DeepEqual(ev.Labels, want) {
		t.Fatalf("labels = %v", ev.Labels)
	}
	if want := []string{"c6eadffa33fcdf37", "bc97ff14869b13e3"}; !reflect.DeepEqual(ev.Fingerprints, want) {
		t.Fatalf("fingerprints = %v, want %v", ev.Fingerprints, want)
	}
	if len(ev.Alerts) != 2 || stats.AlertsFiltered != 0 || stats.Errors() != 0 {
		t.Fatalf("alerts = %d stats = %+v", len(ev.Alerts), stats)
	}
	a := ev.Alerts[0]
	if a.StartsAt != "2021-10-12T09:51:03.157076+02:00" || a.Annotations["runbook_url"] != "https://myrunbook.com/runbook/1234" ||
		!strings.Contains(a.SilenceURL, "alertmanager=grafana") || a.GeneratorURL == "" {
		t.Fatalf("grafana alert fields not decoded: %+v", a)
	}
	// Grafana-only top-level fields stay reachable through the payload.
	v := ev.CELValue()
	payload, _ := v["payload"].(map[string]any)
	if payload["orgId"] != 1.0 || payload["state"] != "alerting" || payload["title"] != "[FIRING:2]  (blue)" {
		t.Fatalf("payload = %v", payload)
	}
}

func TestAlertParseErrors(t *testing.T) {
	tests := []struct{ name, body string }{
		{"not json", `{"status":`},
		{"trailing data", `{"status":"firing","groupKey":"k"} {}`},
		{"array", `[]`},
		{"missing status", `{"groupKey":"k","alerts":[]}`},
		{"unknown status", `{"status":"pending","groupKey":"k"}`},
		{"missing group key", `{"status":"firing","alerts":[]}`},
		{"non-string label", `{"status":"firing","groupKey":"k","alerts":[{"labels":{"a":1}}]}`},
		{"alerts not a list", `{"status":"firing","groupKey":"k","alerts":{}}`},
	}
	for _, kind := range []string{adapters.KindAlertmanager, adapters.KindGrafana} {
		for _, tt := range tests {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				events, _, err := adapters.Parse(kind, []byte(tt.body), adapters.Config{})
				if !errors.Is(err, adapters.ErrPayload) || events != nil {
					t.Fatalf("Parse = (%v, %v), want ErrPayload", events, err)
				}
			})
		}
	}
}

func fingerprintOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestGenericMappingWithEventList(t *testing.T) {
	cfg := resolve(t, adapters.KindGeneric, adapters.Expressions{Mapping: &adapters.MappingExpressions{
		Events:      `payload.items`,
		Key:         keyExpr,
		Title:       `payload.title`,
		Severity:    `payload.sev`,
		Status:      `payload.state == 'ok' ? 'resolved' : 'firing'`,
		Fingerprint: `payload.id + ':' + payload.rev`,
	}})
	body := []byte(`{"items":[
		{"id":"a","title":"Disk full","sev":"critical","state":"bad","rev":"1"},
		{"id":"b","title":"","sev":"warning","state":"ok","rev":"2"},
		{"title":"no id","sev":"info","state":"bad"},
		{"id":"d","title":"third","sev":"info","state":"bad","rev":"4"}
	]}`)
	events, stats, err := adapters.Parse(adapters.KindGeneric, body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// "no id" fails mapping.key; every event with a failing expression is dropped.
	if len(events) != 3 || stats.EventErrors != 1 || stats.Errors() != 1 || stats.LastError == nil {
		t.Fatalf("events = %d, stats = %+v", len(events), stats)
	}

	a, b, d := events[0], events[1], events[2]
	if a.Source != adapters.KindGeneric || a.Key != "a" || a.Name != "Disk full" || a.Severity != "critical" || a.Status != adapters.StatusFiring ||
		!reflect.DeepEqual(a.Fingerprints, []string{"a:1"}) || len(a.Alerts) != 0 || len(a.Labels) != 0 {
		t.Fatalf("event a = %+v", a)
	}
	if string(a.Payload) != `{"id":"a","rev":"1","sev":"critical","state":"bad","title":"Disk full"}` {
		t.Fatalf("payload = %s, want the canonical JSON of the element", a.Payload)
	}
	// Empty title falls back to the key; a resolved event carries no fingerprints.
	if b.Name != "b" || b.Status != "resolved" || len(b.Fingerprints) != 0 {
		t.Fatalf("event b = %+v", b)
	}
	// A "state" other than "ok" maps to firing.
	if d.Key != "d" || d.Status != adapters.StatusFiring {
		t.Fatalf("event d = %+v", d)
	}
}

func TestGenericComputedDefaults(t *testing.T) {
	cfg := resolve(t, adapters.KindGeneric, adapters.Expressions{Mapping: &adapters.MappingExpressions{Key: keyExpr}})
	ev, stats := parseOne(t, adapters.KindGeneric, []byte(`{"id": "x", "n": 1}`), cfg)

	canonical := `{"id":"x","n":1}`
	want := adapters.Event{
		Source:       adapters.KindGeneric,
		Key:          "x",
		Name:         "x", // title default '' -> key
		Status:       adapters.StatusFiring,
		Severity:     "info",
		Labels:       map[string]string{},
		Alerts:       []adapters.Alert{},
		Fingerprints: []string{fingerprintOf(canonical)},
		Payload:      []byte(canonical),
	}
	if !reflect.DeepEqual(ev, want) {
		t.Fatalf("event = %+v\nwant  %+v", ev, want)
	}
	if stats != (adapters.Stats{}) {
		t.Fatalf("stats = %+v", stats)
	}

	// The same defaults apply when Parse gets a Mapping with only Key, without Resolve.
	key, err := celx.Compile(celx.RoleMappingKey, keyExpr)
	if err != nil {
		t.Fatal(err)
	}
	ev2, _ := parseOne(t, adapters.KindGeneric, []byte(`{"id": "x", "n": 1}`), adapters.Config{Mapping: &adapters.Mapping{Key: key}})
	if !reflect.DeepEqual(ev2, want) {
		t.Fatalf("event without Resolve = %+v", ev2)
	}
}

func TestGenericDroppedEvents(t *testing.T) {
	tests := []struct {
		name    string
		mapping adapters.MappingExpressions
		body    string
	}{
		{"missing key field", adapters.MappingExpressions{Key: keyExpr}, `{"other":1}`},
		{"key is a number", adapters.MappingExpressions{Key: keyExpr}, `{"id":5}`},
		{"empty key", adapters.MappingExpressions{Key: keyExpr}, `{"id":""}`},
		{"invalid status", adapters.MappingExpressions{Key: keyExpr, Status: `'warning'`}, bodyA},
		{"title error", adapters.MappingExpressions{Key: keyExpr, Title: missingExpr}, bodyA},
		{"severity error", adapters.MappingExpressions{Key: keyExpr, Severity: missingExpr}, bodyA},
		{"fingerprint error", adapters.MappingExpressions{Key: keyExpr, Fingerprint: missingExpr}, bodyA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := resolve(t, adapters.KindGeneric, adapters.Expressions{Mapping: &tt.mapping})
			events, stats, err := adapters.Parse(adapters.KindGeneric, []byte(tt.body), cfg)
			if err != nil || len(events) != 0 || stats.EventErrors != 1 || stats.LastError == nil {
				t.Fatalf("Parse = (%v, %+v, %v), want one dropped event", events, stats, err)
			}
		})
	}

	t.Run("fingerprint is not evaluated for resolved events", func(t *testing.T) {
		cfg := resolve(t, adapters.KindGeneric, adapters.Expressions{Mapping: &adapters.MappingExpressions{
			Key: keyExpr, Status: `'resolved'`, Fingerprint: missingExpr}})
		events, stats, err := adapters.Parse(adapters.KindGeneric, []byte(bodyA), cfg)
		if err != nil || len(events) != 1 || stats.Errors() != 0 {
			t.Fatalf("Parse = (%v, %+v, %v)", events, stats, err)
		}
	})
	t.Run("empty fingerprint leaves a firing event without fingerprints", func(t *testing.T) {
		cfg := resolve(t, adapters.KindGeneric, adapters.Expressions{Mapping: &adapters.MappingExpressions{Key: keyExpr, Fingerprint: `''`}})
		ev, _ := parseOne(t, adapters.KindGeneric, []byte(bodyA), cfg)
		if len(ev.Fingerprints) != 0 {
			t.Fatalf("fingerprints = %v", ev.Fingerprints)
		}
	})
}

func TestGenericTitleTruncation(t *testing.T) {
	cfg := resolve(t, adapters.KindGeneric, adapters.Expressions{Mapping: &adapters.MappingExpressions{Key: keyExpr, Title: `payload.t`}})
	tests := []struct {
		name, title string
		wantRunes   int
	}{
		{"short", "short", 5},
		{"exactly 120", strings.Repeat("a", 120), 120},
		{"long ascii", strings.Repeat("a", 500), 120},
		{"multibyte cut on characters", strings.Repeat("é", 130), 120},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Appendf(nil, `{"id":"k","t":%q}`, tt.title)
			ev, _ := parseOne(t, adapters.KindGeneric, body, cfg)
			if got := len([]rune(ev.Name)); got != tt.wantRunes || strings.ContainsRune(ev.Name, '\uFFFD') {
				t.Fatalf("name has %d runes (%q), want %d", got, ev.Name, tt.wantRunes)
			}
		})
	}
}

func TestGenericPayloadErrors(t *testing.T) {
	cfg := resolve(t, adapters.KindGeneric, adapters.Expressions{Mapping: &adapters.MappingExpressions{Events: `payload.items`, Key: keyExpr}})
	tests := []struct{ name, body string }{
		{"not json", `nope`},
		{"events field missing", `{"other":1}`},
		{"events is not a list", `{"items":"x"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, _, err := adapters.Parse(adapters.KindGeneric, []byte(tt.body), cfg)
			if !errors.Is(err, adapters.ErrPayload) || events != nil {
				t.Fatalf("Parse = (%v, %v), want ErrPayload", events, err)
			}
		})
	}

	t.Run("empty list is no events", func(t *testing.T) {
		events, stats, err := adapters.Parse(adapters.KindGeneric, []byte(`{"items":[]}`), cfg)
		if err != nil || len(events) != 0 || stats.Errors() != 0 {
			t.Fatalf("Parse = (%v, %+v, %v)", events, stats, err)
		}
	})
}

func TestParseConfigErrors(t *testing.T) {
	if _, _, err := adapters.Parse("nope", []byte(`{}`), adapters.Config{}); !errors.Is(err, adapters.ErrConfig) {
		t.Fatalf("unknown kind error = %v", err)
	}
	if _, _, err := adapters.Parse(adapters.KindGeneric, []byte(`{}`), adapters.Config{}); !errors.Is(err, adapters.ErrConfig) {
		t.Fatalf("generic without mapping error = %v", err)
	}
}

func TestResolveErrors(t *testing.T) {
	tests := []struct {
		name string
		kind string
		x    adapters.Expressions
		want string
	}{
		{"bad alertFilter", adapters.KindAlertmanager, adapters.Expressions{AlertFilter: "alert.labels.x +"}, "webhook.alertFilter:"},
		{"alertFilter wrong type", adapters.KindGrafana, adapters.Expressions{AlertFilter: "'x'"}, "webhook.alertFilter: expression must evaluate to bool"},
		{"bad filter", adapters.KindAlertmanager, adapters.Expressions{Filter: "payload.x"}, "webhook.filter:"},
		{"generic without mapping", adapters.KindGeneric, adapters.Expressions{}, "webhook.mapping.key: required"},
		{"generic without key", adapters.KindGeneric, adapters.Expressions{Mapping: &adapters.MappingExpressions{Title: "'t'"}}, "webhook.mapping.key: required"},
		{"mapping title wrong type", adapters.KindGeneric, adapters.Expressions{Mapping: &adapters.MappingExpressions{Key: keyExpr, Title: "1"}}, "webhook.mapping.title:"},
		{"mapping events wrong type", adapters.KindGeneric, adapters.Expressions{Mapping: &adapters.MappingExpressions{Key: keyExpr, Events: "payload.id == 1"}}, "webhook.mapping.events:"},
		{"unknown adapter", "smtp", adapters.Expressions{}, "unknown adapter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := adapters.Resolve(tt.kind, tt.x, celx.Compile)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Resolve error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestMatchEvent(t *testing.T) {
	ev, _ := parseOne(t, adapters.KindAlertmanager, readTestdata(t, "alertmanager_v4.json"),
		resolve(t, adapters.KindAlertmanager, adapters.Expressions{}))

	tests := []struct {
		name    string
		filter  string
		want    bool
		wantErr bool
	}{
		{"default matches", "", true, false},
		{"severity", `event.severity == 'critical'`, true, false},
		{"labels and strings", `event.labels.team.startsWith('co') && event.name.endsWith('/prod')`, true, false},
		{"fingerprints", `size(event.fingerprints) == 2 && 'aaaa111122223333' in event.fingerprints`, true, false},
		{"alerts", `event.alerts.exists(a, a.labels.pod == 'api-2' && a.status == 'firing')`, true, false},
		{"payload", `event.payload.receiver == 'omp' && event.payload.version == '4'`, true, false},
		{"no match", `event.severity == 'info'`, false, false},
		{"runtime error is false", `event.labels['nope'] == 'x'`, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := resolve(t, adapters.KindAlertmanager, adapters.Expressions{Filter: tt.filter})
			got, err := cfg.MatchEvent(ev)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("MatchEvent = (%v, %v), want (%v, err=%v)", got, err, tt.want, tt.wantErr)
			}
		})
	}

	if got, err := (adapters.Config{}).MatchEvent(ev); !got || err != nil {
		t.Fatalf("nil Filter = (%v, %v), want (true, nil)", got, err)
	}
}

func TestResolveUsesCache(t *testing.T) {
	c := celx.NewCache()
	a, err := adapters.Resolve(adapters.KindAlertmanager, adapters.Expressions{}, c.Compiler("ns", "t", 3))
	if err != nil {
		t.Fatal(err)
	}
	b, err := adapters.Resolve(adapters.KindAlertmanager, adapters.Expressions{}, c.Compiler("ns", "t", 3))
	if err != nil {
		t.Fatal(err)
	}
	if a.AlertFilter != b.AlertFilter || a.Filter != b.Filter {
		t.Fatal("same generation must reuse the cached programs")
	}
	if a.AlertFilter.Expr() != adapters.DefaultAlertFilter {
		t.Fatalf("default alertFilter = %q", a.AlertFilter.Expr())
	}
}
