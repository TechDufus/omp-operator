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

package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func readAll(t *testing.T, f *os.File, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestContinuityID(t *testing.T) {
	// sha256("a") = ca978112ca1bbdcafac231b39a23dc4d...
	if got := ContinuityID("a"); got != "ca978112ca1bbdca" {
		t.Fatalf("ContinuityID(a) = %q", got)
	}
	if ContinuityID("a") == ContinuityID("b") {
		t.Fatal("distinct keys collided")
	}
}

func TestTraversalRefused(t *testing.T) {
	s, dir := newStore(t)
	bad := []string{"", ".", "..", "../x", "a/b", `a\b`, "a\x00b", "/etc/passwd", strings.Repeat("a", 256)}
	for _, name := range bad {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			r := strings.NewReader("x")
			checks := map[string]error{
				"AppendEvents":        s.AppendEvents(name, []Event{{Kind: "k"}}),
				"WriteTranscript":     s.WriteTranscript(name, r),
				"WriteReport":         s.WriteReport(name, "x"),
				"RemoveSession":       s.RemoveSession(name),
				"PutContinuity ns":    s.PutContinuity(name, "a", "id", r),
				"PutContinuity agent": s.PutContinuity("ns", name, "id", r),
				"PutContinuity id":    s.PutContinuity("ns", "a", name, r),
				"RemoveContinuity":    s.RemoveContinuity("ns", "a", name),
				"PutToolchain ns":     s.PutToolchain(name, "a", "h", r),
				"PutToolchain agent":  s.PutToolchain("ns", name, "h", r),
				"PutToolchain hash":   s.PutToolchain("ns", "a", name, r),
				"RemoveAgent ns":      s.RemoveAgent(name, "a"),
				"RemoveAgent agent":   s.RemoveAgent("ns", name),
			}
			_, checks["ReadEvents"] = s.ReadEvents(name, 1)
			_, checks["OpenTranscript"] = s.OpenTranscript(name)
			_, checks["ReadReport"] = s.ReadReport(name)
			_, checks["OpenContinuity"] = s.OpenContinuity("ns", "a", name)
			_, checks["OpenToolchain"] = s.OpenToolchain("ns", "a", name)
			for op, err := range checks {
				if !errors.Is(err, ErrInvalidName) {
					t.Errorf("%s(%q) = %v, want ErrInvalidName", op, name, err)
				}
			}
		})
	}
	// Nothing was created in the root by refused operations.
	if names := listNames(t, dir); len(names) != 0 {
		t.Fatalf("root not empty after refused operations: %v", names)
	}
}

func TestEventsCapAtMax(t *testing.T) {
	s, dir := newStore(t)
	const uid = "uid-1"
	seq := 0
	batch := func(n int) []Event {
		evs := make([]Event, n)
		for i := range evs {
			evs[i] = Event{At: time.Unix(int64(seq), 0).UTC(), Kind: "k", Text: fmt.Sprintf("e%d", seq)}
			seq++
		}
		return evs
	}
	for _, n := range []int{4990, 5, 5, 1, 7, 3, 12000} {
		if err := s.AppendEvents(uid, batch(n)); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "sessions", uid, "events.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if got := bytes.Count(data, []byte{'\n'}); got != min(seq, MaxEventLines) {
			t.Fatalf("after %d events appended: %d lines, want %d", seq, got, min(seq, MaxEventLines))
		}
	}
	all, err := s.ReadEvents(uid, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != MaxEventLines {
		t.Fatalf("ReadEvents returned %d, want %d", len(all), MaxEventLines)
	}
	if all[0].Text != fmt.Sprintf("e%d", seq-MaxEventLines) || all[len(all)-1].Text != fmt.Sprintf("e%d", seq-1) {
		t.Fatalf("kept range wrong: first %q last %q (seq %d)", all[0].Text, all[len(all)-1].Text, seq)
	}
	last, err := s.ReadEvents(uid, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 3 || last[0].Text != fmt.Sprintf("e%d", seq-3) || last[2].Text != fmt.Sprintf("e%d", seq-1) {
		t.Fatalf("last 3 = %+v", last)
	}

	// A fresh Store over the same directory recounts and keeps the cap.
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.AppendEvents(uid, batch(2)); err != nil {
		t.Fatal(err)
	}
	all, _ = s2.ReadEvents(uid, 100000)
	if len(all) != MaxEventLines || all[len(all)-1].Text != fmt.Sprintf("e%d", seq-1) {
		t.Fatalf("after reopen: %d events, last %q", len(all), all[len(all)-1].Text)
	}
}

func TestEventsMissingAndMalformed(t *testing.T) {
	s, dir := newStore(t)
	if evs, err := s.ReadEvents("none", 10); err != nil || evs != nil {
		t.Fatalf("ReadEvents on missing session = %v, %v", evs, err)
	}
	if err := s.AppendEvents("u", []Event{{Kind: "a", Text: "line1\nline2"}}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "sessions", "u", "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("not json\n")
	_ = f.Close()
	evs, err := s.ReadEvents("u", 10)
	if err != nil || len(evs) != 1 || evs[0].Text != "line1\nline2" {
		t.Fatalf("got %+v, %v", evs, err)
	}
}

func TestEventsConcurrentAppend(t *testing.T) {
	s, _ := newStore(t)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 100 {
				if err := s.AppendEvents("u", []Event{{Kind: "k", Text: fmt.Sprintf("%d-%d", w, i)}}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	evs, err := s.ReadEvents("u", 10000)
	if err != nil || len(evs) != 800 {
		t.Fatalf("got %d events, err %v, want 800", len(evs), err)
	}
}

func TestSessionFilesAndRemove(t *testing.T) {
	s, dir := newStore(t)
	const uid = "u1"
	if err := s.WriteTranscript(uid, strings.NewReader("<html>t</html>")); err != nil {
		t.Fatal(err)
	}
	f, err := s.OpenTranscript(uid)
	if got := readAll(t, f, err); got != "<html>t</html>" {
		t.Fatalf("transcript = %q", got)
	}
	if err := s.WriteReport(uid, "# report"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadReport(uid); err != nil || got != "# report" {
		t.Fatalf("report = %q, %v", got, err)
	}
	if err := s.AppendEvents(uid, []Event{{Kind: "k"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenTranscript("other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing transcript err = %v", err)
	}
	if _, err := s.ReadReport("other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing report err = %v", err)
	}

	if err := s.RemoveSession(uid); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions", uid)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session dir still present: %v", err)
	}
	if err := s.RemoveSession(uid); err != nil {
		t.Fatalf("removing missing session: %v", err)
	}
	// Appending after removal starts a fresh log with a correct count.
	if err := s.AppendEvents(uid, []Event{{Kind: "k"}}); err != nil {
		t.Fatal(err)
	}
	if evs, _ := s.ReadEvents(uid, 10); len(evs) != 1 {
		t.Fatalf("events after remove = %d", len(evs))
	}
}

type failingReader struct {
	data []byte
	sent bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.sent {
		return 0, errors.New("boom")
	}
	f.sent = true
	return copy(p, f.data), nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestAtomicWrites(t *testing.T) {
	s, dir := newStore(t)
	const uid = "u1"
	sdir := filepath.Join(dir, "sessions", uid)
	if err := s.WriteTranscript(uid, strings.NewReader("good")); err != nil {
		t.Fatal(err)
	}

	// A read error mid-stream keeps the previous file and leaves no temp files.
	if err := s.WriteTranscript(uid, &failingReader{data: []byte("partial")}); err == nil {
		t.Fatal("expected read error")
	}
	f, err := s.OpenTranscript(uid)
	if got := readAll(t, f, err); got != "good" {
		t.Fatalf("transcript after failed write = %q", got)
	}
	if names := listNames(t, sdir); len(names) != 1 || names[0] != "transcript.html" {
		t.Fatalf("session dir = %v", names)
	}

	// Oversized input is refused and nothing changes.
	big := io.LimitReader(zeroReader{}, MaxTranscriptBytes+1)
	if err := s.WriteTranscript(uid, big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize err = %v", err)
	}
	f, err = s.OpenTranscript(uid)
	if got := readAll(t, f, err); got != "good" {
		t.Fatalf("transcript after oversize write has %d bytes", len(got))
	}
	if names := listNames(t, sdir); len(names) != 1 {
		t.Fatalf("temp file left behind: %v", names)
	}

	// Exactly at the limit is accepted.
	if err := s.WriteTranscript(uid, io.LimitReader(zeroReader{}, MaxTranscriptBytes)); err != nil {
		t.Fatalf("at-limit write: %v", err)
	}
	if st, err := os.Stat(filepath.Join(sdir, "transcript.html")); err != nil || st.Size() != MaxTranscriptBytes {
		t.Fatalf("stat = %v, %v", st, err)
	}
}

func TestContinuityRoundTrip(t *testing.T) {
	s, dir := newStore(t)
	id := ContinuityID("trigger/abc")
	if _, err := s.OpenContinuity("ns", "agent", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open missing = %v", err)
	}
	if err := s.PutContinuity("ns", "agent", id, strings.NewReader("v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutContinuity("ns", "agent", id, strings.NewReader("v2")); err != nil {
		t.Fatal(err)
	}
	other := ContinuityID("other")
	if err := s.PutContinuity("ns", "agent", other, strings.NewReader("o")); err != nil {
		t.Fatal(err)
	}
	f, err := s.OpenContinuity("ns", "agent", id)
	if got := readAll(t, f, err); got != "v2" {
		t.Fatalf("continuity = %q", got)
	}
	if names := listNames(t, filepath.Join(dir, "continuity", "ns", "agent")); len(names) != 2 {
		t.Fatalf("files = %v", names)
	}

	if err := s.RemoveContinuity("ns", "agent", id); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveContinuity("ns", "agent", id); err != nil {
		t.Fatalf("removing missing continuity: %v", err)
	}
	if _, err := s.OpenContinuity("ns", "agent", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open removed = %v", err)
	}
	f, err = s.OpenContinuity("ns", "agent", other)
	if got := readAll(t, f, err); got != "o" {
		t.Fatalf("other continuity = %q", got)
	}
}

func TestToolchainKeepsNewAndPrevious(t *testing.T) {
	s, dir := newStore(t)
	tdir := filepath.Join(dir, "toolchains", "ns", "agent")
	age := func(hash string, ago time.Duration) {
		t.Helper()
		ts := time.Now().Add(-ago)
		if err := os.Chtimes(filepath.Join(tdir, hash+".tar.gz"), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	put := func(hash string) {
		t.Helper()
		if err := s.PutToolchain("ns", "agent", hash, strings.NewReader("tc-"+hash)); err != nil {
			t.Fatal(err)
		}
	}
	has := func(hash string) bool {
		t.Helper()
		_, err := os.Stat(filepath.Join(tdir, hash+".tar.gz"))
		return err == nil
	}

	put("h1")
	age("h1", 3*time.Hour)
	put("h2")
	age("h2", 2*time.Hour)
	if !has("h1") || !has("h2") {
		t.Fatal("first two toolchains should both be kept")
	}
	put("h3")
	if has("h1") || !has("h2") || !has("h3") {
		t.Fatalf("after h3: %v", listNames(t, tdir))
	}
	age("h3", time.Hour)
	// Re-putting an existing hash replaces it; the other stays as previous.
	put("h2")
	if has("h1") || !has("h2") || !has("h3") {
		t.Fatalf("after re-put h2: %v", listNames(t, tdir))
	}
	age("h2", 30*time.Minute)
	put("h4")
	if has("h3") || !has("h2") || !has("h4") {
		t.Fatalf("expected h4 and previous h2: %v", listNames(t, tdir))
	}
	f, err := s.OpenToolchain("ns", "agent", "h4")
	if got := readAll(t, f, err); got != "tc-h4" {
		t.Fatalf("toolchain = %q", got)
	}
	if _, err := s.OpenToolchain("ns", "agent", "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pruned toolchain open = %v", err)
	}
}

func TestToolchainFailedPutPrunesNothing(t *testing.T) {
	s, dir := newStore(t)
	for _, h := range []string{"a", "b"} {
		if err := s.PutToolchain("ns", "agent", h, strings.NewReader(h)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutToolchain("ns", "agent", "c", &failingReader{data: []byte("x")}); err == nil {
		t.Fatal("expected error")
	}
	if names := listNames(t, filepath.Join(dir, "toolchains", "ns", "agent")); len(names) != 2 {
		t.Fatalf("files = %v", names)
	}
}

func TestRemoveAgent(t *testing.T) {
	s, dir := newStore(t)
	if err := s.RemoveAgent("ns", "agent"); err != nil {
		t.Fatalf("removing missing agent: %v", err)
	}
	for _, a := range []string{"agent", "keep"} {
		if err := s.PutContinuity("ns", a, "c1", strings.NewReader("c")); err != nil {
			t.Fatal(err)
		}
		if err := s.PutToolchain("ns", a, "h1", strings.NewReader("t")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RemoveAgent("ns", "agent"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"continuity/ns/agent", "toolchains/ns/agent"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still present: %v", p, err)
		}
	}
	for _, p := range []string{"continuity/ns/keep", "toolchains/ns/keep"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
}

func TestStatfs(t *testing.T) {
	s, _ := newStore(t)
	free, capacity, err := s.Statfs()
	if err != nil {
		t.Fatal(err)
	}
	if capacity == 0 || free > capacity {
		t.Fatalf("free=%d capacity=%d", free, capacity)
	}
}
