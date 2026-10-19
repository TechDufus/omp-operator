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
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	omprpc "github.com/can1357/oh-my-pi/sdk/go/omp-rpc"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// apiAsk is one ask the fake operator holds.
type apiAsk struct {
	turn      int
	n         int
	req       agentapi.AskRequest
	asked     time.Time
	questions []agentapi.Question
	expires   []string // the answer text of every expire call
	reasons   []string // the reason of every expire call
}

// askHuman is the person at the other end: after After they answer every question of every ask.
type askHuman struct {
	After time.Duration
	By    string
	// Answer is the answer to question i (0-based) of the ask.
	Answer func(i int, q agentapi.Question) (selected []string, text string)
}

// askRoute dispatches the asks routes under /thread/turns/{id}/asks.
func (a *threadAPI) asksRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.TrimPrefix(rest, "/thread/turns/"), "/") // {id} asks [{ask} [expire]]
	turnID, _ := strconv.Atoi(parts[0])
	switch {
	case r.Method == http.MethodPost && len(parts) == 2:
		a.postAsk(w, r, turnID)
	case r.Method == http.MethodGet && len(parts) == 3:
		a.waitAsk(w, r, turnID, parts[2])
	case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "expire":
		a.expireAsk(w, r, parts[2])
	default:
		http.NotFound(w, r)
	}
}

func (a *threadAPI) postAsk(w http.ResponseWriter, r *http.Request, turnID int) {
	var req agentapi.AskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if turnID < 1 || turnID > len(a.turns) || a.turns[turnID-1].state != "running" {
		http.Error(w, "turn is not running", http.StatusConflict)
		return
	}
	ask := &apiAsk{turn: turnID, n: len(a.asks) + 1, req: req, asked: time.Now()}
	for i, q := range req.Questions {
		ask.questions = append(ask.questions, agentapi.Question{
			ID: i + 1, Ask: ask.n, Key: q.Key, Text: q.Text, Header: q.Header, Options: q.Options,
			Multi: q.Multi, Recommended: q.Recommended, AskedAt: ask.asked,
		})
	}
	a.asks = append(a.asks, ask)
	w.WriteHeader(http.StatusCreated)
	a.json(w, askState(ask))
}

func askState(ask *apiAsk) agentapi.AskState {
	st := agentapi.AskState{Ask: ask.n, Done: true, Questions: append([]agentapi.Question(nil), ask.questions...)}
	for _, q := range ask.questions {
		st.Done = st.Done && q.AnsweredBy != ""
	}
	return st
}

// waitAsk is the capped long poll of GET asks/{ask}: 200 once every question is answered, else 204.
func (a *threadAPI) waitAsk(w http.ResponseWriter, r *http.Request, turnID int, n string) {
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	deadline := time.Now().Add(min(time.Duration(wait)*time.Second, a.pollCap))
	for {
		a.mu.Lock()
		idx, _ := strconv.Atoi(n)
		if idx < 1 || idx > len(a.asks) || a.asks[idx-1].turn != turnID {
			a.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		ask := a.asks[idx-1]
		a.answerLocked(ask)
		st := askState(ask)
		a.mu.Unlock()
		switch {
		case st.Done:
			a.json(w, st)
			return
		case time.Now().After(deadline), r.Context().Err() != nil:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// answerLocked applies what the operator does on its own: a stopped turn's questions expire
// with "stop", and the human answers after their delay.
func (a *threadAPI) answerLocked(ask *apiAsk) {
	now := time.Now()
	turn := a.turns[ask.turn-1]
	stopped := turn.StopAfter > 0 && !turn.started.IsZero() && now.Sub(turn.started) >= turn.StopAfter
	for i := range ask.questions {
		q := &ask.questions[i]
		if q.AnsweredBy != "" {
			continue
		}
		switch {
		case stopped:
			q.AnsweredBy, q.Expired, q.Answer = "stop", true, "the turn was stopped"
		case a.human != nil && now.Sub(ask.asked) >= a.human.After:
			q.Selected, q.Answer = a.human.Answer(i, *q)
			q.AnsweredBy = a.human.By
		}
		if q.AnsweredBy != "" {
			at := now
			q.AnsweredAt = &at
		}
	}
}

func (a *threadAPI) expireAsk(w http.ResponseWriter, r *http.Request, n string) {
	var body agentapi.AskExpire
	_ = json.NewDecoder(r.Body).Decode(&body)
	a.mu.Lock()
	defer a.mu.Unlock()
	idx, _ := strconv.Atoi(n)
	if idx < 1 || idx > len(a.asks) {
		http.NotFound(w, r)
		return
	}
	ask := a.asks[idx-1]
	ask.expires = append(ask.expires, body.Answer)
	ask.reasons = append(ask.reasons, body.Reason)
	by := "timeout"
	if body.Reason == agentapi.AskExpireDropped {
		by = "dropped"
	}
	for i := range ask.questions {
		if q := &ask.questions[i]; q.AnsweredBy == "" {
			at := time.Now()
			q.Answer, q.AnsweredBy, q.Expired, q.AnsweredAt = body.Answer, by, true, &at
		}
	}
	a.json(w, askState(ask))
}

// stopRequestedLocked is thread/control: whether a running turn was asked to stop. Like the
// operator's Stop, it expires the turn's open questions as stopped before anything can observe
// the request, so the driver's turn-end cleanup never finds them open.
func (a *threadAPI) stopRequestedLocked() bool {
	stop := false
	for _, turn := range a.turns {
		if turn.state != "running" || turn.StopAfter <= 0 || time.Since(turn.started) < turn.StopAfter {
			continue
		}
		stop = true
		for _, ask := range a.asks {
			if ask.turn == turn.ID {
				a.answerLocked(ask)
			}
		}
	}
	return stop
}

// twoQuestions is a turn that asks a single-choice and a multiple-choice question, then replies.
const twoQuestions = `FAKEOMP:{"reply":"went on","ask":{"questions":[` +
	`{"id":"color","question":"Which color?","options":[{"label":"red"},{"label":"blue","description":"cool"}]},` +
	`{"id":"tags","question":"Which tags?","options":[{"label":"a"},{"label":"b"}],"multi":true}]}}`

func TestThreadAskReachesTheOperatorAndTheAnswerReachesTheAgent(t *testing.T) {
	tests := []struct {
		name   string
		answer func(i int, q agentapi.Question) ([]string, string)
		want   string
	}{
		{"options", func(i int, _ agentapi.Question) ([]string, string) {
			if i == 0 {
				return []string{"blue"}, ""
			}
			return []string{"a", "b"}, ""
		}, "went on answers: color=blue; tags=a,b"},
		{"free text for a single choice and options plus text for a multiple choice", func(i int, _ agentapi.Question) ([]string, string) {
			if i == 0 {
				return nil, "teal"
			}
			return []string{"a"}, "and more"
		}, "went on answers: color=teal; tags=a+and more"},
		{"a single choice with an option and words reaches omp as one custom answer", func(i int, _ agentapi.Question) ([]string, string) {
			if i == 0 {
				return []string{"blue"}, "but darker"
			}
			return nil, "none"
		}, "went on answers: color=blue: but darker; tags=none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newThreadEnv(t, nil)
			env.api.human = &askHuman{After: 50 * time.Millisecond, By: "token:dev", Answer: tt.answer}
			env.api.enqueue(twoQuestions)

			runSitting(t, env)

			env.api.mu.Lock()
			defer env.api.mu.Unlock()
			done := env.api.turnDone[1]
			if done.Outcome != agentapi.OutcomeCompleted || done.Reply != tt.want {
				t.Fatalf("turn = %+v, want reply %q", done, tt.want)
			}
			if len(env.api.asks) != 1 || env.api.asks[0].turn != 1 {
				t.Fatalf("asks = %+v, want one ask of turn 1", env.api.asks)
			}
			got := env.api.asks[0].req
			if len(got.Questions) != 2 || got.Questions[0].Key != "color" || got.Questions[0].Text != "Which color?" ||
				got.Questions[0].Multi || len(got.Questions[0].Options) != 2 ||
				got.Questions[0].Options[1] != (agentapi.QuestionOption{Label: "blue", Description: "cool"}) ||
				got.Questions[1].Key != "tags" || !got.Questions[1].Multi {
				t.Fatalf("ask request = %+v", got)
			}
			if len(env.api.asks[0].expires) != 0 {
				t.Fatalf("an answered ask must not be expired: %v", env.api.asks[0].expires)
			}
			var asked, answered []string
			for _, e := range env.api.events {
				switch e.Kind {
				case eventQuestion:
					asked = append(asked, e.Text)
				case eventAnswer:
					answered = append(answered, e.Text)
				}
			}
			if len(asked) != 1 || asked[0] != "asked: Which color?" || len(answered) != 1 || answered[0] != "answered by token:dev" {
				t.Fatalf("question events = %v, answer events = %v", asked, answered)
			}
		})
	}
}

func TestThreadAskTimesOutWithTheNoteAndNeverACancel(t *testing.T) {
	env := newThreadEnv(t, func(cfg *agentapi.SessionConfig) { cfg.Thread.QuestionTimeoutSeconds = 1 })
	env.api.enqueue(twoQuestions) // nobody answers

	start := time.Now()
	runSitting(t, env)

	const note = "No answer within 1s. Continue with your best judgment, say in your reply what you assumed, " +
		"and ask again at the end of your turn if it still matters."
	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("the sitting ended after %v, before the question timeout", elapsed)
	}
	if len(env.api.asks) != 1 || len(env.api.asks[0].expires) != 1 || env.api.asks[0].expires[0] != note ||
		env.api.asks[0].reasons[0] != agentapi.AskExpireTimeout {
		t.Fatalf("asks = %+v, want exactly one expire with the note", env.api.asks)
	}
	// The agent received the note as the answer of every question (omp would have picked a
	// recommended option for a cancellation with timedOut).
	want := "went on answers: color=" + note + "; tags=" + note
	if done := env.api.turnDone[1]; done.Outcome != agentapi.OutcomeCompleted || done.Reply != want {
		t.Fatalf("turn = %+v, want reply %q", done, want)
	}
	var answered []string
	for _, e := range env.api.events {
		if e.Kind == eventAnswer {
			answered = append(answered, e.Text)
		}
	}
	if len(answered) != 1 || answered[0] != "answered by timeout" {
		t.Fatalf("answer events = %v", answered)
	}
}

func TestThreadAskOpenWhenTheTurnEndsIsDropped(t *testing.T) {
	env := newThreadEnv(t, func(cfg *agentapi.SessionConfig) { cfg.Thread.QuestionTimeoutSeconds = 600 })
	env.api.enqueue(twoQuestions) // nobody answers
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for env.api.turnState(1) != "running" || env.api.askCount() == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		cancel() // SIGTERM while the question waits: the turn ends with it open
	}()

	if err := env.d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	if len(env.api.asks) != 1 || len(env.api.asks[0].reasons) != 1 || env.api.asks[0].reasons[0] != agentapi.AskExpireDropped ||
		env.api.asks[0].expires[0] != askDropped {
		t.Fatalf("asks = %+v, want one expire with reason dropped", env.api.asks)
	}
	for _, q := range env.api.asks[0].questions {
		if q.AnsweredBy != "dropped" || !q.Expired || q.Answer != askDropped {
			t.Fatalf("question = %+v, want answered by dropped", q)
		}
	}
}

func (a *threadAPI) askCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.asks)
}

func TestThreadAskStopDuringTheWaitAbortsTheTurn(t *testing.T) {
	env := newThreadEnv(t, func(cfg *agentapi.SessionConfig) { cfg.Thread.QuestionTimeoutSeconds = 60 })
	asking := env.api.enqueue(twoQuestions)
	asking.StopAfter = 300 * time.Millisecond
	env.api.enqueue(`FAKEOMP:{"reply":"after the stop"}`)

	start := time.Now()
	runSitting(t, env)

	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("the stop did not end the wait: %v", elapsed)
	}
	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	if got := env.api.turnDone[1]; got.Outcome != agentapi.OutcomeError || got.Error != "prompt aborted" {
		t.Fatalf("stopped turn = %+v", got)
	}
	if got := env.api.turnDone[2]; got.Outcome != agentapi.OutcomeCompleted || got.Reply != "after the stop" {
		t.Fatalf("the sitting must go on after a stop, turn 2 = %+v", got)
	}
	// The operator expired the question when it stopped the turn; the driver does not answer for it.
	for _, q := range env.api.asks[0].questions {
		if q.AnsweredBy != "stop" {
			t.Fatalf("question = %+v, want answered by stop", q)
		}
	}
	for _, e := range env.api.events {
		if e.Kind == eventAnswer && e.Text == "answered by stop" {
			t.Fatalf("a stopped ask must not be reported as answered: %+v", e)
		}
	}
}

func TestThreadTurnTimeoutPausesWhileAnAskWaits(t *testing.T) {
	oneSecond := func(cfg *agentapi.SessionConfig) {
		cfg.Thread.TurnTimeoutSeconds = 1
		cfg.Thread.QuestionTimeoutSeconds = 30
	}
	t.Run("a slow answer does not time the turn out", func(t *testing.T) {
		env := newThreadEnv(t, oneSecond)
		env.api.human = &askHuman{After: 2500 * time.Millisecond, By: "token:dev",
			Answer: func(int, agentapi.Question) ([]string, string) { return []string{"a"}, "" }}
		env.api.enqueue(`FAKEOMP:{"reply":"ok","ask":{"questions":[{"id":"q","question":"pick","options":[{"label":"a"}]}]}}`)

		runSitting(t, env)

		env.api.mu.Lock()
		defer env.api.mu.Unlock()
		if got := env.api.turnDone[1]; got.Outcome != agentapi.OutcomeCompleted || got.Error != "" || got.Reply != "ok answers: q=a" {
			t.Fatalf("turn = %+v, want completed: the 2.5s wait must not count against the 1s turn timeout", got)
		}
	})
	t.Run("a turn that just works still times out", func(t *testing.T) {
		env := newThreadEnv(t, oneSecond)
		env.api.enqueue(hangScript)

		start := time.Now()
		runSitting(t, env)

		env.api.mu.Lock()
		defer env.api.mu.Unlock()
		if got := env.api.turnDone[1]; got.Outcome != agentapi.OutcomeError || got.Error != errTimedOut {
			t.Fatalf("turn = %+v, want %q", got, errTimedOut)
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Fatalf("timed out after %v", elapsed)
		}
	})
}

func TestTurnClock(t *testing.T) {
	var fired atomic.Int32
	c := newTurnClock(300*time.Millisecond, func() { fired.Add(1) })
	c.pause()
	c.pause() // nested: the clock stands still until both resume
	time.Sleep(500 * time.Millisecond)
	c.resume()
	time.Sleep(100 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatal("the clock fired while an ask was still waiting")
	}
	if left := time.Until(c.deadline()); left > 300*time.Millisecond || left < 100*time.Millisecond {
		t.Fatalf("time left = %v, want what the pauses did not use", left)
	}
	c.resume()
	for deadline := time.Now().Add(5 * time.Second); fired.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if fired.Load() != 1 || time.Until(c.deadline()) > 0 {
		t.Fatalf("fired = %d, deadline in the future: %v", fired.Load(), time.Until(c.deadline()) > 0)
	}

	stopped := newTurnClock(50*time.Millisecond, func() { t.Error("a stopped clock fired") })
	stopped.stop()
	time.Sleep(150 * time.Millisecond)
}

func TestAnswersResponse(t *testing.T) {
	req := omprpc.AskUiRequest{ID: "ui1", Questions: []omprpc.AskQuestion{
		{ID: "single", Question: "s", Options: []omprpc.AskOption{{Label: "x"}}},
		{ID: "multi", Question: "m", Multi: true, Options: []omprpc.AskOption{{Label: "p"}, {Label: "q"}}},
		{ID: "unanswered", Question: "u"},
	}}
	st := agentapi.AskState{Questions: []agentapi.Question{
		{Selected: []string{"x"}, Answer: "with words"},
		{Selected: []string{"p", "q"}, Answer: "and more"},
	}}
	got := answersResponse(req, st, nil)
	if got.ID != "ui1" || len(got.Answers) != 3 {
		t.Fatalf("response = %+v", got)
	}
	single, multi, none := got.Answers[0], got.Answers[1], got.Answers[2]
	if len(single.SelectedOptions) != 0 || single.CustomInput == nil || *single.CustomInput != "x: with words" {
		t.Errorf("single = %+v, want the option and the words as one custom answer", single)
	}
	if len(multi.SelectedOptions) != 2 || multi.CustomInput == nil || *multi.CustomInput != "and more" {
		t.Errorf("multi = %+v", multi)
	}
	if none.SelectedOptions == nil || len(none.SelectedOptions) != 0 || none.CustomInput != nil {
		t.Errorf("a question without an answer = %+v", none)
	}
}

func TestAskQuestionsScrubsCutsAndMapsLabelsBack(t *testing.T) {
	red := NewRedactor([]agentapi.RedactSource{{Name: "KEY", Env: "KEY"}}, func(string) string { return "topsecret-value" }, nil)
	b := &askBridge{red: red}
	long := strings.Repeat("é", 3000) // 6000 bytes
	options := make([]omprpc.AskOption, 0, 28)
	options = append(options, omprpc.AskOption{Label: "use topsecret-value"}, omprpc.AskOption{Label: "use topsecret-value"},
		omprpc.AskOption{Label: "plain"})
	rec := int64(2)
	for i := range 25 {
		options = append(options, omprpc.AskOption{Label: "extra " + strconv.Itoa(i)})
	}
	req, labels := b.askQuestions(omprpc.AskUiRequest{ID: "u", Questions: []omprpc.AskQuestion{
		{ID: "q", Question: "token is topsecret-value? " + long, Header: new("h topsecret-value"), Options: options, Recommended: &rec},
	}})

	q := req.Questions[0]
	if strings.Contains(q.Text, "topsecret-value") || len(q.Text) > maxQuestionBytes || !strings.HasPrefix(q.Text, "token is [redacted:KEY]?") {
		t.Errorf("text = %.60q (%d bytes)", q.Text, len(q.Text))
	}
	if strings.Contains(q.Header, "topsecret-value") {
		t.Errorf("header = %q", q.Header)
	}
	if len(q.Options) != maxQuestionOptions || q.Options[0].Label != "use [redacted:KEY]" || q.Options[1].Label != "plain" {
		t.Errorf("options = %+v (a duplicate is dropped, the list is capped)", q.Options)
	}
	// The recommended option was the third of omp's list, the second that was kept.
	if q.Recommended == nil || *q.Recommended != 1 {
		t.Errorf("recommended = %v, want index 1", q.Recommended)
	}
	if labels[0]["use [redacted:KEY]"] != "use topsecret-value" {
		t.Errorf("labels = %v: an answer must name the option as omp offered it", labels[0])
	}
	resp := answersResponse(omprpc.AskUiRequest{ID: "u", Questions: []omprpc.AskQuestion{{ID: "q"}}},
		agentapi.AskState{Questions: []agentapi.Question{{Selected: []string{"use [redacted:KEY]"}}}}, labels)
	if got := resp.Answers[0].SelectedOptions; len(got) != 1 || got[0] != "use topsecret-value" {
		t.Errorf("selected = %v", got)
	}
}

func TestCutBytes(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"éé", 3, "é"}, // the second é starts at byte 2 and would be split
		{"éé", 4, "éé"},
		{"é", 1, ""},
	}
	for _, tt := range tests {
		if got := cutBytes(tt.in, tt.n); got != tt.want {
			t.Errorf("cutBytes(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

func TestExpiredAnswerNamesTheTimeout(t *testing.T) {
	for in, want := range map[time.Duration]string{
		30 * time.Minute: "No answer within 30m. Continue",
		time.Hour:        "No answer within 1h. Continue",
		90 * time.Minute: "No answer within 1h30m. Continue",
	} {
		if got := expiredAnswer(in); !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "ask again at the end of your turn if it still matters.") {
			t.Errorf("expiredAnswer(%v) = %q", in, got)
		}
	}
}

// pipeOmp is an omp that speaks only enough of the protocol to connect: the test plays the
// server by hand and reads, line by line, what the client sends.
func pipeOmp(t *testing.T) (client *omprpc.Client, fromClient <-chan string) {
	t.Helper()
	srvR, cliW := io.Pipe() // the client writes, the server reads
	cliR, srvW := io.Pipe() // the server writes, the client reads
	ready, _ := json.Marshal(omprpc.ReadyEvent{ProtocolVersion: new(int64(1)), SupportedProtocolVersions: []int64{1}})
	go func() { _, _ = srvW.Write(append(ready, '\n')) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := omprpc.NewClient(ctx, cliR, cliW)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range client.Frames() { //nolint:revive // the bridge under test is called by hand
		}
	}()
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(srvR)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	t.Cleanup(func() {
		_ = srvW.Close() // the client's Close waits for its input to end
		_ = client.Close()
	})
	return client, lines
}

func TestAskBridgeCancelsEveryOtherDialog(t *testing.T) {
	client, fromClient := pipeOmp(t)
	b := &askBridge{
		d: &Driver{Log: discardLogger()}, client: client, red: &Redactor{},
		events: newEventSink(nil, &Redactor{}, time.Hour, discardLogger()),
	}
	ask := omprpc.AskUiRequest{ID: "ask-1", Questions: []omprpc.AskQuestion{{ID: "q", Question: "?"}}}
	requests := []omprpc.ExtensionUiRequestVariant{
		omprpc.SelectUiRequest{ID: "sel-1", Title: "t", Options: []string{"a"}},
		omprpc.ConfirmUiRequest{ID: "conf-1", Title: "t", Message: "m"},
		omprpc.InputUiRequest{ID: "in-1", Title: "t"},
		omprpc.EditorUiRequest{ID: "ed-1", Title: "t"},
		omprpc.NotifyUiRequest{Message: "no reply expected"},
		ask, // between turns nobody can answer an ask either
	}
	for _, v := range requests {
		b.handle(omprpc.ExtensionUiRequest{Value: v})
	}

	got := make([]string, 0, 5)
	for range 5 {
		var line string
		select {
		case line = <-fromClient:
		case <-time.After(10 * time.Second):
			t.Fatalf("no reply after %v", got)
		}
		var resp struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			Cancelled bool   `json:"cancelled"`
			TimedOut  *bool  `json:"timedOut"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Type != "extension_ui_response" || !resp.Cancelled || resp.TimedOut != nil {
			t.Fatalf("reply = %s, want a plain cancel", line)
		}
		got = append(got, resp.ID)
	}
	if want := []string{"sel-1", "conf-1", "in-1", "ed-1", "ask-1"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("cancelled dialogs = %v, want %v (the notification gets no reply)", got, want)
	}
}
