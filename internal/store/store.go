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

// Package store keeps the operator's on-disk data under --data-dir: session
// files, continuity tarballs and toolchain tarballs.
//
// Layout:
//
//	sessions/<uid>/{events.jsonl,transcript.html,report.md}
//	continuity/<ns>/<agent>/<sha16>.tar.gz
//	toolchains/<ns>/<agent>/<hash>.tar.gz
//
// Every path component supplied by callers is validated, and every file is
// written to a temporary file and renamed into place, so readers never see a
// partial file.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// MaxEventLines is the most lines kept in a session's events.jsonl; older lines are dropped.
	MaxEventLines = 5000
	// MaxTranscriptBytes is the largest accepted transcript.
	MaxTranscriptBytes int64 = 64 << 20
	// MaxContinuityBytes is the largest accepted continuity tarball.
	MaxContinuityBytes int64 = 64 << 20
	// MaxToolchainBytes is the largest accepted toolchain tarball.
	MaxToolchainBytes int64 = 2 << 30

	tarballExt = ".tar.gz"
	tempPrefix = ".tmp-"
)

var (
	// ErrNotFound is returned when the requested file does not exist. It wraps fs.ErrNotExist.
	ErrNotFound = fmt.Errorf("not found: %w", fs.ErrNotExist)
	// ErrTooLarge is returned when an upload exceeds its size limit; nothing is stored.
	ErrTooLarge = errors.New("content too large")
	// ErrInvalidName is returned for an empty path component or one that could escape its directory.
	ErrInvalidName = errors.New("invalid path component")
)

// Event is one line of a session's events.jsonl.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

// Store is rooted at the data directory. It is safe for concurrent use.
type Store struct {
	root  string
	locks sync.Map // string -> *entry
}

// entry guards one session's event log or one agent's toolchains.
type entry struct {
	mu    sync.Mutex
	lines int  // events.jsonl line count, valid when known
	known bool // lines has been counted
}

// New returns a Store rooted at dir, creating it if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Store{root: dir}, nil
}

// ContinuityID returns the first 16 hex characters of sha256(key). It names
// continuity tarballs and is the value of the omp.techdufus.com/continuity label.
func ContinuityID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

// Statfs reports the free (available to unprivileged users) and total bytes of
// the filesystem holding the data directory.
func (s *Store) Statfs() (free, capacity uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.root, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize) //nolint:gosec // block size is positive
	return st.Bavail * bsize, st.Blocks * bsize, nil
}

// validName rejects components that are empty, dot entries, or contain a path
// separator, NUL, or backslash.
func validName(parts ...string) error {
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || len(p) > 255 || strings.ContainsAny(p, "/\\\x00") {
			return fmt.Errorf("%w: %q", ErrInvalidName, p)
		}
	}
	return nil
}

func (s *Store) lock(key string) *entry {
	e, _ := s.locks.LoadOrStore(key, &entry{})
	return e.(*entry)
}

// writeAtomic streams r into path via a temp file in the same directory. A
// positive max rejects larger input with ErrTooLarge.
func writeAtomic(path string, r io.Reader, maxBytes int64) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	src := r
	if maxBytes > 0 {
		src = io.LimitReader(r, maxBytes+1)
	}
	n, err := io.Copy(tmp, src)
	if err != nil {
		return err
	}
	if maxBytes > 0 && n > maxBytes {
		return ErrTooLarge
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func open(path string) (*os.File, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// ---- sessions ----

func (s *Store) sessionDir(uid string) string { return filepath.Join(s.root, "sessions", uid) }

// AppendEvents appends events to the session's events.jsonl, dropping the
// oldest lines so at most MaxEventLines remain.
func (s *Store) AppendEvents(uid string, events []Event) error {
	if err := validName(uid); err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	var add bytes.Buffer
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		add.Write(line)
		add.WriteByte('\n')
	}

	path := filepath.Join(s.sessionDir(uid), "events.jsonl")
	e := s.lock("session/" + uid)
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.known {
		old, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		e.lines, e.known = bytes.Count(old, []byte{'\n'}), true
	}
	if e.lines+len(events) <= MaxEventLines {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(add.Bytes()); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		e.lines += len(events)
		return nil
	}

	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	lines := splitLines(append(old, add.Bytes()...))
	lines = lines[max(0, len(lines)-MaxEventLines):]
	out := append(bytes.Join(lines, []byte{'\n'}), '\n')
	if err := writeAtomic(path, bytes.NewReader(out), 0); err != nil {
		e.known = false
		return err
	}
	e.lines = len(lines)
	return nil
}

// ReadEvents returns the last n events, oldest first. Malformed lines are
// skipped. A session without events yields nil.
func (s *Store) ReadEvents(uid string, n int) ([]Event, error) {
	if err := validName(uid); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.sessionDir(uid), "events.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lines := splitLines(data)
	var out []Event
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		var ev Event
		if json.Unmarshal(lines[i], &ev) == nil {
			out = append(out, ev)
		}
	}
	slices.Reverse(out)
	return out, nil
}

// splitLines splits on '\n' and drops empty lines.
func splitLines(b []byte) [][]byte {
	var out [][]byte
	for line := range bytes.SplitSeq(b, []byte{'\n'}) {
		if len(line) > 0 {
			out = append(out, line)
		}
	}
	return out
}

// WriteTranscript stores the session transcript from r. Input over
// MaxTranscriptBytes fails with ErrTooLarge and leaves any previous transcript intact.
func (s *Store) WriteTranscript(uid string, r io.Reader) error {
	if err := validName(uid); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.sessionDir(uid), "transcript.html"), r, MaxTranscriptBytes)
}

// OpenTranscript opens the session transcript, or returns ErrNotFound.
func (s *Store) OpenTranscript(uid string) (*os.File, error) {
	if err := validName(uid); err != nil {
		return nil, err
	}
	return open(filepath.Join(s.sessionDir(uid), "transcript.html"))
}

// WriteReport stores the session report.
func (s *Store) WriteReport(uid, report string) error {
	if err := validName(uid); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.sessionDir(uid), "report.md"), strings.NewReader(report), 0)
}

// ReadReport returns the session report, or ErrNotFound.
func (s *Store) ReadReport(uid string) (string, error) {
	if err := validName(uid); err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(s.sessionDir(uid), "report.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotFound
	}
	return string(b), err
}

// RemoveSession deletes everything stored for the session. Missing data is not an error.
func (s *Store) RemoveSession(uid string) error {
	if err := validName(uid); err != nil {
		return err
	}
	key := "session/" + uid
	e := s.lock(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := os.RemoveAll(s.sessionDir(uid)); err != nil {
		return err
	}
	e.lines, e.known = 0, false
	s.locks.Delete(key)
	return nil
}

// ---- continuity ----

func (s *Store) continuityDir(ns, agent string) string {
	return filepath.Join(s.root, "continuity", ns, agent)
}

func (s *Store) continuityPath(ns, agent, id string) string {
	return filepath.Join(s.continuityDir(ns, agent), id+tarballExt)
}

// PutContinuity streams a continuity tarball for id (see ContinuityID) and
// atomically replaces any previous one. Input over MaxContinuityBytes fails
// with ErrTooLarge.
func (s *Store) PutContinuity(ns, agent, id string, r io.Reader) error {
	if err := validName(ns, agent, id); err != nil {
		return err
	}
	return writeAtomic(s.continuityPath(ns, agent, id), r, MaxContinuityBytes)
}

// OpenContinuity opens the tarball for id, or returns ErrNotFound.
func (s *Store) OpenContinuity(ns, agent, id string) (*os.File, error) {
	if err := validName(ns, agent, id); err != nil {
		return nil, err
	}
	return open(s.continuityPath(ns, agent, id))
}

// RemoveContinuity deletes the tarball for id. A missing tarball is not an error.
func (s *Store) RemoveContinuity(ns, agent, id string) error {
	if err := validName(ns, agent, id); err != nil {
		return err
	}
	if err := os.Remove(s.continuityPath(ns, agent, id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ---- toolchains ----

func (s *Store) toolchainDir(ns, agent string) string {
	return filepath.Join(s.root, "toolchains", ns, agent)
}

func (s *Store) toolchainPath(ns, agent, hash string) string {
	return filepath.Join(s.toolchainDir(ns, agent), hash+tarballExt)
}

// PutToolchain streams the toolchain tarball for hash, then prunes the agent's
// toolchains to the new tarball and the previous (most recently written) one.
// Input over MaxToolchainBytes fails with ErrTooLarge and prunes nothing.
func (s *Store) PutToolchain(ns, agent, hash string, r io.Reader) error {
	if err := validName(ns, agent, hash); err != nil {
		return err
	}
	e := s.lock("toolchain/" + ns + "/" + agent)
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := writeAtomic(s.toolchainPath(ns, agent, hash), r, MaxToolchainBytes); err != nil {
		return err
	}
	return s.pruneToolchains(ns, agent, hash)
}

func (s *Store) pruneToolchains(ns, agent, keep string) error {
	dir := s.toolchainDir(ns, agent)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type old struct {
		name string
		mod  time.Time
	}
	var others []old
	for _, de := range ents {
		name := de.Name()
		if !strings.HasSuffix(name, tarballExt) || name == keep+tarballExt {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		others = append(others, old{name, info.ModTime()})
	}
	// Newest first; the first entry is the previous toolchain and is kept.
	slices.SortFunc(others, func(a, b old) int {
		if c := b.mod.Compare(a.mod); c != 0 {
			return c
		}
		return strings.Compare(b.name, a.name)
	})
	for i, o := range others {
		if i == 0 {
			continue
		}
		if err := os.Remove(filepath.Join(dir, o.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// OpenToolchain opens the tarball for hash, or returns ErrNotFound.
func (s *Store) OpenToolchain(ns, agent, hash string) (*os.File, error) {
	if err := validName(ns, agent, hash); err != nil {
		return nil, err
	}
	return open(s.toolchainPath(ns, agent, hash))
}

// RemoveAgent deletes all continuity and toolchain data of the agent. Missing data is not an error.
func (s *Store) RemoveAgent(ns, agent string) error {
	if err := validName(ns, agent); err != nil {
		return err
	}
	key := "toolchain/" + ns + "/" + agent
	e := s.lock(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := os.RemoveAll(s.continuityDir(ns, agent)); err != nil {
		return err
	}
	if err := os.RemoveAll(s.toolchainDir(ns, agent)); err != nil {
		return err
	}
	s.locks.Delete(key)
	return nil
}
