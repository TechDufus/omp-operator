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

// Package cloudapi serves the HTTP API that local omp instances use to hand
// work to cloud agents (OmpThreads): create a thread from a repository bundle,
// follow it, send follow-up messages and fetch its result branch.
//
// Every request authenticates with an API bearer token from the access file;
// the token's principal "token:<name>" is authorized per namespace by the
// access rules (viewer reads, operator also writes).
package cloudapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/cloudspec"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/dashboard"
	"github.com/TechDufus/omp-operator/internal/gateway"
	"github.com/TechDufus/omp-operator/internal/store"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	// listThreads is how many threads the list route returns, newest first.
	listThreads = 50
	// maxEvents is the most events one events response carries.
	maxEvents = 500
	// maxMessageBodyBytes bounds the JSON body of a message: its text plus the envelope.
	maxMessageBodyBytes = threads.MaxTextBytes + 4<<10
	// maxAnswerBodyBytes bounds the JSON body of an answer: the free text, the selected labels and the envelope.
	maxAnswerBodyBytes = threads.MaxQuestionTextBytes + threads.MaxOptions*(threads.MaxOptionLabelBytes+16) + 4<<10

	messageThreadFull = "thread has 200 turns; start a new one"
	messageDeleting   = "thread is being deleted"
	messageNotWaiting = "question is not waiting for an answer"
)

var log = logf.Log.WithName("cloudapi")

// Server is the cloud API. Build one and mount Handler at /api/.
type Server struct {
	// Client reads lists and writes; APIReader reads single objects uncached
	// (Client is used when it is nil).
	Client    client.Client
	APIReader client.Reader
	Store     *store.Store
	Threads   *threads.Log
	Access    *dashboard.Access
	Options   *config.Options
	// MaxUploadBytes bounds the repository bundle of a new thread.
	MaxUploadBytes int64
}

type principalKey struct{}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func fail(status int, msg string) *apiError { return &apiError{status: status, msg: msg} }

var (
	errNotFound  = fail(http.StatusNotFound, "not found")
	errForbidden = fail(http.StatusForbidden, "forbidden: operator role required")
)

func (s *Server) reader() client.Reader {
	if s.APIReader != nil {
		return s.APIReader
	}
	return s.Client
}

// Handler returns the API handler; it expects requests under /api/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/whoami", s.handleWhoAmI)
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/threads", s.handleCreate)
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/threads", s.handleList)
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/threads/{name}", s.handleGet)
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/threads/{name}/events", s.handleEvents)
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/threads/{name}/messages", s.handleMessage)
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/threads/{name}/stop", s.handleStop)
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/threads/{name}/turns/{turn}/questions/{qid}/answer", s.handleAnswer)
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/threads/{name}/result", s.handleResult)
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/threads/{name}/conversation", s.handleConversation)
	mux.HandleFunc("DELETE /api/v1/namespaces/{ns}/threads/{name}", s.handleDelete)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { writeError(w, errNotFound) })

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		if !s.Access.TokensConfigured() {
			writeError(w, fail(http.StatusServiceUnavailable, "cloud API not configured: no apiTokens in the access file"))
			return
		}
		principal, ok := "", false
		if tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found {
			principal, ok = s.Access.TokenPrincipal(strings.TrimSpace(tok))
		}
		if !ok {
			h.Set("WWW-Authenticate", `Bearer realm="omp-operator"`)
			writeError(w, fail(http.StatusUnauthorized, "unauthorized"))
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
	})
}

func principalOf(r *http.Request) string {
	p, _ := r.Context().Value(principalKey{}).(string)
	return p
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		log.Error(err, "cloud API request failed")
		ae = fail(http.StatusInternalServerError, "internal error")
	}
	writeJSON(w, ae.status, map[string]string{"error": ae.msg})
}

// nsServed reports whether the operator serves ns at all.
func (s *Server) nsServed(ns string) bool {
	return s.Options == nil || s.Options.NamespaceAllowed(ns)
}

// authorize checks the request's principal against ns: not granted or not
// served is "not found", a viewer calling a write route is forbidden.
func (s *Server) authorize(r *http.Request, ns, role string) error {
	p := principalOf(r)
	if !s.nsServed(ns) || !s.Access.Allows(p, nil, ns, dashboard.RoleViewer) {
		return errNotFound
	}
	if role == dashboard.RoleOperator && !s.Access.Allows(p, nil, ns, dashboard.RoleOperator) {
		return errForbidden
	}
	return nil
}

// thread authorizes the request for its namespace and loads the thread it names.
func (s *Server) thread(r *http.Request, role string) (*v1alpha1.OmpThread, error) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := s.authorize(r, ns, role); err != nil {
		return nil, err
	}
	var t v1alpha1.OmpThread
	if err := s.reader().Get(r.Context(), types.NamespacedName{Namespace: ns, Name: name}, &t); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errNotFound
		}
		return nil, err
	}
	return &t, nil
}

// ---- routes ----

func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	var agents v1alpha1.OmpAgentList
	if err := s.Client.List(r.Context(), &agents); err != nil {
		writeError(w, err)
		return
	}
	out := WhoAmI{User: p, Agents: []AgentInfo{}}
	var infos []*v1alpha1.OmpAgent
	for i := range agents.Items {
		a := &agents.Items[i]
		if a.Spec.Cloud == nil || !s.nsServed(a.Namespace) || !s.Access.Allows(p, nil, a.Namespace, dashboard.RoleOperator) {
			continue
		}
		infos = append(infos, a)
	}
	if len(infos) > 0 {
		load, err := s.agentLoads(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		for _, a := range infos {
			info := s.agentInfo(a)
			l := load[agentKey{a.Namespace, a.Name}]
			info.Capacity.Running, info.Threads = l.running, l.threads
			out.Agents = append(out.Agents, info)
		}
	}
	slices.SortFunc(out.Agents, func(a, b AgentInfo) int { return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name) })
	writeJSON(w, http.StatusOK, out)
}

type agentKey struct{ ns, name string }

// agentLoad is what runs on an agent now.
type agentLoad struct {
	running int // sessions in phase Running
	threads ThreadCounts
}

// agentLoads counts the Running sessions and the threads by phase of every agent. A thread
// that no reconcile has given a phase yet has a queued first turn: it counts as working.
func (s *Server) agentLoads(ctx context.Context) (map[agentKey]agentLoad, error) {
	var sessions v1alpha1.OmpSessionList
	if err := s.Client.List(ctx, &sessions); err != nil {
		return nil, err
	}
	var ths v1alpha1.OmpThreadList
	if err := s.Client.List(ctx, &ths); err != nil {
		return nil, err
	}
	load := map[agentKey]agentLoad{}
	for i := range sessions.Items {
		sess := &sessions.Items[i]
		if sess.Status.Phase != v1alpha1.SessionPhaseRunning {
			continue
		}
		k := agentKey{sess.Namespace, sess.Spec.AgentRef.Name}
		l := load[k]
		l.running++
		load[k] = l
	}
	for i := range ths.Items {
		t := &ths.Items[i]
		k := agentKey{t.Namespace, t.Spec.AgentRef.Name}
		l := load[k]
		switch t.Status.Phase {
		case v1alpha1.ThreadPhaseIdle:
			l.threads.Idle++
		case v1alpha1.ThreadPhaseWaiting:
			l.threads.Waiting++
		default:
			l.threads.Working++
		}
		load[k] = l
	}
	return load, nil
}

func (s *Server) agentInfo(a *v1alpha1.OmpAgent) AgentInfo {
	info := AgentInfo{
		Namespace: a.Namespace, Name: a.Name, Ready: meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionReady),
		DefaultModel: a.Spec.Model.Default, ModelPatterns: modelPatterns(a), GitHosts: []string{}, MaxUploadBytes: s.maxUpload(),
		Capacity: Capacity{MaxConcurrentSessions: max(1, int(a.Spec.Limits.MaxConcurrentSessions))},
	}
	for _, g := range a.Spec.Cloud.Git {
		info.GitHosts = append(info.GitHosts, g.Host)
	}
	return info
}

// modelPatterns are the models a thread of the agent may use: what the gateway
// permits, or, without a gateway, the agent's own models.
func modelPatterns(a *v1alpha1.OmpAgent) []string {
	if a.Spec.Model.Gateway != nil {
		return gateway.AllowedModels(a)
	}
	out := []string{a.Spec.Model.Default}
	if sm := a.Spec.Model.Small; sm != "" && sm != a.Spec.Model.Default {
		out = append(out, sm)
	}
	return out
}

// modelAllowed reports whether model matches a pattern: an exact name, or a
// prefix ending in "*".
func modelAllowed(patterns []string, model string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			return strings.HasPrefix(model, prefix)
		}
		return p == model
	})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	if err := s.authorize(r, ns, dashboard.RoleViewer); err != nil {
		writeError(w, err)
		return
	}
	var list v1alpha1.OmpThreadList
	if err := s.Client.List(r.Context(), &list, client.InNamespace(ns)); err != nil {
		writeError(w, err)
		return
	}
	slices.SortFunc(list.Items, func(a, b v1alpha1.OmpThread) int {
		if c := b.CreationTimestamp.Compare(a.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(b.Name, a.Name)
	})
	out := make([]ThreadSummary, 0, min(len(list.Items), listThreads))
	for i := range list.Items {
		if len(out) == listThreads {
			break
		}
		t := &list.Items[i]
		var turns []threads.Turn
		if t.Status.Phase != v1alpha1.ThreadPhaseIdle {
			var err error
			if turns, err = s.Threads.Turns(t.Namespace, t.Name); err != nil {
				writeError(w, err)
				return
			}
		}
		out = append(out, s.summary(t, turns))
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": out})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.thread(r, dashboard.RoleViewer)
	if err != nil {
		writeError(w, err)
		return
	}
	turns, err := s.Threads.Turns(t.Namespace, t.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.detail(t, turns))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	t, err := s.thread(r, dashboard.RoleViewer)
	if err != nil {
		writeError(w, err)
		return
	}
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		if since, err = time.Parse(time.RFC3339Nano, raw); err != nil {
			writeError(w, fail(http.StatusBadRequest, "since must be an RFC 3339 timestamp"))
			return
		}
	}
	var sittings v1alpha1.OmpSessionList
	if err := s.Client.List(r.Context(), &sittings, client.InNamespace(t.Namespace), client.MatchingLabels{v1alpha1.LabelThread: t.Name}); err != nil {
		writeError(w, err)
		return
	}
	out := struct {
		Session string  `json:"session"`
		Events  []Event `json:"events"`
	}{Events: []Event{}}
	if ss := dashboard.EventsSitting(sittings.Items); ss != nil {
		out.Session = ss.Name
		evs, err := s.Store.ReadEvents(string(ss.UID), store.MaxEventLines)
		if err != nil && !errors.Is(err, store.ErrInvalidName) {
			writeError(w, err)
			return
		}
		for _, e := range evs {
			if e.At.After(since) {
				out.Events = append(out.Events, Event{At: e.At, Kind: e.Kind, Text: e.Text})
			}
		}
		if n := len(out.Events); n > maxEvents {
			out.Events = out.Events[n-maxEvents:]
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	t, err := s.thread(r, dashboard.RoleOperator)
	if err != nil {
		writeError(w, err)
		return
	}
	if !t.DeletionTimestamp.IsZero() {
		writeError(w, fail(http.StatusConflict, messageDeleting))
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(w, r, maxMessageBodyBytes, &body); err != nil {
		writeError(w, err)
		return
	}
	turn, err := s.Threads.Enqueue(t.Namespace, t.Name, body.Text, principalOf(r))
	switch {
	case errors.Is(err, threads.ErrThreadFull):
		writeError(w, fail(http.StatusConflict, messageThreadFull))
		return
	case errors.Is(err, threads.ErrInvalidText):
		writeError(w, fail(http.StatusBadRequest, err.Error()))
		return
	case err != nil:
		writeError(w, err)
		return
	}
	s.Threads.Signal(t.Namespace, t.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"turn": turnView(&turn)})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	t, err := s.thread(r, dashboard.RoleOperator)
	if err != nil {
		writeError(w, err)
		return
	}
	stopped, cancelled, err := s.Threads.Stop(t.Namespace, t.Name, principalOf(r))
	if err != nil {
		writeError(w, err)
		return
	}
	s.Threads.Signal(t.Namespace, t.Name)
	writeJSON(w, http.StatusOK, map[string]any{"stoppedRunning": stopped, "cancelled": cancelled})
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	t, err := s.thread(r, dashboard.RoleOperator)
	if err != nil {
		writeError(w, err)
		return
	}
	turn, err1 := strconv.Atoi(r.PathValue("turn"))
	qid, err2 := strconv.Atoi(r.PathValue("qid"))
	if err1 != nil || err2 != nil || turn < 1 || qid < 1 {
		writeError(w, errNotFound)
		return
	}
	var body struct {
		Selected []string `json:"selected"`
		Answer   string   `json:"answer"`
	}
	if err := decodeJSON(w, r, maxAnswerBodyBytes, &body); err != nil {
		writeError(w, err)
		return
	}
	q, err := s.Threads.Answer(t.Namespace, t.Name, turn, qid, body.Selected, body.Answer, principalOf(r))
	switch {
	case errors.Is(err, threads.ErrQuestionNotFound):
		writeError(w, errNotFound)
		return
	case errors.Is(err, threads.ErrNotWaiting):
		writeError(w, fail(http.StatusConflict, messageNotWaiting))
		return
	case errors.Is(err, threads.ErrInvalidAnswer):
		writeError(w, fail(http.StatusBadRequest, err.Error()))
		return
	case err != nil:
		writeError(w, err)
		return
	}
	s.Threads.Signal(t.Namespace, t.Name)
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	t, err := s.thread(r, dashboard.RoleViewer)
	if err != nil {
		writeError(w, err)
		return
	}
	f, err := s.Store.OpenThreadFile(t.Namespace, t.Name, "result.bundle")
	if err != nil {
		writeError(w, storeError(err, "no result yet"))
		return
	}
	defer func() { _ = f.Close() }()
	h := w.Header()
	h.Set("X-Omp-Branch", cloudspec.Branch(t.Name))
	if res := t.Status.Result; res != nil {
		h.Set("X-Omp-Head", res.Head)
	}
	h.Set("X-Omp-Base", t.Spec.Repo.BaseCommit)
	serveFile(w, r, f, "application/octet-stream")
}

func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	t, err := s.thread(r, dashboard.RoleViewer)
	if err != nil {
		writeError(w, err)
		return
	}
	f, err := s.Store.OpenContinuity(t.Namespace, t.Spec.AgentRef.Name, store.ContinuityID(cloudspec.ContinuityKey(t.Namespace, t.Name)))
	if err != nil {
		writeError(w, storeError(err, "no conversation yet"))
		return
	}
	defer func() { _ = f.Close() }()
	serveFile(w, r, f, "application/gzip")
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	t, err := s.thread(r, dashboard.RoleOperator)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.Client.Delete(r.Context(), t, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleting"})
}

// ---- helpers ----

func serveFile(w http.ResponseWriter, r *http.Request, f io.ReadSeeker, contentType string) {
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", time.Time{}, f)
}

func storeError(err error, notFoundMsg string) error {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidName) {
		return fail(http.StatusNotFound, notFoundMsg)
	}
	return err
}

// decodeJSON reads one JSON object of at most limit bytes from the request body.
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(v); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return fail(http.StatusRequestEntityTooLarge, "request body is too large")
		}
		return fail(http.StatusBadRequest, "invalid JSON body")
	}
	return nil
}
