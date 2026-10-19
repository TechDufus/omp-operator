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
	// Capacity says how many sessions the agent runs at once and how many run now.
	Capacity Capacity `json:"capacity"`
	// Threads counts the agent's threads by phase.
	Threads ThreadCounts `json:"threads"`
}

// Capacity is the session capacity of an agent.
type Capacity struct {
	MaxConcurrentSessions int `json:"maxConcurrentSessions"`
	Running               int `json:"running"`
}

// ThreadCounts counts the threads of an agent in a namespace by phase.
type ThreadCounts struct {
	Working int `json:"working"`
	Waiting int `json:"waiting"`
	Idle    int `json:"idle"`
}

// WhoAmI is the body of GET /api/v1/whoami.
type WhoAmI struct {
	User   string      `json:"user"`
	Agents []AgentInfo `json:"agents"`
}

// Question is the oldest unanswered question of a thread, in summaries.
type Question struct {
	Turn        int                      `json:"turn"`
	ID          int                      `json:"id"`
	Ask         int                      `json:"ask"`
	Text        string                   `json:"text"`
	Header      string                   `json:"header"`
	Options     []threads.QuestionOption `json:"options"`
	Multi       bool                     `json:"multi"`
	Recommended *int                     `json:"recommended"`
	AskedAt     time.Time                `json:"askedAt"`
	// Remaining counts the unanswered questions of the same ask, this one included.
	Remaining int `json:"remaining"`
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
	Question       *Question                   `json:"question"`
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
	Question       *Question                   `json:"question"`
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
	Questions    []threads.Question `json:"questions,omitempty"`
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
		Pushed: t.Pushed, PushError: t.PushError, Questions: t.Questions,
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

func questionView(p *threads.Pending) *Question {
	if p == nil {
		return nil
	}
	q := p.Question
	opts := q.Options
	if opts == nil {
		opts = []threads.QuestionOption{}
	}
	return &Question{
		Turn: p.Turn, ID: q.ID, Ask: q.Ask, Text: q.Text, Header: q.Header, Options: opts, Multi: q.Multi,
		Recommended: q.Recommended, AskedAt: q.AskedAt.UTC(), Remaining: p.Remaining,
	}
}

// summary is the thread's status plus its pending question, which comes from the turn log (turns)
// and not from the status, so it is current. A pending question makes the thread Waiting.
func (s *Server) summary(t *v1alpha1.OmpThread, turns []threads.Turn) ThreadSummary {
	out := ThreadSummary{
		Namespace: t.Namespace, Name: t.Name, Agent: t.Spec.AgentRef.Name, Title: t.Spec.Title, Owner: t.Spec.Owner,
		Phase: t.Status.Phase, Turns: t.Status.Turns, Queued: t.Status.Queued, Model: t.Spec.Model,
		Latest: t.Status.Latest, Result: t.Status.Result, CreatedAt: t.CreationTimestamp.UTC(),
		URL: s.threadURL(t.Namespace, t.Name), Question: questionView(threads.PendingQuestion(turns)),
	}
	if out.Question != nil {
		out.Phase = v1alpha1.ThreadPhaseWaiting
	}
	if a := t.Status.LastActivityAt; a != nil {
		out.LastActivityAt = timePtr(&a.Time)
	}
	return out
}

func (s *Server) detail(t *v1alpha1.OmpThread, turns []threads.Turn) ThreadDetail {
	sum := s.summary(t, turns)
	d := ThreadDetail{
		Namespace: sum.Namespace, Name: sum.Name, Agent: sum.Agent, Title: sum.Title, Owner: sum.Owner, Phase: sum.Phase,
		Queued: sum.Queued, Model: sum.Model, Latest: sum.Latest, Question: sum.Question, Result: sum.Result,
		CreatedAt: sum.CreatedAt, LastActivityAt: sum.LastActivityAt, URL: sum.URL, Repo: t.Spec.Repo, Paths: t.Spec.Paths,
		ActiveSession: t.Status.ActiveSession, Turns: make([]Turn, 0, len(turns)),
	}
	for i := range turns {
		d.Turns = append(d.Turns, turnView(&turns[i]))
	}
	return d
}
