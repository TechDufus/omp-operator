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
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"

	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/prompt"
)

const (
	// maxAskWait is the longest single asks long-poll the operator allows.
	maxAskWait = 50 * time.Second
	// maxQuestionEcho is how much of a question its progress event shows.
	maxQuestionEcho = 200

	// The operator's limits of one question (threads.MaxQuestionTextBytes and friends).
	maxQuestionBytes    = 4000
	maxQuestionHeader   = 200
	maxOptionLabelBytes = 200
	maxOptionDescBytes  = 500
	maxQuestionOptions  = 20

	// askDropped is the answer of a question that was still open when its turn ended.
	askDropped = "The question was dropped because the turn ended before it was answered."
	// askAnsweredByStop is the AnsweredBy of a question the operator closed because the turn was stopped.
	askAnsweredByStop = "stop"
)

// expiredAnswer is what an ask that nobody answered within timeout tells the agent. It is the
// answer text, never a cancellation: omp would answer a timed-out ask with its recommended options.
func expiredAnswer(timeout time.Duration) string {
	return "No answer within " + prompt.FormatDuration(timeout) + ". Continue with your best judgment, " +
		"say in your reply what you assumed, and ask again at the end of your turn if it still matters."
}

// turnClock is a turn's timeout. It runs while the agent works and stands still while the turn
// waits for an answer, so that a slow human does not use up the agent's time.
type turnClock struct {
	mu        sync.Mutex
	remaining time.Duration
	since     time.Time // when the clock last started running
	paused    int       // asks being waited for
	gen       int       // identifies the armed timer, so a stale one is ignored
	timer     *time.Timer
	fired     bool
	stopped   bool
	onFire    func()
}

// newTurnClock starts a clock that calls onFire once limit of running time has passed.
func newTurnClock(limit time.Duration, onFire func()) *turnClock {
	c := &turnClock{remaining: limit, onFire: onFire}
	c.mu.Lock()
	c.arm()
	c.mu.Unlock()
	return c
}

// arm starts the timer for the time that remains. The caller holds c.mu.
func (c *turnClock) arm() {
	c.gen++
	gen := c.gen
	c.since = time.Now()
	c.timer = time.AfterFunc(max(c.remaining, 0), func() { c.fire(gen) })
}

func (c *turnClock) fire(gen int) {
	c.mu.Lock()
	if gen != c.gen || c.paused > 0 || c.fired || c.stopped {
		c.mu.Unlock()
		return
	}
	c.fired = true
	c.mu.Unlock()
	c.onFire()
}

// pause stops the clock until the matching resume; pauses nest.
func (c *turnClock) pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused++
	if c.paused == 1 && !c.fired && !c.stopped {
		c.timer.Stop()
		c.gen++
		c.remaining -= time.Since(c.since)
	}
}

func (c *turnClock) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused == 0 {
		return
	}
	if c.paused--; c.paused == 0 && !c.fired && !c.stopped {
		c.arm()
	}
}

// stop ends the clock without firing.
func (c *turnClock) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	if c.timer != nil {
		c.timer.Stop()
	}
}

// deadline is when the turn times out if it keeps running from now on; a clock that fired reports the past.
func (c *turnClock) deadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	switch {
	case c.fired:
		return now.Add(-time.Second)
	case c.paused > 0 || c.stopped:
		return now.Add(c.remaining)
	}
	return now.Add(c.remaining - time.Since(c.since))
}

// askTurn is the running turn as the ask bridge sees it.
type askTurn struct {
	id     int
	ctx    context.Context
	cancel context.CancelFunc
	clock  *turnClock
	wg     sync.WaitGroup
}

// askBridge answers omp's extension UI requests during a thread sitting. An ask tool call becomes
// questions on the operator, which shows them to the user; the answer (or the question
// timeout) goes back to omp as the tool's result. Every other dialog gets its cancel reply:
// nobody can answer a prompt that exists only on the pod's stdout.
type askBridge struct {
	d       *Driver
	client  *omprpc.Client
	events  *eventSink
	red     *Redactor
	timeout time.Duration // cloud.questionTimeout

	mu   sync.Mutex
	turn *askTurn // the turn being played; nil between turns
}

// enable opts in to ask requests. omp then sends one extension_ui_request of method "ask" per
// ask tool call; without it the questions would arrive as single-choice dialogs. A failure is an
// event, not an error: the agent then gets cancelled questions and carries on.
func (b *askBridge) enable(ctx context.Context) {
	ectx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	on, err := b.client.SetAskDialog(ectx, omprpc.SetAskDialogCommand{Enabled: true})
	switch {
	case err != nil:
		b.d.Log.Warn("set_ask_dialog", "error", err)
		b.events.add(eventNotice, "questions are unavailable: set_ask_dialog: "+truncate(err.Error(), 200))
	case !on:
		b.events.add(eventNotice, "questions are unavailable: omp did not enable the ask dialog")
	}
}

// begin makes turn id the running turn; its questions stop counting toward clock while they wait.
// ctx is the turn's own context: it ends with the turn.
func (b *askBridge) begin(ctx context.Context, id int, clock *turnClock) *askTurn {
	tctx, cancel := context.WithCancel(ctx)
	t := &askTurn{id: id, ctx: tctx, cancel: cancel, clock: clock}
	b.mu.Lock()
	b.turn = t
	b.mu.Unlock()
	return t
}

// end finishes the turn: questions still waiting are dropped, and it returns once they are.
func (b *askBridge) end(t *askTurn) {
	b.mu.Lock()
	if b.turn == t {
		b.turn = nil
	}
	b.mu.Unlock()
	t.cancel()
	t.wg.Wait()
}

// handle is called for every extension UI request omp sends. It never blocks: asks wait in their own goroutine.
func (b *askBridge) handle(req omprpc.ExtensionUiRequest) {
	switch v := req.Value.(type) {
	case omprpc.AskUiRequest:
		b.mu.Lock()
		t := b.turn
		if t != nil {
			t.wg.Add(1)
		}
		b.mu.Unlock()
		if t == nil {
			b.cancelDialog(v.ID)
			return
		}
		go func() {
			defer t.wg.Done()
			b.ask(t, v)
		}()
	case omprpc.SelectUiRequest:
		b.cancelDialog(v.ID)
	case omprpc.ConfirmUiRequest:
		b.cancelDialog(v.ID)
	case omprpc.InputUiRequest:
		b.cancelDialog(v.ID)
	case omprpc.EditorUiRequest:
		b.cancelDialog(v.ID)
	}
	// Notifications, status lines and widgets expect no reply.
}

func (b *askBridge) cancelDialog(id string) {
	if err := b.client.Send(omprpc.CancelUiResponse{ID: id}); err != nil {
		b.d.Log.Warn("cancel dialog", "error", err)
	}
}

// askQuestions converts omp's questions to the operator's, scrubbed and cut to its limits. The
// returned lookups map each question's (scrubbed) option labels back to omp's own, so the
// answer names options exactly as omp offered them.
func (b *askBridge) askQuestions(req omprpc.AskUiRequest) (agentapi.AskRequest, []map[string]string) {
	out := agentapi.AskRequest{Questions: make([]agentapi.AskQuestion, 0, len(req.Questions))}
	labels := make([]map[string]string, 0, len(req.Questions))
	for _, q := range req.Questions {
		aq := agentapi.AskQuestion{
			Key:   cutBytes(q.ID, maxQuestionHeader),
			Text:  cutBytes(b.red.String(q.Question), maxQuestionBytes),
			Multi: q.Multi,
		}
		if q.Header != nil {
			aq.Header = cutBytes(b.red.String(*q.Header), maxQuestionHeader)
		}
		lookup := map[string]string{}
		for i, o := range q.Options {
			if len(aq.Options) == maxQuestionOptions {
				break
			}
			label := cutBytes(b.red.String(o.Label), maxOptionLabelBytes)
			if _, dup := lookup[label]; dup || label == "" {
				continue
			}
			lookup[label] = o.Label
			opt := agentapi.QuestionOption{Label: label}
			if o.Description != nil {
				opt.Description = cutBytes(b.red.String(*o.Description), maxOptionDescBytes)
			}
			if q.Recommended != nil && int(*q.Recommended) == i {
				// The index counts only the options that were kept.
				rec := len(aq.Options)
				aq.Recommended = &rec
			}
			aq.Options = append(aq.Options, opt)
		}
		out.Questions = append(out.Questions, aq)
		labels = append(labels, lookup)
	}
	return out, labels
}

// ask runs one ask tool call to its answer.
func (b *askBridge) ask(t *askTurn, req omprpc.AskUiRequest) {
	t.clock.pause()
	defer t.clock.resume()

	areq, labels := b.askQuestions(req)
	pctx, cancel := context.WithTimeout(t.ctx, controlTimeout)
	st, err := b.d.API.PostAsk(pctx, t.id, areq)
	cancel()
	if err != nil {
		if t.ctx.Err() == nil {
			b.d.Log.Warn("post ask", "error", err)
			b.events.add(eventQuestion, "question not delivered: "+truncate(err.Error(), 200))
			b.cancelDialog(req.ID)
		}
		return
	}
	first := ""
	if len(req.Questions) > 0 {
		first = req.Questions[0].Question
	}
	b.events.add(eventQuestion, "asked: "+truncate(first, maxQuestionEcho))

	final, ok := b.await(t, st)
	if !ok {
		// The turn ended (stopped, interrupted or finished) with the question open. Close it, so that
		// it does not stay pending on the operator for a turn that no longer waits for it.
		b.drop(t, st)
		return
	}
	if stoppedByUser(final) {
		return
	}
	b.events.add(eventAnswer, "answered by "+answeredBy(final))
	if err := b.client.Send(answersResponse(req, final, labels)); err != nil {
		b.d.Log.Warn("send answers", "error", err)
	}
}

// await long-polls the ask until every question is answered or the question timeout runs out,
// when it expires the open questions with the "continue on your own" note. ok is false when the
// turn ended first.
func (b *askBridge) await(t *askTurn, st agentapi.AskState) (agentapi.AskState, bool) {
	deadline := time.Now().Add(b.timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		got, done, err := b.d.API.WaitAsk(t.ctx, t.id, st.Ask, min(left, maxAskWait))
		switch {
		case t.ctx.Err() != nil:
			return st, false
		case err != nil:
			if se, isStatus := errors.AsType[*StatusError](err); isStatus && se.Status < http.StatusInternalServerError &&
				se.Status != http.StatusTooManyRequests {
				// The operator does not know the ask (or the turn) any more: nobody will answer it.
				b.d.Log.Warn("wait for answer", "error", err)
				return expireLocally(st, expiredAnswer(b.timeout)), true
			}
			b.d.Log.Warn("wait for answer", "error", err)
			select {
			case <-t.ctx.Done():
				return st, false
			case <-time.After(min(b.d.ControlInterval, max(left, 0))):
			}
		case done:
			return got, true
		}
	}

	ectx, cancel := context.WithTimeout(t.ctx, controlTimeout)
	defer cancel()
	final, err := b.d.API.ExpireAsk(ectx, t.id, st.Ask, expiredAnswer(b.timeout), agentapi.AskExpireTimeout)
	switch {
	case t.ctx.Err() != nil:
		return st, false
	case err != nil:
		// Without the operator's record the agent still gets its note.
		b.d.Log.Warn("expire ask", "error", err)
		b.events.add(eventQuestion, "question timeout not recorded: "+truncate(err.Error(), 200))
		return expireLocally(st, expiredAnswer(b.timeout)), true
	}
	return final, true
}

// drop closes an ask whose turn ended. It must not depend on the turn's context.
func (b *askBridge) drop(t *askTurn, st agentapi.AskState) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.ctx), abortTimeout)
	defer cancel()
	if _, err := b.d.API.ExpireAsk(ctx, t.id, st.Ask, askDropped, agentapi.AskExpireDropped); err != nil {
		b.d.Log.Warn("drop ask", "error", err)
	}
}

// expireLocally answers every open question of st with text, as the operator would on a timeout.
func expireLocally(st agentapi.AskState, text string) agentapi.AskState {
	st.Questions = slices.Clone(st.Questions)
	for i := range st.Questions {
		if st.Questions[i].AnsweredBy == "" {
			st.Questions[i].Answer, st.Questions[i].AnsweredBy, st.Questions[i].Expired = text, "timeout", true
		}
	}
	st.Done = true
	return st
}

// stoppedByUser is true when the operator closed every question because the turn was stopped: omp is
// being aborted, and there is nobody to answer.
func stoppedByUser(st agentapi.AskState) bool {
	return len(st.Questions) > 0 && !slices.ContainsFunc(st.Questions, func(q agentapi.Question) bool {
		return q.AnsweredBy != askAnsweredByStop
	})
}

// answeredBy names who answered the ask: the distinct answerers, in order.
func answeredBy(st agentapi.AskState) string {
	var who []string
	for _, q := range st.Questions {
		if q.AnsweredBy != "" && !slices.Contains(who, q.AnsweredBy) {
			who = append(who, q.AnsweredBy)
		}
	}
	if len(who) == 0 {
		return "unknown"
	}
	return strings.Join(who, ", ")
}

// answersResponse is the AnswersUiResponse for req from the operator's final state: one answer
// per question, in question order. labels maps each question's option labels back to omp's.
func answersResponse(req omprpc.AskUiRequest, st agentapi.AskState, labels []map[string]string) omprpc.AnswersUiResponse {
	resp := omprpc.AnswersUiResponse{ID: req.ID, Answers: make([]omprpc.AskAnswer, 0, len(req.Questions))}
	for i, q := range req.Questions {
		ans := omprpc.AskAnswer{ID: q.ID, SelectedOptions: []string{}}
		if i < len(st.Questions) {
			var lookup map[string]string
			if i < len(labels) {
				lookup = labels[i]
			}
			selected, custom := st.Questions[i].Selected, st.Questions[i].Answer
			chosen := make([]string, 0, len(selected))
			for _, label := range selected {
				if orig, ok := lookup[label]; ok {
					label = orig
				}
				chosen = append(chosen, label)
			}
			if !q.Multi && len(chosen) > 0 && (custom != "" || len(chosen) > 1) {
				// omp refuses a single-choice answer that carries more than one value. Both the
				// option and the words reach the agent as one custom answer.
				custom = strings.Join(chosen, ", ") + joinNonEmpty(": ", custom)
				chosen = chosen[:0]
			}
			ans.SelectedOptions = chosen
			if custom != "" {
				ans.CustomInput = &custom
			}
		}
		resp.Answers = append(resp.Answers, ans)
	}
	return resp
}

func joinNonEmpty(sep, s string) string {
	if s == "" {
		return ""
	}
	return sep + s
}

// cutBytes cuts s to at most n bytes without splitting a character.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
