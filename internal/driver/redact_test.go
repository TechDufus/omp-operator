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

package driver

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

func TestRedactor(t *testing.T) {
	env := map[string]string{
		"TOKEN":  "abcdefgh-12345",
		"SHORT":  "abc",
		"PREFIX": "abcdefgh",
	}
	files := map[string]string{"/key": "line1-of-key\nline2-of-key\n"}
	r := NewRedactor([]agentapi.RedactSource{
		{Name: "long", Env: "TOKEN"},
		{Name: "short", Env: "SHORT"},
		{Name: "prefix", Env: "PREFIX"},
		{Name: "unset", Env: "NOPE"},
		{Name: "key", File: "/key"},
		{Name: "missing", File: "/missing"},
	}, func(k string) string { return env[k] }, func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	})

	tests := map[string]string{
		"plain abcdefgh-12345 here":       "plain [redacted:long] here", // the longer value wins over its prefix
		"just abcdefgh":                   "just [redacted:prefix]",
		"abc is too short":                "abc is too short",
		"key: line1-of-key\nline2-of-key": "key: [redacted:key]",
	}
	for in, want := range tests {
		if got := r.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}

	// A multi-line secret inside a JSON document is JSON-escaped; it is still found.
	doc, _ := json.Marshal(map[string]string{"k": "line1-of-key\nline2-of-key"})
	if got := string(r.Raw(doc)); got != `{"k":"[redacted:key]"}` {
		t.Errorf("Raw = %s", got)
	}
}

func TestRedactorLinesSources(t *testing.T) {
	dotenv := "# comment\nAPI_KEY=abcdefghij-key\nexport PASSWORD=\"quoted pass phrase\"\nyaml_key: 'single-quoted-value'\n" +
		"SHORT=abc\nURL=postgres://u:p@host/db?x=1\nno separator line here\nEMPTY=\n"
	read := func(string) ([]byte, error) { return []byte(dotenv), nil }
	none := func(string) string { return "" }

	lines := NewRedactor([]agentapi.RedactSource{{Name: "dotenv", File: "/ws/.env", Lines: true}}, none, read)
	tests := map[string]string{
		"key abcdefghij-key used":           "key [redacted:dotenv] used",
		"pw quoted pass phrase used":        "pw [redacted:dotenv] used",
		"y single-quoted-value used":        "y [redacted:dotenv] used",
		"conn postgres://u:p@host/db?x=1":   "conn [redacted:dotenv]",
		"short abc stays":                   "short abc stays",
		"the comment and EMPTY stay intact": "the comment and EMPTY stay intact",
	}
	for in, want := range tests {
		if got := lines.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}

	// Without Lines only the whole file is a secret.
	whole := NewRedactor([]agentapi.RedactSource{{Name: "dotenv", File: "/ws/.env"}}, none, read)
	if got := whole.String("key abcdefghij-key"); got != "key abcdefghij-key" {
		t.Errorf("a source without Lines must not scrub single values, got %q", got)
	}
}

func TestRedactorEmptyIsPassthrough(t *testing.T) {
	var nilRedactor *Redactor
	for _, r := range []*Redactor{nilRedactor, {}} {
		if got := r.String("anything"); got != "anything" {
			t.Fatalf("got %q", got)
		}
		if got := string(r.Bytes([]byte("x"))); got != "x" {
			t.Fatalf("got %q", got)
		}
	}
}

func TestRedactorWriterRedactsAcrossWrites(t *testing.T) {
	r := NewRedactor([]agentapi.RedactSource{{Name: "t", Env: "T"}},
		func(string) string { return "supersecret-value" }, nil)
	var out bytes.Buffer
	w := r.Writer(&out)
	// The secret is split over two writes; line buffering must still catch it.
	for _, chunk := range []string{"token=supers", "ecret-value\nnext line\npartial supersecret", "-value"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "token=[redacted:t]\nnext line\npartial [redacted:t]"; got != want {
		t.Fatalf("out = %q, want %q", got, want)
	}
}

func TestRedactorDefaultsNameToSource(t *testing.T) {
	r := NewRedactor([]agentapi.RedactSource{{Env: "X"}},
		func(string) string { return "0123456789" }, nil)
	if got := r.String("0123456789"); got != "[redacted:X]" {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(r.String("0123456789"), "0123456789") {
		t.Fatal("leak")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("héllo wörld", 5); got != "héllo" {
		t.Fatalf("got %q", got)
	}
	if got := truncate("short", 50); got != "short" {
		t.Fatalf("got %q", got)
	}
}
