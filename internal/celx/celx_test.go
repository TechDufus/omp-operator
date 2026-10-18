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

package celx_test

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/TechDufus/omp-operator/internal/celx"
)

const (
	fieldLabels    = "labels"
	labelAlertname = "alertname"
	fieldName      = "name"
	fieldItems     = "items"
)

// defaultAlertFilter is the plan's computed default for alertFilter.
const defaultAlertFilter = `!(alert.labels.?alertname.orValue('') in ['Watchdog','InfoInhibitor'])`

func mustCompile(t *testing.T, role celx.Role, expr string) *celx.Program {
	t.Helper()
	p, err := celx.Compile(role, expr)
	if err != nil {
		t.Fatalf("Compile(%s, %q): %v", role, expr, err)
	}
	return p
}

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name string
		role celx.Role
		expr string
		want string
	}{
		{"empty", celx.RoleEventFilter, "", "expression is empty"},
		{"syntax", celx.RoleEventFilter, "event.name ==", "filter:"},
		{"undeclared variable", celx.RoleEventFilter, "alert.status == 'firing'", "undeclared reference to 'alert'"},
		{"event variable not in alert env", celx.RoleAlertFilter, "event.name == 'x'", "undeclared reference to 'event'"},
		{"payload variable not in event env", celx.RoleEventFilter, "payload.a == 'x'", "undeclared reference to 'payload'"},
		{"string literal for bool", celx.RoleAlertFilter, "'yes'", "must evaluate to bool, got string"},
		{"int for string", celx.RoleMappingKey, "1", "must evaluate to string, got int"},
		{"string for list", celx.RoleMappingEvents, "'x'", "must evaluate to list, got string"},
		{"bool for list", celx.RoleMappingEvents, "payload != null", "must evaluate to list, got bool"},
		{"unknown role", celx.Role("nope"), "true", "unknown expression role"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := celx.Compile(tt.role, tt.expr)
			if err == nil {
				t.Fatalf("Compile(%s, %q) succeeded, want error containing %q", tt.role, tt.expr, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestDynResultMustMatchAtRuntime(t *testing.T) {
	filter := mustCompile(t, celx.RoleEventFilter, "event.name")
	got, err := filter.Bool(map[string]any{fieldName: "x"})
	if got || err == nil || !strings.Contains(err.Error(), "want bool") {
		t.Fatalf("Bool = (%v, %v), want (false, error mentioning bool)", got, err)
	}
	key := mustCompile(t, celx.RoleMappingKey, "payload.id")
	if _, err := key.String(map[string]any{"id": 5.0}); err == nil {
		t.Fatal("numeric key must be an error, got nil")
	}
	if s, err := key.String(map[string]any{"id": "abc"}); err != nil || s != "abc" {
		t.Fatalf("String = (%q, %v), want abc", s, err)
	}
}

func TestProgramRoleMisuse(t *testing.T) {
	p := mustCompile(t, celx.RoleMappingKey, "'k'")
	if _, err := p.Bool(nil); err == nil {
		t.Fatal("Bool on a string program must fail")
	}
	if _, err := p.List(nil); err == nil {
		t.Fatal("List on a string program must fail")
	}
}

func TestOptionalLabels(t *testing.T) {
	filter := mustCompile(t, celx.RoleAlertFilter, defaultAlertFilter)
	tests := []struct {
		name  string
		alert map[string]any
		want  bool
	}{
		{"normal alert", map[string]any{fieldLabels: map[string]any{labelAlertname: "HighCPU"}}, true},
		{"watchdog dropped", map[string]any{fieldLabels: map[string]any{labelAlertname: "Watchdog"}}, false},
		{"inhibitor dropped", map[string]any{fieldLabels: map[string]any{labelAlertname: "InfoInhibitor"}}, false},
		{"no alertname label kept", map[string]any{fieldLabels: map[string]any{"team": "a"}}, true},
		{"empty labels kept", map[string]any{fieldLabels: map[string]any{}}, true},
		{"typed string map", map[string]any{fieldLabels: map[string]string{labelAlertname: "Watchdog"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := filter.Bool(tt.alert)
			if err != nil || got != tt.want {
				t.Fatalf("Bool = (%v, %v), want (%v, nil)", got, err, tt.want)
			}
		})
	}
}

func TestStringsExtension(t *testing.T) {
	filter := mustCompile(t, celx.RoleEventFilter, "event.name.lowerAscii().startsWith('highcpu') && event.labels.?team.orValue('') == 'a'")
	got, err := filter.Bool(map[string]any{fieldName: "HighCPU/prod", fieldLabels: map[string]any{"team": "a"}})
	if err != nil || !got {
		t.Fatalf("Bool = (%v, %v), want (true, nil)", got, err)
	}
}

func TestRuntimeErrorIsFalse(t *testing.T) {
	filter := mustCompile(t, celx.RoleEventFilter, "event.labels['missing'] == 'x'")
	got, err := filter.Bool(map[string]any{fieldLabels: map[string]any{}})
	if got || err == nil {
		t.Fatalf("Bool = (%v, %v), want (false, error)", got, err)
	}
	if !strings.HasPrefix(err.Error(), "filter:") {
		t.Fatalf("error %q should name its role", err)
	}
}

func TestCostLimit(t *testing.T) {
	items := make([]any, 100)
	for i := range items {
		items[i] = float64(i)
	}
	payload := map[string]any{fieldItems: items}

	cheap := mustCompile(t, celx.RoleMappingEvents, "payload.items.map(i, {'id': i})")
	if got, err := cheap.List(payload); err != nil || len(got) != 100 {
		t.Fatalf("cheap List = (%d items, %v), want 100 items", len(got), err)
	}

	// 100^3 comparisons far exceeds CostLimit.
	expensive := mustCompile(t, celx.RoleMappingEvents,
		"payload.items.all(a, payload.items.all(b, payload.items.all(c, a >= 0 && b >= 0 && c >= 0))) ? [payload] : []")
	if _, err := expensive.List(payload); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expensive List error = %v, want a cost-limit error", err)
	}

	// For a filter the cost error evaluates to false.
	filter := mustCompile(t, celx.RoleEventFilter,
		"event.payload.items.all(a, event.payload.items.all(b, event.payload.items.all(c, true)))")
	got, err := filter.Bool(map[string]any{"payload": payload})
	if got || err == nil {
		t.Fatalf("expensive filter = (%v, %v), want (false, error)", got, err)
	}
}

func TestListResultIsJSONLike(t *testing.T) {
	p := mustCompile(t, celx.RoleMappingEvents, "payload.items.map(i, {'id': i.id, 'tags': [i.id, 'x'], 'ok': true})")
	got, err := p.List(map[string]any{fieldItems: []any{
		map[string]any{"id": "a"},
		map[string]any{"id": "b"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"id": "a", "tags": []any{"a", "x"}, "ok": true},
		map[string]any{"id": "b", "tags": []any{"b", "x"}, "ok": true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %#v, want %#v", got, want)
	}

	// Plain passthrough keeps the original element shape.
	one := mustCompile(t, celx.RoleMappingEvents, "[payload]")
	in := map[string]any{"n": 1.5, "s": []any{"x"}, "z": nil}
	got, err = one.List(in)
	if err != nil || !reflect.DeepEqual(got, []any{in}) {
		t.Fatalf("[payload] = (%#v, %v), want %#v", got, err, []any{in})
	}
}

func TestListRuntimeErrors(t *testing.T) {
	p := mustCompile(t, celx.RoleMappingEvents, "payload.items")
	if _, err := p.List(map[string]any{"other": 1}); err == nil {
		t.Fatal("missing field must be an error")
	}
	if _, err := p.List(map[string]any{fieldItems: "not a list"}); err == nil || !strings.Contains(err.Error(), "list") {
		t.Fatalf("non-list result error = %v, want one mentioning list", err)
	}
}

func TestCache(t *testing.T) {
	c := celx.NewCache()
	compile := func(gen int64) celx.Compiler { return c.Compiler("ns", "trig", gen) }

	a, err := compile(1)(celx.RoleEventFilter, "true")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := compile(1)(celx.RoleEventFilter, "true"); b != a {
		t.Fatal("same generation, role and expression must return the cached program")
	}
	if b, _ := compile(1)(celx.RoleAlertFilter, "true"); b == a {
		t.Fatal("a different role must not share a program")
	}
	if b, _ := compile(1)(celx.RoleEventFilter, "false"); b == a {
		t.Fatal("a changed expression must recompile")
	}

	// A newer generation replaces all roles of the old one.
	old, _ := compile(1)(celx.RoleEventFilter, "false")
	n, _ := compile(2)(celx.RoleEventFilter, "false")
	if n == old {
		t.Fatal("a new generation must recompile")
	}
	if again, _ := compile(2)(celx.RoleEventFilter, "false"); again != n {
		t.Fatal("generation 2 must be cached")
	}

	// A stale generation is compiled but never replaces the newer entry.
	stale, err := compile(1)(celx.RoleEventFilter, "false")
	if err != nil || stale == n {
		t.Fatalf("stale compile = (%p, %v), want a fresh program", stale, err)
	}
	if again, _ := compile(2)(celx.RoleEventFilter, "false"); again != n {
		t.Fatal("a stale request must not evict the newer generation")
	}
	// Roles cached under generation 1 are gone after moving to generation 2.
	if again, _ := compile(2)(celx.RoleAlertFilter, "true"); again == a {
		t.Fatal("old-generation programs must be dropped")
	}

	// Errors are not cached and are reported.
	if _, err := compile(2)(celx.RoleEventFilter, "event.x +"); err == nil {
		t.Fatal("expected compile error")
	}

	c.Forget("ns", "trig")
	if after, _ := compile(2)(celx.RoleEventFilter, "false"); after == n {
		t.Fatal("Forget must drop cached programs")
	}
}

func TestCacheConcurrent(t *testing.T) {
	c := celx.NewCache()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 50 {
				gen := int64(i % 5)
				compile := c.Compiler("ns", fmt.Sprintf("t%d", g%2), gen)
				p, err := compile(celx.RoleEventFilter, "event.name == 'x'")
				if err != nil {
					t.Error(err)
					return
				}
				if got, err := p.Bool(map[string]any{fieldName: "x"}); err != nil || !got {
					t.Errorf("Bool = (%v, %v)", got, err)
					return
				}
				if i%17 == 0 {
					c.Forget("ns", "t0")
				}
			}
		})
	}
	wg.Wait()
}
