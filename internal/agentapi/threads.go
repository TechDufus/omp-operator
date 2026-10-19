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

package agentapi

import (
	"errors"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/store"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	// maxNextWaitSeconds is the longest long-poll of GET thread/next.
	maxNextWaitSeconds = 50
	// maxReplyChars bounds a turn's reply.
	maxReplyChars = 20000
	// maxPushErrorChars bounds a turn's push error.
	maxPushErrorChars = 300
	// maxDiffStat bounds the diffstat numbers a turn reports.
	maxDiffStat = 1 << 30
)

// validCommit matches a full git object name.
var validCommit = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// threadReq is an authenticated request of a thread sitting.
type threadReq struct {
	*sessionReq
	thread *v1alpha1.OmpThread
}

// sessionThread loads the OmpThread of a thread sitting. For other sessions it returns
// (nil, true); on failure it writes the error response and returns ok=false.
func (s *Server) sessionThread(w http.ResponseWriter, r *http.Request, req *sessionReq) (thread *v1alpha1.OmpThread, ok bool) {
	ref := req.session.Spec.ThreadRef
	if ref == nil {
		return nil, true
	}
	var t v1alpha1.OmpThread
	if err := s.APIReader.Get(r.Context(), types.NamespacedName{Namespace: req.ns, Name: ref.Name}, &t); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "thread not found")
		} else {
			s.internalError(w, "get thread", err)
		}
		return nil, false
	}
	return &t, true
}

// threadSession is session for the routes only thread sittings have: sessions without
// spec.threadRef get 404.
func (s *Server) threadSession(h func(http.ResponseWriter, *http.Request, *threadReq)) http.HandlerFunc {
	return s.session(func(w http.ResponseWriter, r *http.Request, req *sessionReq) {
		thread, ok := s.sessionThread(w, r, req)
		if !ok {
			return
		}
		if thread == nil {
			writeError(w, http.StatusNotFound, "session is not a thread sitting")
			return
		}
		h(w, r, &threadReq{sessionReq: req, thread: thread})
	})
}

// serveThreadFile streams a stored thread file, or answers 404 when it does not exist.
func (s *Server) serveThreadFile(w http.ResponseWriter, r *http.Request, req *threadReq, file string) {
	f, err := s.Store.OpenThreadFile(req.ns, req.thread.Name, file)
	s.serveFile(w, r, f, err, "application/octet-stream")
}

// serveFile streams f with the content type, or answers 404 when err is store.ErrNotFound.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, f *os.File, err error, contentType string) {
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
		} else {
			s.internalError(w, "open file", err)
		}
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		s.internalError(w, "stat file", err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

func (s *Server) handleThreadSource(w http.ResponseWriter, r *http.Request, req *threadReq) {
	s.serveThreadFile(w, r, req, fileSource)
}

func (s *Server) handleThreadSetup(w http.ResponseWriter, r *http.Request, req *threadReq) {
	s.serveThreadFile(w, r, req, fileSetup)
}

func (s *Server) handleGetThreadResult(w http.ResponseWriter, r *http.Request, req *threadReq) {
	s.serveThreadFile(w, r, req, fileResult)
}

func (s *Server) handlePutThreadResult(w http.ResponseWriter, r *http.Request, req *threadReq) {
	body := http.MaxBytesReader(w, r.Body, store.MaxResultBytes)
	if err := s.Store.PutThreadFile(req.ns, req.thread.Name, fileResult, body, store.MaxResultBytes); err != nil {
		s.storeError(w, "store thread result", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleThreadNext long-polls for the sitting's next turn: 200 with the assignment, or 204.
func (s *Server) handleThreadNext(w http.ResponseWriter, r *http.Request, req *threadReq) {
	wait := time.Duration(0)
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid wait")
			return
		}
		wait = time.Duration(min(max(n, 1), maxNextWaitSeconds)) * time.Second
	}
	turn, err := s.Threads.Next(r.Context(), req.ns, req.thread.Name, req.name, wait)
	if err != nil {
		if r.Context().Err() != nil {
			return // the driver went away
		}
		s.internalError(w, "next turn", err)
		return
	}
	if turn == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.Threads.Signal(req.ns, req.thread.Name) // a turn started: the thread reconciler updates its status
	writeJSON(w, http.StatusOK, TurnAssignment{ID: turn.ID, Text: turn.Text})
}

func (s *Server) handleThreadControl(w http.ResponseWriter, _ *http.Request, req *threadReq) {
	stop, err := s.Threads.Control(req.ns, req.thread.Name, req.name)
	if err != nil {
		s.internalError(w, "thread control", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stop": stop})
}

var turnOutcomes = []string{OutcomeCompleted, OutcomeError}

// handleThreadTurnComplete records the result of a turn. A turn that is not running for this
// sitting (a retried report whose first response was lost) answers 409.
func (s *Server) handleThreadTurnComplete(w http.ResponseWriter, r *http.Request, req *threadReq) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid turn id")
		return
	}
	var c TurnComplete
	if !decodeBody(w, r, &c) {
		return
	}
	if !slices.Contains(turnOutcomes, c.Outcome) || len(c.Usage.Cost) > maxCostChars ||
		(c.Head != "" && !validCommit.MatchString(c.Head)) ||
		c.FilesChanged < 0 || c.Insertions < 0 || c.Deletions < 0 ||
		c.FilesChanged > maxDiffStat || c.Insertions > maxDiffStat || c.Deletions > maxDiffStat {
		writeError(w, http.StatusBadRequest, "invalid completion report")
		return
	}
	res := threads.Result{
		Outcome:      c.Outcome,
		Error:        truncate(c.Error, maxErrorChars),
		Reply:        truncate(c.Reply, maxReplyChars),
		Head:         c.Head,
		FilesChanged: c.FilesChanged,
		Insertions:   c.Insertions,
		Deletions:    c.Deletions,
		Pushed:       c.Pushed,
		PushError:    truncate(c.PushError, maxPushErrorChars),
		Usage: threads.Usage{
			InputTokens:      c.Usage.InputTokens,
			OutputTokens:     c.Usage.OutputTokens,
			CacheReadTokens:  c.Usage.CacheReadTokens,
			CacheWriteTokens: c.Usage.CacheWriteTokens,
			Cost:             c.Usage.Cost,
		},
	}
	if err := s.Threads.Complete(req.ns, req.thread.Name, req.name, id, res); err != nil {
		if errors.Is(err, threads.ErrNotRunning) {
			writeError(w, http.StatusConflict, "turn is not running for this session")
			return
		}
		s.internalError(w, "complete turn", err)
		return
	}
	s.Threads.Signal(req.ns, req.thread.Name)
	w.WriteHeader(http.StatusNoContent)
}

// askIDs parses the {id} and {ask} path values; ask is optional (zero).
func askIDs(w http.ResponseWriter, r *http.Request, withAsk bool) (turn, ask int, ok bool) {
	turn, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || turn < 1 {
		writeError(w, http.StatusBadRequest, "invalid turn id")
		return 0, 0, false
	}
	if !withAsk {
		return turn, 0, true
	}
	ask, err = strconv.Atoi(r.PathValue("ask"))
	if err != nil || ask < 1 {
		writeError(w, http.StatusBadRequest, "invalid ask number")
		return 0, 0, false
	}
	return turn, ask, true
}

// askState converts the questions of one ask to its wire state.
func askState(ask int, qs []threads.Question, done bool) AskState {
	st := AskState{Ask: ask, Done: done, Questions: make([]Question, len(qs))}
	for i, q := range qs {
		st.Questions[i] = Question{
			ID: q.ID, Ask: q.Ask, Key: q.Key, Text: q.Text, Header: q.Header, Multi: q.Multi,
			Recommended: q.Recommended, AskedAt: q.AskedAt, Selected: q.Selected, Answer: q.Answer,
			AnsweredBy: q.AnsweredBy, AnsweredAt: q.AnsweredAt, Expired: q.Expired,
		}
		for _, o := range q.Options {
			st.Questions[i].Options = append(st.Questions[i].Options, QuestionOption{Label: o.Label, Description: o.Description})
		}
	}
	return st
}

// allAnswered reports whether every question has an answer.
func allAnswered(qs []threads.Question) bool {
	return !slices.ContainsFunc(qs, func(q threads.Question) bool { return !q.Answered() })
}

// askError answers the errors the ask calls share: 409 for a turn that is not running for this
// sitting, 404 for an unknown ask, 400 for invalid input.
func (s *Server) askError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, threads.ErrNotRunning):
		writeError(w, http.StatusConflict, "turn is not running for this session")
	case errors.Is(err, threads.ErrQuestionNotFound):
		writeError(w, http.StatusNotFound, "ask not found")
	case errors.Is(err, threads.ErrInvalidQuestion), errors.Is(err, threads.ErrInvalidAnswer):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.internalError(w, what, err)
	}
}

// handleThreadAsk records one ask call of the running turn: 201 with its state, 409 when the turn
// is not running for this sitting, 400 for invalid questions.
func (s *Server) handleThreadAsk(w http.ResponseWriter, r *http.Request, req *threadReq) {
	id, _, ok := askIDs(w, r, false)
	if !ok {
		return
	}
	var body AskRequest
	if !decodeBody(w, r, &body) {
		return
	}
	in := make([]threads.QuestionInput, len(body.Questions))
	for i, q := range body.Questions {
		in[i] = threads.QuestionInput{Key: q.Key, Text: q.Text, Header: q.Header, Multi: q.Multi, Recommended: q.Recommended}
		for _, o := range q.Options {
			in[i].Options = append(in[i].Options, threads.QuestionOption{Label: o.Label, Description: o.Description})
		}
	}
	ask, qs, err := s.Threads.Ask(req.ns, req.thread.Name, req.name, id, in)
	if err != nil {
		s.askError(w, "ask", err)
		return
	}
	s.Threads.Signal(req.ns, req.thread.Name) // the thread is Waiting now
	writeJSON(w, http.StatusCreated, askState(ask, qs, false))
}

// handleThreadAskState long-polls for the answers of an ask: 200 when every question has one, 204 while waiting.
func (s *Server) handleThreadAskState(w http.ResponseWriter, r *http.Request, req *threadReq) {
	id, ask, ok := askIDs(w, r, true)
	if !ok {
		return
	}
	wait := time.Duration(0)
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid wait")
			return
		}
		wait = time.Duration(min(max(n, 1), maxNextWaitSeconds)) * time.Second
	}
	qs, done, err := s.Threads.WaitAsk(r.Context(), req.ns, req.thread.Name, req.name, id, ask, wait)
	if err != nil {
		if r.Context().Err() != nil {
			return // the driver went away
		}
		s.askError(w, "wait for ask", err)
		return
	}
	if !done {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, askState(ask, qs, true))
}

// handleThreadAskExpire answers the open questions of an ask with the driver's text: its question
// timeout ran out, or (reason dropped) the turn ended with the question open.
func (s *Server) handleThreadAskExpire(w http.ResponseWriter, r *http.Request, req *threadReq) {
	id, ask, ok := askIDs(w, r, true)
	if !ok {
		return
	}
	var body AskExpire
	if !decodeBody(w, r, &body) {
		return
	}
	var by string
	switch body.Reason {
	case "", AskExpireTimeout:
		by = threads.AnsweredByTimeout
	case AskExpireDropped:
		by = threads.AnsweredByDropped
	default:
		writeError(w, http.StatusBadRequest, "reason must be "+AskExpireTimeout+" or "+AskExpireDropped)
		return
	}
	qs, err := s.Threads.ExpireAsk(req.ns, req.thread.Name, req.name, id, ask, body.Answer, by)
	if err != nil {
		s.askError(w, "expire ask", err)
		return
	}
	s.Threads.Signal(req.ns, req.thread.Name) // the thread is no longer Waiting
	writeJSON(w, http.StatusOK, askState(ask, qs, allAnswered(qs)))
}

// handleGetCloudToolchain serves a cached repository toolchain of the agent.
func (s *Server) handleGetCloudToolchain(w http.ResponseWriter, r *http.Request, req *threadReq) {
	hash := r.PathValue("hash")
	if !validHash.MatchString(hash) {
		writeError(w, http.StatusBadRequest, "invalid hash")
		return
	}
	f, err := s.Store.OpenCloudToolchain(req.ns, req.session.Spec.AgentRef.Name, hash)
	s.serveTarball(w, r, f, err)
}

// handlePutCloudToolchain stores a repository toolchain in the agent's cache.
func (s *Server) handlePutCloudToolchain(w http.ResponseWriter, r *http.Request, req *threadReq) {
	hash := r.PathValue("hash")
	if !validHash.MatchString(hash) {
		writeError(w, http.StatusBadRequest, "invalid hash")
		return
	}
	body := http.MaxBytesReader(w, r.Body, store.MaxToolchainBytes)
	if err := s.Store.PutCloudToolchain(req.ns, req.session.Spec.AgentRef.Name, hash, body); err != nil {
		s.storeError(w, "store cloud toolchain", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
