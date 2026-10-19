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
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// minSecretLen is the shortest value the redactor scrubs; shorter values would
// mangle ordinary text.
const minSecretLen = 8

// Redactor rewrites known secret values to [redacted:<name>].
type Redactor struct {
	r *strings.Replacer
}

// NewRedactor builds the replacer for sources. Env values are read with getenv and file
// contents with readFile (surrounding whitespace trimmed). Values shorter than 8
// characters, unset variables and unreadable files are skipped. Each value also
// matches in its JSON-escaped form, so a multi-line secret inside a JSON document is caught.
func NewRedactor(sources []agentapi.RedactSource, getenv func(string) string, readFile func(string) ([]byte, error)) *Redactor {
	type pair struct{ value, name string }
	var pairs []pair
	add := func(value, name string) {
		value = strings.TrimSpace(value)
		if len(value) < minSecretLen {
			return
		}
		pairs = append(pairs, pair{value, name})
		if escaped := jsonEscape(value); escaped != value {
			pairs = append(pairs, pair{escaped, name})
		}
	}
	for _, s := range sources {
		name := s.Name
		if name == "" {
			name = firstNonEmpty(s.Env, s.File)
		}
		if s.Env != "" {
			add(getenv(s.Env), name)
		}
		if s.File != "" {
			if raw, err := readFile(s.File); err == nil {
				add(string(raw), name)
				if s.Lines {
					for line := range strings.Lines(string(raw)) {
						if value, ok := lineValue(line); ok {
							add(value, name)
						}
					}
				}
			}
		}
	}
	// strings.Replacer prefers earlier pairs at one position: longest values first.
	slices.SortStableFunc(pairs, func(a, b pair) int { return len(b.value) - len(a.value) })
	args := make([]string, 0, 2*len(pairs))
	seen := map[string]bool{}
	for _, p := range pairs {
		if seen[p.value] {
			continue
		}
		seen[p.value] = true
		args = append(args, p.value, "[redacted:"+p.name+"]")
	}
	if len(args) == 0 {
		return &Redactor{}
	}
	return &Redactor{r: strings.NewReplacer(args...)}
}

// OSRedactor builds a Redactor over the process environment and filesystem.
func OSRedactor(sources []agentapi.RedactSource) *Redactor {
	return NewRedactor(sources, os.Getenv, os.ReadFile)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// lineValue is the value of a key=value or key: value line: what follows the first '=' or ':',
// trimmed and with one pair of surrounding quotes removed.
func lineValue(line string) (string, bool) {
	_, value, ok := strings.Cut(line, "=")
	if colon := strings.IndexByte(line, ':'); colon >= 0 && (!ok || colon < strings.IndexByte(line, '=')) {
		value, ok = line[colon+1:], true
	}
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
		value = value[1 : n-1]
	}
	return value, true
}

// jsonEscape returns v as it appears inside a JSON string literal.
func jsonEscape(v string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return v
	}
	out := strings.TrimSuffix(buf.String(), "\n")
	return out[1 : len(out)-1]
}

// String redacts s.
func (r *Redactor) String(s string) string {
	if r == nil || r.r == nil {
		return s
	}
	return r.r.Replace(s)
}

// Bytes redacts b.
func (r *Redactor) Bytes(b []byte) []byte {
	if r == nil || r.r == nil {
		return b
	}
	return []byte(r.r.Replace(string(b)))
}

// Raw redacts a JSON document in place of its text; the replacement never contains JSON metacharacters.
func (r *Redactor) Raw(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(r.Bytes(raw))
}

// Writer returns a writer that redacts line by line before writing to w. Close flushes
// a trailing partial line.
func (r *Redactor) Writer(w io.Writer) io.WriteCloser {
	return &lineRedactor{r: r, w: w}
}

// maxLine bounds how much of an unterminated line is buffered.
const maxLine = 64 << 10

type lineRedactor struct {
	r   *Redactor
	w   io.Writer
	mu  sync.Mutex
	buf []byte
}

func (l *lineRedactor) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if err := l.emit(l.buf[:i+1]); err != nil {
			return len(p), err
		}
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > maxLine {
		err := l.emit(l.buf)
		l.buf = nil
		return len(p), err
	}
	return len(p), nil
}

func (l *lineRedactor) emit(b []byte) error {
	_, err := l.w.Write(l.r.Bytes(b))
	return err
}

func (l *lineRedactor) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) == 0 {
		return nil
	}
	err := l.emit(l.buf)
	l.buf = nil
	return err
}
