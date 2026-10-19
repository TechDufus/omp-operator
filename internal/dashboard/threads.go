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

package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/cloudspec"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	indexThreads = 20
	// threadEvents is how many of the newest sitting's events the thread page shows.
	threadEvents = 100
	// turnTextLimit cuts the message of a turn shown in the turns table.
	turnTextLimit = 2000
)

// threadLog is the turn log, or one over the store when none was injected.
func (s *Server) threadLog() *threads.Log {
	if s.Threads != nil {
		return s.Threads
	}
	return &threads.Log{Store: s.Store}
}

func (s *Server) visibleThreads(ctx context.Context, p *principal) ([]v1alpha1.OmpThread, error) {
	out, err := collect(ctx, s, p, func() *v1alpha1.OmpThreadList { return &v1alpha1.OmpThreadList{} },
		func(l *v1alpha1.OmpThreadList) []v1alpha1.OmpThread { return l.Items },
		func(t *v1alpha1.OmpThread) string { return t.Namespace })
	slices.SortFunc(out, func(a, b v1alpha1.OmpThread) int {
		if c := b.CreationTimestamp.Compare(a.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(b.Name, a.Name)
	})
	return out, err
}

type threadRowView struct {
	Link, Name, Namespace, Agent, Phase, Latest, Title, Age, Created string
	Turns                                                            int32
}

func (s *Server) threadRows(ts []v1alpha1.OmpThread, limit int) []threadRowView {
	out := make([]threadRowView, 0, min(len(ts), limit))
	for i := range ts {
		if len(out) == limit {
			break
		}
		t := &ts[i]
		row := threadRowView{
			Link: objectLink("threads", t.Namespace, t.Name), Name: t.Name, Namespace: t.Namespace,
			Agent: t.Spec.AgentRef.Name, Phase: string(t.Status.Phase), Title: t.Spec.Title, Turns: t.Status.Turns,
			Age: s.ago(t.CreationTimestamp.Time), Created: stamp(t.CreationTimestamp.Time),
		}
		if l := t.Status.Latest; l != nil {
			row.Latest = string(l.State)
		}
		out = append(out, row)
	}
	return out
}

type turnView struct {
	ID                                    int
	State, Text, By, Queued, Title, Reply string
	Error, Usage, Head, Push              string
}

type threadResultView struct {
	Head, Branch, Changes, Files, Pushed string
}

type threadView struct {
	Namespace, Name, Agent, AgentLink, Owner string
	Phase, Model, Thinking, Title            string
	Repo, Base, LocalBranch, Branch          string
	Paths                                    string
	Created, LastActivity                    string
	ActiveSession, ActiveLink                string
	Result                                   *threadResultView
	PullHint                                 string
	Turns                                    []turnView
	EventsSession, EventsLink                string
	Events                                   []eventView
	Refresh                                  bool
	Conditions                               []string
	SecretFiles                              []string
	SecretEnv                                []string
	Queued                                   int32
	TurnCount                                int
	Questions                                []questionRowView
	Pending                                  *pendingView
	AnswerHint                               string
}

type optionView struct {
	Label, Description string
	Recommended        bool
}

// questionRowView is one question the agent asked, with its answer.
type questionRowView struct {
	Turn, ID, Ask             int
	Header, Text              string
	Options                   []optionView
	Multi                     bool
	Asked, Status, Selected   string
	Free, By, Answered        string
	Pending, Expired, Waiting bool
}

// pendingView is the oldest unanswered question of the running turn.
type pendingView struct {
	questionRowView
	Remaining int
}

func questionRows(ts []threads.Turn) []questionRowView {
	var out []questionRowView
	pending := threads.PendingQuestion(ts)
	for _, t := range ts {
		for _, q := range t.Questions {
			out = append(out, questionRow(t.ID, q, pending != nil && pending.Turn == t.ID && pending.Question.ID == q.ID))
		}
	}
	return out
}

func questionRow(turn int, q threads.Question, pending bool) questionRowView {
	row := questionRowView{
		Turn: turn, ID: q.ID, Ask: q.Ask, Header: q.Header, Text: q.Text, Multi: q.Multi, Asked: stamp(q.AskedAt),
		Selected: strings.Join(q.Selected, ", "), Free: q.Answer, By: q.AnsweredBy, Expired: q.Expired,
		Pending: pending, Status: "waiting for an answer", Waiting: !q.Answered(),
	}
	for i, o := range q.Options {
		row.Options = append(row.Options, optionView{Label: o.Label, Description: o.Description, Recommended: q.Recommended != nil && *q.Recommended == i})
	}
	switch {
	case q.Expired:
		row.Status = "expired (" + q.AnsweredBy + ")"
		row.By = ""
	case q.Answered():
		row.Status = "answered"
	}
	if q.AnsweredAt != nil {
		row.Answered = stamp(*q.AnsweredAt)
	}
	return row
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func turnUsage(u *threads.Usage) string {
	if u == nil {
		return ""
	}
	t := fmt.Sprintf("input %d, output %d, cache read %d, cache write %d", u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	if u.Cost != "" {
		t += ", cost " + u.Cost
	}
	return t
}

func (s *Server) turnViews(ts []threads.Turn) []turnView {
	out := make([]turnView, 0, len(ts))
	for _, t := range ts {
		v := turnView{
			ID: t.ID, State: string(t.State), Text: clip(t.Text, turnTextLimit), By: t.By, Queued: stamp(t.QueuedAt),
			Title: t.Title, Reply: t.Reply, Error: t.Error, Usage: turnUsage(t.Usage), Head: t.Head,
		}
		switch {
		case t.PushError != "":
			v.Push = "push failed: " + t.PushError
		case t.Pushed:
			v.Push = "pushed"
		}
		out = append(out, v)
	}
	return out
}

// EventsSitting is the sitting whose events a thread page and the cloud API
// show: the active one (the newest that is neither Succeeded nor Failed), else
// the newest. It returns nil for no sittings.
func EventsSitting(sittings []v1alpha1.OmpSession) *v1alpha1.OmpSession {
	var newest *v1alpha1.OmpSession
	for i := range sittings {
		ss := &sittings[i]
		if newest == nil || sittingNewer(ss, newest) {
			newest = ss
		}
	}
	var active *v1alpha1.OmpSession
	for i := range sittings {
		ss := &sittings[i]
		if ss.Status.Phase == v1alpha1.SessionPhaseSucceeded || ss.Status.Phase == v1alpha1.SessionPhaseFailed {
			continue
		}
		if active == nil || sittingNewer(ss, active) {
			active = ss
		}
	}
	if active != nil {
		return active
	}
	return newest
}

func sittingNewer(a, b *v1alpha1.OmpSession) bool {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c > 0
	}
	return a.Name > b.Name
}

func (s *Server) buildThreadView(ctx context.Context, t *v1alpha1.OmpThread) (threadView, error) {
	sp, st := &t.Spec, &t.Status
	v := threadView{
		Namespace: t.Namespace, Name: t.Name, Agent: sp.AgentRef.Name,
		AgentLink: objectLink("agents", t.Namespace, sp.AgentRef.Name), Owner: sp.Owner,
		Phase: string(st.Phase), Model: sp.Model, Thinking: sp.Thinking, Title: sp.Title,
		Repo: sp.Repo.Name, Base: sp.Repo.BaseCommit, LocalBranch: sp.Repo.LocalBranch, Branch: cloudspec.Branch(t.Name),
		Created: s.when(&t.CreationTimestamp), LastActivity: s.when(st.LastActivityAt),
		ActiveSession: st.ActiveSession, Queued: st.Queued, SecretFiles: sp.SecretFiles, SecretEnv: sp.SecretEnv,
		PullHint: "/cloud pull " + t.Name,
		Refresh:  st.Phase == v1alpha1.ThreadPhaseWorking || st.Phase == v1alpha1.ThreadPhaseWaiting,
	}
	if sp.Repo.RemoteURL != "" {
		v.Repo = sp.Repo.Name + " (" + safeURL(sp.Repo.RemoteURL) + ")"
	}
	if sp.Paths.Home != "" || sp.Paths.Workspace != "" {
		v.Paths = "home " + sp.Paths.Home + ", workspace " + sp.Paths.Workspace
	}
	if st.ActiveSession != "" {
		v.ActiveLink = objectLink("sessions", t.Namespace, st.ActiveSession)
	}
	for _, c := range st.Conditions {
		v.Conditions = append(v.Conditions, fmt.Sprintf("%s=%s %s", c.Type, c.Status, c.Reason))
	}
	if r := st.Result; r != nil {
		rv := &threadResultView{
			Head: r.Head, Branch: r.Branch,
			Changes: fmt.Sprintf("+%d \u2212%d", r.Insertions, r.Deletions), Files: fmt.Sprint(r.FilesChanged),
		}
		switch {
		case r.PushError != "":
			rv.Pushed = "push failed: " + r.PushError
		case r.Pushed:
			rv.Pushed = "pushed"
		default:
			rv.Pushed = "not pushed"
		}
		v.Result = rv
	}
	if s.Store != nil {
		turns, err := s.threadLog().Turns(t.Namespace, t.Name)
		if err != nil {
			return threadView{}, err
		}
		v.Turns = s.turnViews(turns)
		v.TurnCount = len(turns)
		v.Questions = questionRows(turns)
		if p := threads.PendingQuestion(turns); p != nil {
			v.Phase, v.Refresh = string(v1alpha1.ThreadPhaseWaiting), true
			v.Pending = &pendingView{questionRowView: questionRow(p.Turn, p.Question, true), Remaining: p.Remaining}
			v.AnswerHint = "/cloud answer " + t.Name
		}
		if !v.Refresh {
			for _, tu := range turns {
				if tu.State == v1alpha1.TurnStateQueued || tu.State == v1alpha1.TurnStateRunning {
					v.Refresh = true
				}
			}
		}
	}
	var sittings v1alpha1.OmpSessionList
	if err := s.reader().List(ctx, &sittings, client.InNamespace(t.Namespace), client.MatchingLabels{v1alpha1.LabelThread: t.Name}); err != nil {
		return threadView{}, err
	}
	if ss := EventsSitting(sittings.Items); ss != nil {
		v.EventsSession = ss.Name
		v.EventsLink = objectLink("sessions", ss.Namespace, ss.Name)
		if s.Store != nil && ss.UID != "" {
			if evs, err := s.Store.ReadEvents(string(ss.UID), threadEvents); err == nil {
				for _, e := range evs {
					v.Events = append(v.Events, eventView{At: stamp(e.At), Kind: e.Kind, Text: e.Text})
				}
			}
		}
	}
	return v, nil
}

func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if !s.canSee(who(r), ns) {
		notFound(w)
		return
	}
	var t v1alpha1.OmpThread
	if err := s.fresh().Get(r.Context(), types.NamespacedName{Namespace: ns, Name: name}, &t); err != nil {
		fail(w, err)
		return
	}
	v, err := s.buildThreadView(r.Context(), &t)
	if err != nil {
		fail(w, err)
		return
	}
	s.page(w, r, "thread", "Thread "+ns+"/"+name, v.Refresh, v)
}
