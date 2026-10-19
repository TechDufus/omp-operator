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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/adapters"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	indexSessions   = 50
	agentSessions   = 100
	triggerSessions = 50
	sessionEvents   = 200

	defaultEventSessionsPerDay       = 12
	defaultNonCriticalSessionsPerDay = 8
	defaultTools                     = "read, grep, glob, bash, todo"
)

// pageData is the data of the layout template.
type pageData struct {
	Title   string
	User    string
	Session bool // signed in through OIDC: show sign out
	Refresh bool
	Body    any
}

func (s *Server) renderPage(w http.ResponseWriter, status int, name string, d pageData) {
	var buf bytes.Buffer
	if err := templates[name].ExecuteTemplate(&buf, "layout", d); err != nil {
		log.Error(err, "render page", "page", name)
		textError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) page(w http.ResponseWriter, r *http.Request, name, title string, refresh bool, body any) {
	p := who(r)
	s.renderPage(w, http.StatusOK, name, pageData{Title: title, User: p.Name, Session: p.Session, Refresh: refresh, Body: body})
}

// fail answers a data error: 404 for a missing or invisible object, else 500.
func fail(w http.ResponseWriter, err error) {
	if apierrors.IsNotFound(err) {
		notFound(w)
		return
	}
	log.Error(err, "dashboard read")
	textError(w, http.StatusInternalServerError, "internal error")
}

// ---- reading ----

func collect[T any, L client.ObjectList](ctx context.Context, s *Server, p *principal, newList func() L, items func(L) []T, nsOf func(*T) string) ([]T, error) {
	var out []T
	for _, opts := range s.scopeOptions(p) {
		l := newList()
		if err := s.reader().List(ctx, l, opts...); err != nil {
			return nil, err
		}
		its := items(l)
		for i := range its {
			if s.canSee(p, nsOf(&its[i])) {
				out = append(out, its[i])
			}
		}
	}
	return out, nil
}

func (s *Server) visibleAgents(ctx context.Context, p *principal) ([]v1alpha1.OmpAgent, error) {
	out, err := collect(ctx, s, p, func() *v1alpha1.OmpAgentList { return &v1alpha1.OmpAgentList{} },
		func(l *v1alpha1.OmpAgentList) []v1alpha1.OmpAgent { return l.Items },
		func(a *v1alpha1.OmpAgent) string { return a.Namespace })
	slices.SortFunc(out, func(a, b v1alpha1.OmpAgent) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	return out, err
}

func (s *Server) visibleTriggers(ctx context.Context, p *principal) ([]v1alpha1.OmpTrigger, error) {
	out, err := collect(ctx, s, p, func() *v1alpha1.OmpTriggerList { return &v1alpha1.OmpTriggerList{} },
		func(l *v1alpha1.OmpTriggerList) []v1alpha1.OmpTrigger { return l.Items },
		func(t *v1alpha1.OmpTrigger) string { return t.Namespace })
	slices.SortFunc(out, func(a, b v1alpha1.OmpTrigger) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	return out, err
}

func (s *Server) visibleSessions(ctx context.Context, p *principal) ([]v1alpha1.OmpSession, error) {
	out, err := collect(ctx, s, p, func() *v1alpha1.OmpSessionList { return &v1alpha1.OmpSessionList{} },
		func(l *v1alpha1.OmpSessionList) []v1alpha1.OmpSession { return l.Items },
		func(x *v1alpha1.OmpSession) string { return x.Namespace })
	sortNewest(out)
	return out, err
}

func sortNewest(ss []v1alpha1.OmpSession) {
	slices.SortFunc(ss, func(a, b v1alpha1.OmpSession) int {
		if c := b.CreationTimestamp.Compare(a.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(b.Name, a.Name)
	})
}

// nsSessions lists the sessions of one namespace, newest first.
func (s *Server) nsSessions(ctx context.Context, ns string) ([]v1alpha1.OmpSession, error) {
	var l v1alpha1.OmpSessionList
	if err := s.reader().List(ctx, &l, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	sortNewest(l.Items)
	return l.Items, nil
}

func (s *Server) nsTriggers(ctx context.Context, ns string) ([]v1alpha1.OmpTrigger, error) {
	var l v1alpha1.OmpTriggerList
	if err := s.reader().List(ctx, &l, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	slices.SortFunc(l.Items, func(a, b v1alpha1.OmpTrigger) int { return strings.Compare(a.Name, b.Name) })
	return l.Items, nil
}

// ---- view helpers ----

func (s *Server) ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := s.now().Sub(t)
	switch {
	case d < 0:
		return "in " + shortDuration(-d)
	case d < time.Minute:
		return "just now"
	}
	return shortDuration(d) + " ago"
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

func (s *Server) when(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return stamp(t.Time) + " (" + s.ago(t.Time) + ")"
}

func readyOf(conds []metav1.Condition) (status, reason, message string) {
	c := meta.FindStatusCondition(conds, v1alpha1.ConditionReady)
	if c == nil {
		return "Unknown", "", ""
	}
	return string(c.Status), c.Reason, c.Message
}

func objectLink(kind, ns, name string) string {
	return "/" + kind + "/" + url.PathEscape(ns) + "/" + url.PathEscape(name)
}

// safeURL drops credentials, query and fragment from a URL before it is shown.
func safeURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		return u.String()
	}
	if i := strings.LastIndex(raw, "@"); i >= 0 && !strings.Contains(raw, "://") {
		raw = raw[i+1:] // scp-style user@host:path
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if strings.Contains(raw, "@") {
		return "(url hidden)"
	}
	return raw
}

type sessionRowView struct {
	Link, Name, Namespace, Agent, Trigger, Kind string
	Phase, Verdict, Title, Age, Created         string
}

func sessionVerdict(ss *v1alpha1.OmpSession) string {
	if ss.Status.Phase == v1alpha1.SessionPhaseFailed {
		return "failed"
	}
	return string(ss.Status.Verdict)
}

func sessionKind(ss *v1alpha1.OmpSession) string {
	if k := ss.Labels[v1alpha1.LabelKind]; k != "" {
		return k
	}
	if ss.Spec.Event != nil {
		return string(v1alpha1.SessionKindWebhook)
	}
	return string(v1alpha1.SessionKindManual)
}

func (s *Server) sessionRows(sessions []v1alpha1.OmpSession, limit int, withNS bool) []sessionRowView {
	out := make([]sessionRowView, 0, min(len(sessions), limit))
	for i := range sessions {
		if len(out) == limit {
			break
		}
		ss := &sessions[i]
		row := sessionRowView{
			Link:      objectLink("sessions", ss.Namespace, ss.Name),
			Name:      ss.Name,
			Namespace: ss.Namespace,
			Agent:     ss.Spec.AgentRef.Name,
			Kind:      sessionKind(ss),
			Phase:     string(ss.Status.Phase),
			Verdict:   sessionVerdict(ss),
			Title:     ss.Status.Title,
			Age:       s.ago(ss.CreationTimestamp.Time),
			Created:   stamp(ss.CreationTimestamp.Time),
		}
		if ss.Spec.TriggerRef != nil {
			row.Trigger = ss.Spec.TriggerRef.Name
		}
		if !withNS {
			row.Namespace = ""
		}
		out = append(out, row)
	}
	return out
}

type budgetView struct {
	EventUsed, EventLimit       int
	NonCriticalUsed, NonCritLim int
}

// budgets counts the agent's webhook-created sessions in the last 24 hours against its limits.
func (s *Server) budgets(agent *v1alpha1.OmpAgent, sessions []v1alpha1.OmpSession) budgetView {
	b := budgetView{EventLimit: int(agent.Spec.Limits.EventSessionsPerDay), NonCritLim: int(agent.Spec.Limits.NonCriticalSessionsPerDay)}
	if b.EventLimit == 0 {
		b.EventLimit = defaultEventSessionsPerDay
	}
	if b.NonCritLim == 0 {
		b.NonCritLim = defaultNonCriticalSessionsPerDay
	}
	cutoff := s.now().Add(-24 * time.Hour)
	for i := range sessions {
		ss := &sessions[i]
		if ss.Namespace != agent.Namespace || ss.Spec.AgentRef.Name != agent.Name || ss.Spec.Event == nil ||
			ss.CreationTimestamp.Time.Before(cutoff) {
			continue
		}
		b.EventUsed++
		if ss.Spec.Event.Severity != "critical" {
			b.NonCriticalUsed++
		}
	}
	return b
}

type verdictCounts struct {
	OK, Fixed, NeedsOwner, Inconclusive, Failed, Active int
}

func (s *Server) countVerdicts(agent *v1alpha1.OmpAgent, sessions []v1alpha1.OmpSession) verdictCounts {
	var c verdictCounts
	cutoff := s.now().Add(-24 * time.Hour)
	for i := range sessions {
		ss := &sessions[i]
		if ss.Namespace != agent.Namespace || ss.Spec.AgentRef.Name != agent.Name || ss.CreationTimestamp.Time.Before(cutoff) {
			continue
		}
		switch {
		case ss.Status.Phase == v1alpha1.SessionPhaseFailed:
			c.Failed++
		case ss.Status.Phase == v1alpha1.SessionPhasePending || ss.Status.Phase == v1alpha1.SessionPhaseRunning || ss.Status.Phase == "":
			c.Active++
		case ss.Status.Verdict == v1alpha1.VerdictOK:
			c.OK++
		case ss.Status.Verdict == v1alpha1.VerdictFixed:
			c.Fixed++
		case ss.Status.Verdict == v1alpha1.VerdictNeedsOwner:
			c.NeedsOwner++
		default:
			c.Inconclusive++
		}
	}
	return c
}

type agentRowView struct {
	Link, Namespace, Name, Model string
	Ready, ReadyReason           string
	Counts                       verdictCounts
	Budget                       budgetView
}

func (s *Server) agentRow(a *v1alpha1.OmpAgent, sessions []v1alpha1.OmpSession) agentRowView {
	ready, reason, _ := readyOf(a.Status.Conditions)
	return agentRowView{
		Link: objectLink("agents", a.Namespace, a.Name), Namespace: a.Namespace, Name: a.Name,
		Model: a.Spec.Model.Default, Ready: ready, ReadyReason: reason,
		Counts: s.countVerdicts(a, sessions), Budget: s.budgets(a, sessions),
	}
}

type triggerRowView struct {
	Link, Namespace, Name, Agent, Type, Ready, ReadyReason string
	Enabled                                                bool
}

func triggerType(t *v1alpha1.OmpTrigger) string {
	switch {
	case t.Status.Type != "":
		return string(t.Status.Type)
	case t.Spec.Webhook != nil:
		return string(v1alpha1.TriggerWebhook)
	case t.Spec.Schedule != nil:
		return string(v1alpha1.TriggerSchedule)
	}
	return ""
}

func triggerRow(t *v1alpha1.OmpTrigger) triggerRowView {
	ready, reason, _ := readyOf(t.Status.Conditions)
	return triggerRowView{
		Link: objectLink("triggers", t.Namespace, t.Name), Namespace: t.Namespace, Name: t.Name,
		Agent: t.Spec.AgentRef.Name, Type: triggerType(t), Ready: ready, ReadyReason: reason,
		Enabled: t.Spec.Enabled == nil || *t.Spec.Enabled,
	}
}

// ---- index ----

type indexView struct {
	Agents   []agentRowView
	Triggers []triggerRowView
	Threads  []threadRowView
	Sessions []sessionRowView
	NoAccess bool
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	ctx := r.Context()
	agents, err := s.visibleAgents(ctx, p)
	if err != nil {
		fail(w, err)
		return
	}
	triggers, err := s.visibleTriggers(ctx, p)
	if err != nil {
		fail(w, err)
		return
	}
	sessions, err := s.visibleSessions(ctx, p)
	if err != nil {
		fail(w, err)
		return
	}
	threadList, err := s.visibleThreads(ctx, p)
	if err != nil {
		fail(w, err)
		return
	}
	v := indexView{Sessions: s.sessionRows(sessions, indexSessions, true), Threads: s.threadRows(threadList, indexThreads)}
	for i := range agents {
		v.Agents = append(v.Agents, s.agentRow(&agents[i], sessions))
	}
	for i := range triggers {
		v.Triggers = append(v.Triggers, triggerRow(&triggers[i]))
	}
	all, nss := s.Access.scope(p)
	v.NoAccess = !all && len(nss) == 0
	s.page(w, r, "index", "Overview", false, v)
}

// ---- agent ----

type credentialView struct{ Name, Description, Where string }

type agentView struct {
	Namespace, Name                  string
	Ready, ReadyReason, ReadyMessage string
	Image, ServiceAccount            string
	Model, Small, Thinking, Gateway  string
	Tools                            string
	Skills                           []string
	Workspace                        []string
	Credentials                      []credentialView
	MCP                              []string
	Actions                          []string
	Notify                           []string
	Toolchain                        string
	Limits                           []string
	Triggers                         []triggerRowView
	Counts                           verdictCounts
	Budget                           budgetView
	Sessions                         []sessionRowView
}

func skillSourceText(sk *v1alpha1.SkillSource) string {
	switch {
	case sk.Git != nil:
		t := "git " + safeURL(sk.Git.URL)
		if sk.Git.Ref != "" {
			t += " @" + sk.Git.Ref
		}
		if sk.Git.Path != "" {
			t += " (" + sk.Git.Path + ")"
		}
		return t
	case sk.ConfigMap != nil:
		return "configMap " + sk.ConfigMap.Name
	case sk.FluxSource != nil:
		f := sk.FluxSource
		t := fmt.Sprintf("%s %s/%s", f.Kind, f.Namespace, f.Name)
		if f.Path != "" {
			t += " (" + f.Path + ")"
		}
		return t
	}
	return "-"
}

func workspaceLines(ws *v1alpha1.WorkspaceSpec) []string {
	if ws == nil {
		return nil
	}
	var out []string
	switch {
	case ws.Git != nil:
		t := "git " + safeURL(ws.Git.URL)
		if ws.Git.Ref != "" {
			t += " @" + ws.Git.Ref
		}
		out = append(out, t)
	case ws.FluxSource != nil:
		f := ws.FluxSource
		out = append(out, fmt.Sprintf("%s %s/%s", f.Kind, f.Namespace, f.Name))
	}
	for _, l := range ws.Links {
		out = append(out, "link "+l.Path+" -> "+l.Target)
	}
	return out
}

func (s *Server) buildAgentView(ctx context.Context, a *v1alpha1.OmpAgent) (agentView, error) {
	sessions, err := s.nsSessions(ctx, a.Namespace)
	if err != nil {
		return agentView{}, err
	}
	triggers, err := s.nsTriggers(ctx, a.Namespace)
	if err != nil {
		return agentView{}, err
	}
	ready, reason, msg := readyOf(a.Status.Conditions)
	sp := &a.Spec
	v := agentView{
		Namespace: a.Namespace, Name: a.Name, Ready: ready, ReadyReason: reason, ReadyMessage: msg,
		Image: sp.Image, ServiceAccount: sp.ServiceAccountName,
		Model: sp.Model.Default, Small: sp.Model.Small, Thinking: string(sp.Model.Thinking),
		Tools:     defaultTools,
		Workspace: workspaceLines(sp.Workspace),
		Counts:    s.countVerdicts(a, sessions), Budget: s.budgets(a, sessions),
	}
	if v.Image == "" && s.Options != nil {
		v.Image = s.Options.DefaultRuntimeImage
	}
	if v.Thinking == "" {
		v.Thinking = string(v1alpha1.ThinkingMedium)
	}
	if g := sp.Model.Gateway; g != nil {
		v.Gateway = safeURL(g.URL)
		if g.TokenSecretRef == nil {
			v.Gateway += " (managed token)"
		}
	}
	if len(sp.Tools.Allow) > 0 {
		v.Tools = strings.Join(sp.Tools.Allow, ", ")
	}
	for i := range sp.Skills {
		v.Skills = append(v.Skills, skillSourceText(&sp.Skills[i]))
	}
	for _, c := range sp.Credentials {
		where := "-"
		switch {
		case c.Env != "":
			where = "environment variable $" + c.Env
		case c.File != "":
			where = "file " + c.File
		}
		v.Credentials = append(v.Credentials, credentialView{Name: c.Name, Description: c.Description, Where: where})
	}
	for _, m := range sp.MCPServers {
		v.MCP = append(v.MCP, m.Name)
	}
	for _, ar := range sp.Actions {
		v.Actions = append(v.Actions, ar.Name)
	}
	for _, n := range sp.Notify {
		v.Notify = append(v.Notify, string(n.Type))
	}
	if sp.Toolchain != nil {
		v.Toolchain = "enabled"
		if len(sp.Toolchain.Disable) > 0 {
			v.Toolchain += ", disabled: " + strings.Join(sp.Toolchain.Disable, ", ")
		}
		if h := a.Status.Toolchain.Hash; h != "" {
			v.Toolchain += ", hash " + h[:min(len(h), 12)]
		}
		if a.Status.Toolchain.Stale {
			v.Toolchain += ", stale"
		}
	}
	lim := sp.Limits
	v.Limits = []string{
		"session timeout " + durationText(lim.SessionTimeout, 30*time.Minute),
		fmt.Sprintf("max attempts %d", orDefault(lim.MaxAttempts, 4)),
		"retry delay " + durationText(lim.RetryDelay, 15*time.Minute),
		"retention " + durationText(sp.SessionRetention, 720*time.Hour),
	}
	for i := range triggers {
		if triggers[i].Spec.AgentRef.Name == a.Name {
			v.Triggers = append(v.Triggers, triggerRow(&triggers[i]))
		}
	}
	var mine []v1alpha1.OmpSession
	for i := range sessions {
		if sessions[i].Spec.AgentRef.Name == a.Name {
			mine = append(mine, sessions[i])
		}
	}
	v.Sessions = s.sessionRows(mine, agentSessions, false)
	return v, nil
}

func orDefault(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

func durationText(d metav1.Duration, def time.Duration) string {
	if d.Duration == 0 {
		return def.String()
	}
	return d.Duration.String()
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if !s.canSee(who(r), ns) {
		notFound(w)
		return
	}
	var a v1alpha1.OmpAgent
	if err := s.fresh().Get(r.Context(), types.NamespacedName{Namespace: ns, Name: name}, &a); err != nil {
		fail(w, err)
		return
	}
	v, err := s.buildAgentView(r.Context(), &a)
	if err != nil {
		fail(w, err)
		return
	}
	s.page(w, r, "agent", "Agent "+ns+"/"+name, false, v)
}

// ---- trigger ----

type groupView struct {
	ID, Name, Severity                                string
	EpisodeStart, LastNotified, Resolved, LastSession string
	LastSessionLink                                   string
	Triaged                                           int
}

type triggerView struct {
	Namespace, Name, Agent, AgentLink      string
	Type, Ready, ReadyReason, ReadyMessage string
	Enabled                                bool
	HookURL, Adapter, Auth                 string
	AlertFilter, Filter                    string
	Mapping                                [][2]string
	Resume, Priority                       string
	Grouping                               [][2]string
	Schedule                               [][2]string
	Groups                                 []groupView
	Sessions                               []sessionRowView
}

func (s *Server) buildTriggerView(ctx context.Context, t *v1alpha1.OmpTrigger) (triggerView, error) {
	sessions, err := s.nsSessions(ctx, t.Namespace)
	if err != nil {
		return triggerView{}, err
	}
	ready, reason, msg := readyOf(t.Status.Conditions)
	v := triggerView{
		Namespace: t.Namespace, Name: t.Name, Agent: t.Spec.AgentRef.Name,
		AgentLink: objectLink("agents", t.Namespace, t.Spec.AgentRef.Name),
		Type:      triggerType(t), Ready: ready, ReadyReason: reason, ReadyMessage: msg,
		Enabled: t.Spec.Enabled == nil || *t.Spec.Enabled,
		Resume:  string(t.Spec.Session.Resume),
	}
	if v.Resume == "" {
		v.Resume = string(v1alpha1.ResumeNone)
	}
	if t.Spec.Priority != nil {
		v.Priority = fmt.Sprint(*t.Spec.Priority)
	} else if t.Spec.Webhook != nil {
		v.Priority = "2 (1 when critical)"
	} else {
		v.Priority = "3"
	}
	if w := t.Spec.Webhook; w != nil {
		path := t.Status.HookPath
		if path == "" {
			path = "/hooks/" + t.Namespace + "/" + t.Name
		}
		v.HookURL = path
		if s.Options != nil && s.Options.HooksPublicURL != "" {
			v.HookURL = strings.TrimRight(s.Options.HooksPublicURL, "/") + path
		}
		v.Adapter = string(w.Adapter)
		switch {
		case w.Auth.Bearer != nil:
			v.Auth = "bearer"
		case w.Auth.Basic != nil:
			v.Auth = "basic"
		case w.Auth.HMAC != nil:
			v.Auth = "hmac"
		}
		if w.Adapter == v1alpha1.AdapterAlertmanager || w.Adapter == v1alpha1.AdapterGrafana {
			v.AlertFilter = firstNonEmpty(w.AlertFilter, adapters.DefaultAlertFilter)
		}
		v.Filter = firstNonEmpty(w.Filter, adapters.DefaultFilter)
		if m := w.Mapping; m != nil && w.Adapter == v1alpha1.AdapterGeneric {
			v.Mapping = [][2]string{
				{"events", firstNonEmpty(m.Events, adapters.DefaultMappingEvents)},
				{"key", m.Key},
				{"title", firstNonEmpty(m.Title, adapters.DefaultMappingTitle)},
				{"severity", firstNonEmpty(m.Severity, adapters.DefaultMappingSeverity)},
				{"status", firstNonEmpty(m.Status, adapters.DefaultMappingStatus)},
				{"fingerprint", firstNonEmpty(m.Fingerprint, adapters.DefaultMappingFingerprint)},
			}
		}
		g := w.Grouping
		v.Grouping = [][2]string{
			{"episode gap", durationText(g.EpisodeGap, 30*time.Minute)},
			{"retriage after", durationText(g.RetriageAfter, time.Hour)},
			{"per group per day", fmt.Sprint(orDefault(g.PerGroupPerDay, 2))},
		}
	}
	if sc := t.Spec.Schedule; sc != nil {
		v.Schedule = [][2]string{
			{"cron", sc.Cron},
			{"time zone", firstNonEmpty(sc.TimeZone, "UTC")},
			{"next fire", s.when(t.Status.NextFireAt)},
			{"last fire", s.when(t.Status.LastFireAt)},
		}
	}
	for _, g := range t.Status.Groups {
		gv := groupView{
			ID: g.ID, Name: g.Name, Severity: g.Severity,
			EpisodeStart: stamp(g.EpisodeStart.Time), LastNotified: stamp(g.LastNotified.Time),
			Resolved: s.when(g.ResolvedAt), Triaged: len(g.TriagedFingerprints), LastSession: g.LastSession,
		}
		if g.LastSession != "" {
			gv.LastSessionLink = objectLink("sessions", t.Namespace, g.LastSession)
		}
		v.Groups = append(v.Groups, gv)
	}
	var mine []v1alpha1.OmpSession
	for i := range sessions {
		if tr := sessions[i].Spec.TriggerRef; tr != nil && tr.Name == t.Name {
			mine = append(mine, sessions[i])
		}
	}
	v.Sessions = s.sessionRows(mine, triggerSessions, false)
	return v, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if !s.canSee(who(r), ns) {
		notFound(w)
		return
	}
	var t v1alpha1.OmpTrigger
	if err := s.fresh().Get(r.Context(), types.NamespacedName{Namespace: ns, Name: name}, &t); err != nil {
		fail(w, err)
		return
	}
	v, err := s.buildTriggerView(r.Context(), &t)
	if err != nil {
		fail(w, err)
		return
	}
	s.page(w, r, "trigger", "Trigger "+ns+"/"+name, false, v)
}

// ---- session ----

type eventView struct{ At, Kind, Text string }

type actionView struct {
	Action, Target, At, Result string
	OK                         bool
}

type sessionView struct {
	Namespace, Name, Agent, AgentLink, Trigger, TriggerLink, Kind string
	Thread, ThreadLink                                            string
	Phase, Verdict, Outcome, Title, Summary, Error                string
	Created, Started, Completed, Pod, Revision                    string
	Attempts                                                      int32
	Event                                                         string
	ProposedCommands                                              []string
	Actions                                                       []actionView
	Events                                                        []eventView
	Report                                                        string
	Usage                                                         string
	Continuity                                                    string
	TranscriptLink                                                string
	Refresh                                                       bool
}

func usageText(u *v1alpha1.SessionUsage) string {
	if u == nil {
		return ""
	}
	t := fmt.Sprintf("input %d, output %d, cache read %d, cache write %d", u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	if u.Cost != "" {
		t += ", cost " + u.Cost
	}
	return t
}

func (s *Server) buildSessionView(ss *v1alpha1.OmpSession) sessionView {
	st := &ss.Status
	v := sessionView{
		Namespace: ss.Namespace, Name: ss.Name,
		Agent: ss.Spec.AgentRef.Name, AgentLink: objectLink("agents", ss.Namespace, ss.Spec.AgentRef.Name),
		Kind: sessionKind(ss), Phase: string(st.Phase), Verdict: sessionVerdict(ss), Outcome: string(st.Outcome),
		Title: st.Title, Summary: st.Summary, Error: st.Error,
		Created: s.when(&ss.CreationTimestamp), Started: s.when(st.StartTime), Completed: s.when(st.CompletionTime),
		Pod: st.PodName, Revision: st.WorkspaceRevision, Attempts: st.Attempts,
		ProposedCommands: st.ProposedCommands, Usage: usageText(st.Usage),
		Refresh: st.Phase == v1alpha1.SessionPhasePending || st.Phase == v1alpha1.SessionPhaseRunning || st.Phase == "",
	}
	if tr := ss.Spec.TriggerRef; tr != nil {
		v.Trigger = tr.Name
		v.TriggerLink = objectLink("triggers", ss.Namespace, tr.Name)
	}
	if tr := ss.Spec.ThreadRef; tr != nil {
		v.Thread = tr.Name
		v.ThreadLink = objectLink("threads", ss.Namespace, tr.Name)
	}
	if e := ss.Spec.Event; e != nil {
		v.Event = strings.TrimSpace(fmt.Sprintf("%s (%s) %s", e.Name, e.Severity, e.GroupID))
	}
	for _, a := range st.Actions {
		v.Actions = append(v.Actions, actionView{Action: a.Action, Target: a.Target, At: stamp(a.At.Time), Result: a.Result, OK: a.OK})
	}
	if c := st.Continuity; c != nil {
		v.Continuity = fmt.Sprintf("restored: %t, stored: %t", c.Restored, c.Stored)
	}
	if st.RetryAt != nil && st.Phase == v1alpha1.SessionPhasePending {
		v.Outcome = strings.TrimSpace(v.Outcome + " (retry " + s.when(st.RetryAt) + ")")
	}
	if s.Store != nil && ss.UID != "" {
		uid := string(ss.UID)
		if evs, err := s.Store.ReadEvents(uid, sessionEvents); err == nil {
			for _, e := range evs {
				v.Events = append(v.Events, eventView{At: stamp(e.At), Kind: e.Kind, Text: e.Text})
			}
		}
		if rep, err := s.Store.ReadReport(uid); err == nil {
			v.Report = rep
		}
		if f, err := s.Store.OpenTranscript(uid); err == nil {
			_ = f.Close()
			v.TranscriptLink = objectLink("sessions", ss.Namespace, ss.Name) + "/transcript"
		}
	}
	return v
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) (*v1alpha1.OmpSession, bool) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if !s.canSee(who(r), ns) {
		notFound(w)
		return nil, false
	}
	var ss v1alpha1.OmpSession
	if err := s.fresh().Get(r.Context(), types.NamespacedName{Namespace: ns, Name: name}, &ss); err != nil {
		fail(w, err)
		return nil, false
	}
	return &ss, true
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.getSession(w, r)
	if !ok {
		return
	}
	v := s.buildSessionView(ss)
	s.page(w, r, "session", "Session "+ss.Namespace+"/"+ss.Name, v.Refresh, v)
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.getSession(w, r)
	if !ok {
		return
	}
	if s.Store == nil || ss.UID == "" {
		notFound(w)
		return
	}
	f, err := s.Store.OpenTranscript(string(ss.UID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidName) {
			notFound(w)
			return
		}
		fail(w, err)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", transcriptCSP)
	h.Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
