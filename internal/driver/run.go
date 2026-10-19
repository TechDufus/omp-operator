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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

const (
	finishTool = agentapi.FinishToolName
	// nudgeMessage is sent once when the agent ended without a recorded finish.
	nudgeMessage = "You ended without calling finish. Call finish now with your report."
	// maxErrorLen is the longest error the API stores.
	maxErrorLen = 1000
	// rpcTimeout bounds the small omp commands run after the prompt.
	rpcTimeout = time.Minute

	errTimedOut = "session timed out"
)

// runState is what the host tool handlers share with the run loop.
type runState struct {
	finished atomic.Bool
}

// Run is the main container of a session pod: it starts omp, plays the one prompt, and reports
// the outcome. It returns an error only when the driver could not do its job (no config, no
// way to report); an agent that failed still ends with a "complete" report and a nil error.
func (d *Driver) Run(ctx context.Context) error {
	cctx, cancel := child(ctx, controlTimeout)
	cfg, err := d.API.Config(cctx)
	cancel()
	if err != nil {
		return fmt.Errorf("fetch session config: %w", err)
	}
	if cfg.Thread != nil {
		return d.runThread(ctx, cfg)
	}
	red := d.redactor(cfg)
	if err := d.writeOmpConfig(cfg); err != nil {
		return fmt.Errorf("write omp config: %w", err)
	}

	events := newEventSink(d.API, red, d.EventInterval, d.Log)
	go events.run()
	done := d.runAgent(ctx, cfg, red, events)
	events.close()

	done.Error = truncate(red.String(done.Error), maxErrorLen)
	// The pod may be terminating (ctx canceled); the report still gets its chance.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlTimeout)
	defer cancel()
	if err := d.API.Complete(rctx, done); err != nil {
		return fmt.Errorf("report completion: %w", err)
	}
	d.Log.Info("session complete", "outcome", done.Outcome, "finishCalled", done.FinishCalled)
	return nil
}

// redactor builds the replacer for the session's secret sources. The gateway token is always
// included, whether or not the operator listed it.
func (d *Driver) redactor(cfg agentapi.SessionConfig) *Redactor {
	sources := cfg.Redact
	listed := false
	for _, s := range sources {
		listed = listed || s.Env == agentapi.EnvGatewayToken
	}
	if !listed {
		sources = append(slices.Clip(sources), agentapi.RedactSource{
			Name: agentapi.EnvGatewayToken, Env: agentapi.EnvGatewayToken,
		})
	}
	return OSRedactor(sources)
}

// runAgent plays the session and returns its report. It never fails: every problem becomes an outcome.
func (d *Driver) runAgent(ctx context.Context, cfg agentapi.SessionConfig, red *Redactor, events *eventSink) agentapi.Complete {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = DefaultSessionTimeout
	}
	deadline := time.Now().Add(timeout)

	state := &runState{}
	tools, err := d.hostTools(cfg.Actions, red, state)
	if err != nil {
		return failed(agentapi.OutcomeError, err.Error())
	}
	bin, err := exec.LookPath(d.OmpBin)
	if err != nil {
		return failed(agentapi.OutcomeError, "omp executable: "+err.Error())
	}
	if err := os.MkdirAll(d.Paths.Workspace, 0o755); err != nil {
		return failed(agentapi.OutcomeError, err.Error())
	}

	cmd := exec.Command(bin, ompArgs(cfg, timeout, d.Paths)...)
	cmd.Dir = d.Paths.Workspace
	cmd.Env = append(os.Environ(), "HOME="+d.Paths.Home)
	stderr := red.Writer(d.Stderr)
	defer func() { _ = stderr.Close() }()
	cmd.Stderr = stderr

	startCtx, cancel := context.WithTimeout(ctx, d.ReadyTimeout)
	client, err := omprpc.Start(startCtx, cmd, omprpc.WithHostTools(tools...))
	cancel()
	if err != nil {
		return failed(agentapi.OutcomeRetryable, "omp did not become ready: "+err.Error())
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		events.drain(client.Frames())
	}()

	done := d.play(ctx, client, cfg, state, events, deadline)

	alive := client.Err() == nil
	if alive {
		done.Usage = d.usage(ctx, client)
		d.uploadTranscript(ctx, client, red, events)
	}
	if err := client.Close(); err != nil {
		d.Log.Warn("stop omp", "error", err)
	}
	<-drained
	if alive && cfg.Continuity {
		d.uploadContinuity(ctx, events)
	}
	done.FinishCalled = state.finished.Load()
	return done
}

func failed(outcome, msg string) agentapi.Complete {
	return agentapi.Complete{Outcome: outcome, Error: msg}
}

// ompArgs is the plan's command line.
func ompArgs(cfg agentapi.SessionConfig, timeout time.Duration, p Paths) []string {
	args := []string{"--mode", "rpc", "--no-ui", "--approval-mode", approvalYolo}
	if len(cfg.Tools) > 0 {
		args = append(args, "--tools", strings.Join(cfg.Tools, ","))
	} else {
		args = append(args, "--no-tools")
	}
	args = append(args, "--max-time", strconv.FormatInt(int64(timeout/time.Second), 10)+"s",
		"--session-dir", p.SessionDir)
	if cfg.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", p.SystemPrompt)
	}
	return args
}

// play opens the continuity session, sends the prompt, nudges once when finish was not
// called, and classifies how it ended.
func (d *Driver) play(ctx context.Context, client *omprpc.Client, cfg agentapi.SessionConfig, state *runState,
	events *eventSink, deadline time.Time) agentapi.Complete {
	var done agentapi.Complete
	if cfg.Continuity {
		provider, id, err := splitModel(cfg.Model.Default)
		if err != nil {
			return failed(agentapi.OutcomeError, err.Error())
		}
		octx, cancel := context.WithTimeout(ctx, rpcTimeout)
		res, err := client.OpenSession(octx, omprpc.OpenSessionCommand{
			SessionDir: d.Paths.SessionDir, Provider: &provider, ModelID: &id,
		})
		cancel()
		if err != nil {
			return failed(agentapi.OutcomeError, "open continuity session: "+err.Error())
		}
		done.ContinuityRestored = res.Resumed
	}

	outcome, msg := d.prompt(ctx, client, cfg.Prompt, time.Until(deadline)+d.PromptGrace, deadline)
	if outcome == agentapi.OutcomeCompleted && !state.finished.Load() {
		if remaining := time.Until(deadline); remaining > 0 {
			events.add(eventNudge, "agent ended without calling finish; sending one reminder")
			outcome, msg = d.prompt(ctx, client, nudgeMessage, min(remaining, d.NudgeMax), deadline)
		}
	}
	done.Outcome, done.Error = outcome, msg
	return done
}

// prompt sends one prompt, waits at most wait for its result, and aborts the run when the wait expires.
func (d *Driver) prompt(ctx context.Context, client *omprpc.Client, message string, wait time.Duration,
	deadline time.Time) (outcome, msg string) {
	turn, err := client.PromptAndWait(ctx, omprpc.PromptCommand{Message: message}, wait)
	if err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if abortErr := client.Abort(actx); abortErr != nil {
			d.Log.Warn("abort", "error", abortErr)
		}
		cancel()
	}
	return classify(turn, err, deadline, ctx.Err() != nil)
}

// classify maps the end of a prompt to an outcome and message:
//
//	auth       the model answered HTTP 401 or 403
//	retryable  omp never became ready, exited before its prompt_result, reported the failure
//	           as retryable, or the model answered HTTP 429 or 5xx
//	error      a deadline (session timed out), an aborted prompt, a rejected command, anything else
//	completed  everything that finished normally
func classify(turn omprpc.PromptTurn, err error, deadline time.Time, interrupted bool) (outcome, msg string) {
	if err != nil {
		var cmdErr *omprpc.CommandError
		switch {
		case interrupted:
			return agentapi.OutcomeRetryable, "driver interrupted"
		case errors.Is(err, context.DeadlineExceeded):
			return agentapi.OutcomeError, errTimedOut
		case errors.Is(err, omprpc.ErrClosed):
			return agentapi.OutcomeRetryable, "omp exited before prompt_result"
		case errors.As(err, &cmdErr):
			return agentapi.OutcomeError, "prompt rejected: " + cmdErr.Error()
		default:
			return agentapi.OutcomeError, err.Error()
		}
	}
	if turn.Result == nil {
		return agentapi.OutcomeCompleted, ""
	}
	switch turn.Result.Status {
	case omprpc.PromptStatusAborted:
		if !time.Now().Before(deadline) {
			return agentapi.OutcomeError, errTimedOut
		}
		return agentapi.OutcomeError, "prompt aborted"
	case omprpc.PromptStatusError:
		perr := turn.Result.Error
		if perr == nil {
			return agentapi.OutcomeError, "prompt failed"
		}
		status := errorStatus(perr)
		switch {
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return agentapi.OutcomeAuth, perr.Message
		case perr.Retryable || status == http.StatusTooManyRequests || status >= 500:
			return agentapi.OutcomeRetryable, perr.Message
		default:
			return agentapi.OutcomeError, perr.Message
		}
	default:
		return agentapi.OutcomeCompleted, ""
	}
}

// gatewayStatus matches the prefix omp puts on a pi-native failure whose auth-gateway answer is
// not a JSON error object (the gateway answers {"error": "<text>"}).
var gatewayStatus = regexp.MustCompile(`^auth-gateway (\d{3})\b`)

// errorStatus is the HTTP status of a failed prompt. omp reports httpStatus for direct provider
// calls; for models routed through an auth-gateway (transport pi-native) it never does, and the
// status survives only as the "auth-gateway <status>: ..." message prefix.
func errorStatus(perr *omprpc.PromptError) int64 {
	if perr.HTTPStatus != nil {
		return *perr.HTTPStatus
	}
	if m := gatewayStatus.FindStringSubmatch(perr.Message); m != nil {
		if status, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return status
		}
	}
	return 0
}

// hostTools builds one omp host tool per action definition. Each call is forwarded to the
// operator, which runs it; arguments and results are redacted on the way.
func (d *Driver) hostTools(defs []agentapi.ToolDef, red *Redactor, state *runState) ([]omprpc.HostTool, error) {
	tools := make([]omprpc.HostTool, 0, len(defs))
	for _, def := range defs {
		params := map[string]json.RawMessage{}
		if len(def.Parameters) > 0 {
			if err := json.Unmarshal(def.Parameters, &params); err != nil {
				return nil, fmt.Errorf("tool %q: parameters are not a JSON object: %w", def.Name, err)
			}
		}
		if len(params) == 0 {
			params["type"] = json.RawMessage(`"object"`)
		}
		tools = append(tools, omprpc.HostTool{
			Definition: omprpc.HostToolDefinition{
				Name:        def.Name,
				Description: def.Description,
				Parameters:  params,
				LoadMode:    omprpc.Ptr(omprpc.ToolLoadModeEssential),
			},
			Handler: d.toolHandler(def.Name, red, state),
		})
	}
	return tools, nil
}

func (d *Driver) toolHandler(name string, red *Redactor, state *runState) omprpc.HostToolHandler {
	return func(ctx context.Context, _ *omprpc.HostToolCall, args json.RawMessage) (omprpc.HostToolResultPayload, error) {
		res, err := d.API.Action(ctx, name, red.Raw(args))
		if err != nil {
			d.Log.Warn("action call failed", "tool", name, "error", red.String(err.Error()))
			return toolResult("action failed: "+red.String(err.Error()), true), nil
		}
		if name == finishTool && !res.IsError {
			state.finished.Store(true)
		}
		return toolResult(red.String(res.Text), res.IsError), nil
	}
}

func toolResult(text string, isError bool) omprpc.HostToolResultPayload {
	res := omprpc.TextResult(text)
	if isError {
		res.IsError = new(true)
	}
	return res
}

// usage reads the session statistics; a failure yields zero usage.
func (d *Driver) usage(ctx context.Context, client *omprpc.Client) agentapi.Usage {
	sctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	stats, err := client.GetSessionStats(sctx)
	if err != nil {
		d.Log.Warn("get_session_stats", "error", err)
		return agentapi.Usage{}
	}
	return agentapi.Usage{
		InputTokens:      stats.Tokens.Input,
		OutputTokens:     stats.Tokens.Output,
		CacheReadTokens:  stats.Tokens.CacheRead,
		CacheWriteTokens: stats.Tokens.CacheWrite,
		Cost:             strconv.FormatFloat(stats.Cost, 'f', -1, 64),
	}
}

// uploadTranscript exports the session as HTML, redacts it and uploads it. Failures are
// reported as events and never fail the session.
func (d *Driver) uploadTranscript(ctx context.Context, client *omprpc.Client, red *Redactor, events *eventSink) {
	ectx, cancel := context.WithTimeout(ctx, rpcTimeout)
	_, err := client.ExportHTML(ectx, omprpc.ExportHTMLCommand{OutputPath: &d.Paths.Transcript})
	cancel()
	if err != nil {
		events.add(eventTranscript, "transcript export failed: "+err.Error())
		return
	}
	if err := d.putTranscriptFile(ctx, d.Paths.Transcript, red); err != nil {
		events.add(eventTranscript, "transcript upload failed: "+err.Error())
	}
}

// maxTranscript is the largest transcript the operator accepts.
const maxTranscript = 64 << 20

func (d *Driver) putTranscriptFile(ctx context.Context, file string, red *Redactor) error {
	st, err := os.Stat(file)
	if err != nil {
		return err
	}
	if st.Size() > maxTranscript {
		return fmt.Errorf("transcript is %d bytes, over the %d byte limit", st.Size(), maxTranscript)
	}
	html, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	uctx, cancel := context.WithTimeout(ctx, tarballTimeout)
	defer cancel()
	return d.API.PutTranscript(uctx, red.Bytes(html))
}

// uploadContinuity packs the omp session directory and uploads it. Over the size limit it is
// skipped with an event.
func (d *Driver) uploadContinuity(ctx context.Context, events *eventSink) {
	tmp, err := os.CreateTemp("", "continuity-*.tar.gz")
	if err != nil {
		events.add(eventContinuity, "continuity not stored: "+err.Error())
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	packErr := WriteTarGz(tmp, d.Paths.SessionDir, "")
	size := int64(0)
	if st, err := tmp.Stat(); err == nil {
		size = st.Size()
	}
	if err := errors.Join(packErr, tmp.Close()); err != nil {
		events.add(eventContinuity, "continuity not stored: "+err.Error())
		return
	}
	if size > d.ContinuityMax {
		events.add(eventContinuity, fmt.Sprintf("continuity not stored: %d bytes exceeds the %d byte limit", size, d.ContinuityMax))
		return
	}
	uctx, cancel := context.WithTimeout(ctx, tarballTimeout)
	defer cancel()
	if err := d.API.PutFile(uctx, "/continuity", tmp.Name()); err != nil {
		events.add(eventContinuity, "continuity not stored: "+err.Error())
	}
}
