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

package cloudapi

import (
	"strings"
	"time"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/threads"
)

// maxTurnTextBytes cuts the message of a turn in thread views; the full text stays in the turn log.
const maxTurnTextBytes = 2000

// AgentInfo is one cloud agent in the whoami response.
type AgentInfo struct {
	Namespace      string   `json:"namespace"`
	Name           string   `json:"name"`
	Ready          bool     `json:"ready"`
	DefaultModel   string   `json:"defaultModel"`
	ModelPatterns  []string `json:"modelPatterns"`
	GitHosts       []string `json:"gitHosts"`
	MaxUploadBytes int64    `json:"maxUploadBytes"`
}

// WhoAmI is the body of GET /api/v1/whoami.
type WhoAmI struct {
	User   string      `json:"user"`
	Agents []AgentInfo `json:"agents"`
}

// ThreadSummary is a thread in lists and in the creation response.
type ThreadSummary struct {
	Namespace      string                      `json:"namespace"`
	Name           string                      `json:"name"`
	Agent          string                      `json:"agent"`
	Title          string                      `json:"title,omitempty"`
	Owner          string                      `json:"owner"`
	Phase          v1alpha1.ThreadPhase        `json:"phase,omitempty"`
	Turns          int32                       `json:"turns"`
	Queued         int32                       `json:"queued"`
	Model          string                      `json:"model,omitempty"`
	Latest         *v1alpha1.ThreadTurnSummary `json:"latest,omitempty"`
	Result         *v1alpha1.ThreadResult      `json:"result,omitempty"`
	CreatedAt      time.Time                   `json:"createdAt"`
	LastActivityAt *time.Time                  `json:"lastActivityAt,omitempty"`
	URL            string                      `json:"url"`
	Warnings       []string                    `json:"warnings,omitempty"`
}

// ThreadDetail is one thread with its repository, paths and turns. Its Turns
// are the turn objects, where ThreadSummary.Turns is their number.
type ThreadDetail struct {
	Namespace      string                      `json:"namespace"`
	Name           string                      `json:"name"`
	Agent          string                      `json:"agent"`
	Title          string                      `json:"title,omitempty"`
	Owner          string                      `json:"owner"`
	Phase          v1alpha1.ThreadPhase        `json:"phase,omitempty"`
	Turns          []Turn                      `json:"turns"`
	Queued         int32                       `json:"queued"`
	Model          string                      `json:"model,omitempty"`
	Latest         *v1alpha1.ThreadTurnSummary `json:"latest,omitempty"`
	Result         *v1alpha1.ThreadResult      `json:"result,omitempty"`
	CreatedAt      time.Time                   `json:"createdAt"`
	LastActivityAt *time.Time                  `json:"lastActivityAt,omitempty"`
	URL            string                      `json:"url"`
	Repo           v1alpha1.ThreadRepo         `json:"repo"`
	Paths          v1alpha1.ThreadPaths        `json:"paths"`
	ActiveSession  string                      `json:"activeSession,omitempty"`
}

// Turn is one turn of a thread as the API shows it.
type Turn struct {
	ID           int                `json:"id"`
	State        v1alpha1.TurnState `json:"state"`
	Text         string             `json:"text"`
	By           string             `json:"by"`
	QueuedAt     time.Time          `json:"queuedAt"`
	StartedAt    *time.Time         `json:"startedAt,omitempty"`
	CompletedAt  *time.Time         `json:"completedAt,omitempty"`
	Reply        string             `json:"reply,omitempty"`
	Title        string             `json:"title,omitempty"`
	Error        string             `json:"error,omitempty"`
	Usage        *threads.Usage     `json:"usage,omitempty"`
	Head         string             `json:"head,omitempty"`
	FilesChanged int                `json:"filesChanged,omitempty"`
	Insertions   int                `json:"insertions,omitempty"`
	Deletions    int                `json:"deletions,omitempty"`
	Pushed       bool               `json:"pushed,omitempty"`
	PushError    string             `json:"pushError,omitempty"`
}

// Event is one entry of the events response.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

func turnView(t *threads.Turn) Turn {
	return Turn{
		ID: t.ID, State: t.State, Text: cut(t.Text, maxTurnTextBytes), By: t.By, QueuedAt: t.QueuedAt,
		StartedAt: t.StartedAt, CompletedAt: t.CompletedAt, Reply: t.Reply, Title: t.Title, Error: t.Error,
		Usage: t.Usage, Head: t.Head, FilesChanged: t.FilesChanged, Insertions: t.Insertions, Deletions: t.Deletions,
		Pushed: t.Pushed, PushError: t.PushError,
	}
}

// cut shortens s to at most n bytes without splitting a UTF-8 sequence.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

func (s *Server) threadURL(ns, name string) string {
	if s.Options == nil || s.Options.DashboardURL == "" {
		return ""
	}
	return strings.TrimRight(s.Options.DashboardURL, "/") + "/threads/" + ns + "/" + name
}

func timePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func (s *Server) summary(t *v1alpha1.OmpThread) ThreadSummary {
	out := ThreadSummary{
		Namespace: t.Namespace, Name: t.Name, Agent: t.Spec.AgentRef.Name, Title: t.Spec.Title, Owner: t.Spec.Owner,
		Phase: t.Status.Phase, Turns: t.Status.Turns, Queued: t.Status.Queued, Model: t.Spec.Model,
		Latest: t.Status.Latest, Result: t.Status.Result, CreatedAt: t.CreationTimestamp.UTC(),
		URL: s.threadURL(t.Namespace, t.Name),
	}
	if a := t.Status.LastActivityAt; a != nil {
		out.LastActivityAt = timePtr(&a.Time)
	}
	return out
}

func (s *Server) detail(t *v1alpha1.OmpThread, turns []threads.Turn) ThreadDetail {
	sum := s.summary(t)
	d := ThreadDetail{
		Namespace: sum.Namespace, Name: sum.Name, Agent: sum.Agent, Title: sum.Title, Owner: sum.Owner, Phase: sum.Phase,
		Queued: sum.Queued, Model: sum.Model, Latest: sum.Latest, Result: sum.Result, CreatedAt: sum.CreatedAt,
		LastActivityAt: sum.LastActivityAt, URL: sum.URL, Repo: t.Spec.Repo, Paths: t.Spec.Paths,
		ActiveSession: t.Status.ActiveSession, Turns: make([]Turn, 0, len(turns)),
	}
	for i := range turns {
		d.Turns = append(d.Turns, turnView(&turns[i]))
	}
	return d
}
