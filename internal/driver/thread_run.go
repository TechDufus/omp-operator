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
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

const (
	// maxNextWait is the longest single thread/next long-poll the operator allows.
	maxNextWait = 50 * time.Second
	// maxReply is the longest reply sent with a finished turn.
	maxReply = 20000
	// maxTurnEcho is how much of a turn's message its progress event shows.
	maxTurnEcho = 200
	// interruptBudget is what a terminating pod spends saving its work (its grace period is 60s).
	interruptBudget = 45 * time.Second
	// abortTimeout bounds the abort command sent to omp.
	abortTimeout = 10 * time.Second

	errInterrupted = "driver interrupted"
	errOmpExited   = "omp exited"
)

// runThread is Run for a sitting of an OmpThread: one omp process that serves the thread's
// turns one after another until it has been idle too long or its lifetime is nearly used up.
func (d *Driver) runThread(ctx context.Context, cfg agentapi.SessionConfig) error {
	red := d.redactor(cfg)
	if err := d.writeThreadOmpConfig(cfg); err != nil {
		return fmt.Errorf("write omp config: %w", err)
	}
	events := newEventSink(d.API, red, d.EventInterval, d.Log)
	go events.run()
	done := d.sit(ctx, cfg, red, events)
	events.close()

	done.Error = truncate(red.String(done.Error), maxErrorLen)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlTimeout)
	defer cancel()
	if err := d.API.Complete(rctx, done); err != nil {
		return fmt.Errorf("report completion: %w", err)
	}
	d.Log.Info("sitting complete", "outcome", done.Outcome)
	return nil
}

// sit starts omp, serves the turns, and returns the sitting's report. Like runAgent it never
// fails: every problem becomes an outcome.
func (d *Driver) sit(ctx context.Context, cfg agentapi.SessionConfig, red *Redactor, events *eventSink) agentapi.Complete {
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

	cmd := exec.Command(bin, threadOmpArgs(cfg, d.Paths)...)
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

	done := d.serveTurns(ctx, client, cfg, red, events)

	if client.Err() == nil {
		done.Usage = d.usage(context.WithoutCancel(ctx), client)
	}
	if err := client.Close(); err != nil {
		d.Log.Warn("stop omp", "error", err)
	}
	<-drained
	done.FinishCalled = true // thread turns end with their reply; there is no finish to wait for
	return done
}

// serveTurns opens the thread's conversation and runs turns until the sitting ends. The
// outcome is completed for an idle or full sitting; retryable or auth when the pod, omp or the
// model failed in a way a new sitting may survive (the running turn then stays running and is
// delivered again); error when the sitting could not start.
func (d *Driver) serveTurns(ctx context.Context, client *omprpc.Client, cfg agentapi.SessionConfig, red *Redactor,
	events *eventSink) agentapi.Complete {
	var done agentapi.Complete
	th := *cfg.Thread
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
		return failed(agentapi.OutcomeError, "open conversation: "+err.Error())
	}
	done.ContinuityRestored = res.Resumed
	done.Outcome = agentapi.OutcomeCompleted

	turnTimeout := secondsOr(th.TurnTimeoutSeconds, DefaultSessionTimeout)
	idle := secondsOr(th.IdleTimeoutSeconds, 10*time.Minute)
	sitting := secondsOr(th.MaxSittingSeconds, 4*time.Hour)
	start := time.Now()
	idleDeadline := start.Add(idle)
	tr := &threadRun{cfg: th, red: red, events: events, uploaded: d.resultTip(ctx)}

	for turns := 0; ; {
		if ctx.Err() != nil {
			return retry(done, errInterrupted)
		}
		// The first turn is always taken: a sitting that refuses every turn would be replaced by
		// another one that does the same.
		if turns > 0 && !time.Now().Before(start.Add(sitting-turnTimeout)) {
			return done
		}
		turn, err := d.API.NextTurn(ctx, min(maxNextWait, time.Until(idleDeadline)))
		switch {
		case ctx.Err() != nil:
			return retry(done, errInterrupted)
		case err != nil:
			return retry(done, "next turn: "+err.Error())
		case turn == nil:
			if !time.Now().Before(idleDeadline) {
				return done
			}
			continue
		}
		turns++
		if outcome, msg := d.runTurn(ctx, client, tr, *turn, turnTimeout); outcome != "" {
			done.Outcome, done.Error = outcome, msg
			return done
		}
		idleDeadline = time.Now().Add(idle)
	}
}

func retry(done agentapi.Complete, msg string) agentapi.Complete {
	done.Outcome, done.Error = agentapi.OutcomeRetryable, msg
	return done
}

func secondsOr(seconds int64, fallback time.Duration) time.Duration {
	if seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

// runTurn plays one turn and reports it to the operator. It returns an empty outcome when the
// sitting can serve the next turn, else the sitting's outcome and message: the turn is then
// still running on the operator and will be delivered to the next sitting.
func (d *Driver) runTurn(ctx context.Context, client *omprpc.Client, tr *threadRun, turn agentapi.TurnAssignment,
	timeout time.Duration) (outcome, msg string) {
	tr.events.add(eventTurn, fmt.Sprintf("turn %d: %s", turn.ID, truncate(turn.Text, maxTurnEcho)))
	before := d.usage(ctx, client)
	deadline := time.Now().Add(timeout)

	stopWatching := d.watchStop(ctx, client)
	outcome, msg = d.prompt(ctx, client, turn.Text, timeout+d.PromptGrace, deadline)
	stopWatching()
	interrupted := ctx.Err() != nil
	if interrupted {
		d.abort(ctx, client)
	}
	usage := subUsage(d.usage(context.WithoutCancel(ctx), client), before)

	// Saving the work must not be cut short by the very signal that asks for it, but a
	// terminating pod has only its grace period.
	sctx := context.WithoutCancel(ctx)
	if interrupted {
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeout(sctx, interruptBudget)
		defer cancel()
	}
	reply := ""
	if outcome == agentapi.OutcomeCompleted {
		reply = d.lastReply(sctx, client)
	}
	fin := d.finalizeThread(sctx, tr, turn.ID, reply)
	alive := client.Err() == nil
	if alive {
		d.uploadTranscript(sctx, client, tr.red, tr.events)
	}
	d.uploadContinuity(sctx, tr.events)

	if outcome == agentapi.OutcomeAuth || outcome == agentapi.OutcomeRetryable {
		return outcome, msg
	}
	done := agentapi.TurnComplete{
		Outcome:      outcome,
		Error:        truncate(tr.red.String(msg), maxErrorLen),
		Reply:        truncate(tr.red.String(reply), maxReply),
		Usage:        usage,
		Head:         fin.Head,
		FilesChanged: fin.Files,
		Insertions:   fin.Insertions,
		Deletions:    fin.Deletions,
		Pushed:       fin.Pushed,
		PushError:    fin.PushError,
	}
	cctx, cancel := context.WithTimeout(sctx, controlTimeout)
	defer cancel()
	if err := d.API.CompleteTurn(cctx, turn.ID, done); err != nil {
		if se, ok := errors.AsType[*StatusError](err); ok && se.Status == http.StatusConflict {
			// The operator no longer counts this turn as running here; the next one is unaffected.
			d.Log.Warn("turn is no longer running in this sitting", "turn", turn.ID, "error", err)
		} else {
			return agentapi.OutcomeRetryable, "report turn " + fmt.Sprint(turn.ID) + ": " + err.Error()
		}
	}
	if !alive {
		return agentapi.OutcomeRetryable, errOmpExited
	}
	return "", ""
}

// lastReply reads the last assistant text; a failure yields an empty reply.
func (d *Driver) lastReply(ctx context.Context, client *omprpc.Client) string {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	text, err := client.GetLastAssistantText(rctx)
	if err != nil {
		d.Log.Warn("get_last_assistant_text", "error", err)
		return ""
	}
	if text == nil {
		return ""
	}
	return *text
}

// abort asks omp to stop the running prompt, whatever state ctx is in.
func (d *Driver) abort(ctx context.Context, client *omprpc.Client) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	if err := client.Abort(actx); err != nil {
		d.Log.Warn("abort", "error", err)
	}
}

// watchStop polls the operator while a turn runs and aborts the prompt when the turn was
// stopped. The returned func ends the polling and waits for it.
func (d *Driver) watchStop(ctx context.Context, client *omprpc.Client) (stop func()) {
	wctx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		tick := time.NewTicker(d.ControlInterval)
		defer tick.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-tick.C:
			}
			cctx, cancelCall := context.WithTimeout(wctx, controlTimeout)
			stopped, err := d.API.ThreadStop(cctx)
			cancelCall()
			if err != nil {
				if wctx.Err() == nil {
					d.Log.Warn("thread control", "error", err)
				}
				continue
			}
			if stopped {
				d.abort(ctx, client)
				return
			}
		}
	}()
	return func() {
		cancel()
		<-finished
	}
}
