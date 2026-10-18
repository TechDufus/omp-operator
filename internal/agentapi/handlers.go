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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/store"
)

const (
	maxEventsPerPost  = 1000
	maxEventTextChars = 500
	maxEventKindChars = 32
	maxErrorChars     = 1000
	maxRevisionChars  = 256
	maxCostChars      = 32
)

// validHash matches toolchain hashes (hex digests) and keeps them safe as file names.
var validHash = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// getAgent reads the OmpAgent a session refers to, or an agent addressed directly.
func (s *Server) getAgent(r *http.Request, ns, name string) (*v1alpha1.OmpAgent, error) {
	var agent v1alpha1.OmpAgent
	if err := s.APIReader.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: name}, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

// sessionAgent loads the session's agent; it writes the error response and returns nil on failure.
func (s *Server) sessionAgent(w http.ResponseWriter, r *http.Request, req *sessionReq) *v1alpha1.OmpAgent {
	agent, err := s.getAgent(r, req.ns, req.session.Spec.AgentRef.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "agent not found")
		} else {
			s.internalError(w, "get agent", err)
		}
		return nil
	}
	return agent
}

// planError answers a failed plan: an unavailable Flux source is 503 (the driver retries), anything else 500.
func (s *Server) planError(w http.ResponseWriter, err error) {
	if noArtifact, ok := errors.AsType[errNoArtifact](err); ok {
		writeError(w, http.StatusServiceUnavailable, noArtifact.Error())
		return
	}
	if apierrors.IsNotFound(err) {
		writeError(w, http.StatusServiceUnavailable, "source not found")
		return
	}
	s.internalError(w, "build plan", err)
}

func (s *Server) handleSessionPlan(w http.ResponseWriter, r *http.Request, req *sessionReq) {
	agent := s.sessionAgent(w, r, req)
	if agent == nil {
		return
	}
	plan, err := s.sessionPlan(r.Context(), agent, req.session)
	if err != nil {
		s.planError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleSessionConfig(w http.ResponseWriter, r *http.Request, req *sessionReq) {
	agent := s.sessionAgent(w, r, req)
	if agent == nil {
		return
	}
	cfg, err := s.sessionConfig(r.Context(), agent, req.session)
	if err != nil {
		s.internalError(w, "build session config", err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handlePrepared(w http.ResponseWriter, r *http.Request, req *sessionReq) {
	var p Prepared
	if !decodeBody(w, r, &p) {
		return
	}
	if len(p.WorkspaceRevision) > maxRevisionChars || (p.ToolchainHash != "" && !validHash.MatchString(p.ToolchainHash)) {
		writeError(w, http.StatusBadRequest, "invalid prepared report")
		return
	}
	agent := s.sessionAgent(w, r, req)
	if agent == nil {
		return
	}
	ctx := r.Context()
	key := types.NamespacedName{Namespace: req.ns, Name: req.name}
	if err := s.updateSessionStatus(ctx, key, func(sess *v1alpha1.OmpSession) bool {
		if sess.Status.WorkspaceRevision == p.WorkspaceRevision {
			return false
		}
		sess.Status.WorkspaceRevision = p.WorkspaceRevision
		return true
	}); err != nil {
		s.internalError(w, "update session status", err)
		return
	}
	if agent.Spec.Toolchain != nil {
		agentKey := types.NamespacedName{Namespace: agent.Namespace, Name: agent.Name}
		if err := s.updateAgentStatus(ctx, agentKey, func(a *v1alpha1.OmpAgent) bool {
			if a.Spec.Toolchain == nil || a.Status.Toolchain.Hash == p.ToolchainHash || a.Status.Toolchain.Stale {
				return false
			}
			a.Status.Toolchain.Stale = true
			return true
		}); err != nil {
			s.internalError(w, "update agent status", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSessionToolchain(w http.ResponseWriter, r *http.Request, req *sessionReq) {
	agent := s.sessionAgent(w, r, req)
	if agent == nil {
		return
	}
	hash := agent.Status.Toolchain.Hash
	if agent.Spec.Toolchain == nil || hash == "" {
		writeError(w, http.StatusNotFound, "no toolchain stored")
		return
	}
	f, err := s.Store.OpenToolchain(agent.Namespace, agent.Name, hash)
	s.serveTarball(w, r, f, err)
}

// serveTarball streams f, or answers 404 when it does not exist.
func (s *Server) serveTarball(w http.ResponseWriter, r *http.Request, f *os.File, err error) {
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
		} else {
			s.internalError(w, "open tarball", err)
		}
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		s.internalError(w, "stat tarball", err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

func (s *Server) handleGetContinuity(w http.ResponseWriter, r *http.Request, req *sessionReq) {
	key := req.session.Spec.ContinuityKey
	if key == "" {
		writeError(w, http.StatusNotFound, "session has no continuity")
		return
	}
	f, err := s.Store.OpenContinuity(req.ns, req.session.Spec.AgentRef.Name, store.ContinuityID(key))
	s.serveTarball(w, r, f, err)
}

func (s *Server) handlePutContinuity(w http.ResponseWriter, r *http.Request, req *sessionReq) {
	key := req.session.Spec.ContinuityKey
	if key == "" {
		writeError(w, http.StatusBadRequest, "session has no continuity")
		return
	}
	body := http.MaxBytesReader(w, r.Body, store.MaxContinuityBytes)
	if err := s.Store.PutContinuity(req.ns, req.session.Spec.AgentRef.Name, store.ContinuityID(key), body); err != nil {
		s.storeError(w, "store continuity", err)
		return
	}
	statusKey := types.NamespacedName{Namespace: req.ns, Name: req.name}
	if err := s.updateSessionStatus(r.Context(), statusKey, func(sess *v1alpha1.OmpSession) bool {
		if sess.Status.Continuity != nil && sess.Status.Continuity.Stored {
			return false
		}
		if sess.Status.Continuity == nil {
			sess.Status.Continuity = &v1alpha1.ContinuityStatus{}
		}
		sess.Status.Continuity.Stored = true
		return true
	}); err != nil {
		s.internalError(w, "update session status", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// storeError answers a failed store write: 413 for oversize input, else 500.
func (s *Server) storeError(w http.ResponseWriter, what string, err error) {
	if tooLarge(err) {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	s.internalError(w, what, err)
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request, req *sessionReq) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSmallBody)
	args, err := io.ReadAll(r.Body)
	if err != nil {
		if tooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		} else {
			writeError(w, http.StatusBadRequest, "unreadable body")
		}
		return
	}
	if len(args) == 0 {
		args = []byte("{}")
	}
	if !json.Valid(args) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	agent := s.sessionAgent(w, r, req)
	if agent == nil {
		return
	}
	res, err := s.Runner.Call(r.Context(), agent, req.session, r.PathValue("tool"), args)
	if err != nil {
		if errors.Is(err, ErrUnknownTool) {
			writeError(w, http.StatusNotFound, "unknown tool")
			return
		}
		s.internalError(w, "call action", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, req *sessionReq) {
	var in Events
	if !decodeBody(w, r, &in) {
		return
	}
	if len(in.Events) > maxEventsPerPost {
		writeError(w, http.StatusBadRequest, "too many events")
		return
	}
	now := s.now()
	events := make([]store.Event, 0, len(in.Events))
	for _, e := range in.Events {
		at := e.At
		if at.IsZero() {
			at = now
		}
		kind := truncate(e.Kind, maxEventKindChars)
		if kind == "" {
			kind = "info"
		}
		events = append(events, store.Event{At: at, Kind: kind, Text: truncate(e.Text, maxEventTextChars)})
	}
	if err := s.Store.AppendEvents(string(req.session.UID), events); err != nil {
		s.internalError(w, "append events", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request, req *sessionReq) {
	body := http.MaxBytesReader(w, r.Body, store.MaxTranscriptBytes)
	if err := s.Store.WriteTranscript(string(req.session.UID), body); err != nil {
		s.storeError(w, "store transcript", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var validOutcomes = []string{OutcomeCompleted, OutcomeRetryable, OutcomeAuth, OutcomeError}

func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request, req *sessionReq) {
	var c Complete
	if !decodeBody(w, r, &c) {
		return
	}
	if !slices.Contains(validOutcomes, c.Outcome) || len(c.Usage.Cost) > maxCostChars {
		writeError(w, http.StatusBadRequest, "invalid completion report")
		return
	}
	key := types.NamespacedName{Namespace: req.ns, Name: req.name}
	if err := s.updateSessionStatus(r.Context(), key, func(sess *v1alpha1.OmpSession) bool {
		sess.Status.Outcome = v1alpha1.SessionOutcome(c.Outcome)
		sess.Status.Error = truncate(c.Error, maxErrorChars)
		sess.Status.Usage = &v1alpha1.SessionUsage{
			InputTokens:      c.Usage.InputTokens,
			OutputTokens:     c.Usage.OutputTokens,
			CacheReadTokens:  c.Usage.CacheReadTokens,
			CacheWriteTokens: c.Usage.CacheWriteTokens,
			Cost:             c.Usage.Cost,
		}
		if c.ContinuityRestored || sess.Status.Continuity != nil {
			if sess.Status.Continuity == nil {
				sess.Status.Continuity = &v1alpha1.ContinuityStatus{}
			}
			sess.Status.Continuity.Restored = c.ContinuityRestored
		}
		return true
	}); err != nil {
		s.internalError(w, "update session status", err)
		return
	}
	s.tokens.forget(req.ns, SessionTokenSecretName(req.name))
	w.WriteHeader(http.StatusNoContent)
}

// ---- toolsync (agent) routes ----

// toolsyncAgent loads the agent of a toolsync request; it writes the error response and returns nil on failure.
func (s *Server) toolsyncAgent(w http.ResponseWriter, r *http.Request, req *agentReq) *v1alpha1.OmpAgent {
	agent, err := s.getAgent(r, req.ns, req.name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			s.tokens.forget(req.ns, ToolsyncTokenSecretName(req.name))
			writeError(w, http.StatusUnauthorized, "unauthorized")
		} else {
			s.internalError(w, "get agent", err)
		}
		return nil
	}
	return agent
}

func (s *Server) handleAgentPlan(w http.ResponseWriter, r *http.Request, req *agentReq) {
	agent := s.toolsyncAgent(w, r, req)
	if agent == nil {
		return
	}
	plan, err := s.agentPlan(r.Context(), agent)
	if err != nil {
		s.planError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleAgentToolchain(w http.ResponseWriter, r *http.Request, req *agentReq) {
	agent := s.toolsyncAgent(w, r, req)
	if agent == nil {
		return
	}
	writeJSON(w, http.StatusOK, ToolchainHash{Hash: agent.Status.Toolchain.Hash})
}

func (s *Server) handleToolchainUnchanged(w http.ResponseWriter, r *http.Request, req *agentReq) {
	var h ToolchainHash
	if !decodeBody(w, r, &h) {
		return
	}
	if h.Hash != "" && !validHash.MatchString(h.Hash) {
		writeError(w, http.StatusBadRequest, "invalid hash")
		return
	}
	agent := s.toolsyncAgent(w, r, req)
	if agent == nil {
		return
	}
	if err := s.markToolchainSynced(r.Context(), types.NamespacedName{Namespace: req.ns, Name: req.name}, h.Hash); err != nil {
		s.internalError(w, "update agent status", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleToolchainUpload(w http.ResponseWriter, r *http.Request, req *agentReq) {
	hash := r.PathValue("hash")
	if !validHash.MatchString(hash) {
		writeError(w, http.StatusBadRequest, "invalid hash")
		return
	}
	agent := s.toolsyncAgent(w, r, req)
	if agent == nil {
		return
	}
	body := http.MaxBytesReader(w, r.Body, store.MaxToolchainBytes)
	if err := s.Store.PutToolchain(req.ns, req.name, hash, body); err != nil {
		s.storeError(w, "store toolchain", err)
		return
	}
	if err := s.markToolchainSynced(r.Context(), types.NamespacedName{Namespace: req.ns, Name: req.name}, hash); err != nil {
		s.internalError(w, "update agent status", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
