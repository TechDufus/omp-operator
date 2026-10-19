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

package threads

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const sitting = "coder-k3x9fa-1"

// runningTurn enqueues one turn and hands it to the sitting.
func runningTurn(t *testing.T, l *Log) Turn {
	t.Helper()
	enqueue(t, l, "work")
	got := next(t, l, sitting)
	if got == nil {
		t.Fatal("no turn")
	}
	return *got
}

func opts(labels ...string) []QuestionOption {
	out := make([]QuestionOption, len(labels))
	for i, l := range labels {
		out[i] = QuestionOption{Label: l, Description: "about " + l}
	}
	return out
}

func ask(t *testing.T, l *Log, turn int, qs ...QuestionInput) (int, []Question) {
	t.Helper()
	n, out, err := l.Ask(ns, thread, sitting, turn, qs)
	if err != nil {
		t.Fatal(err)
	}
	return n, out
}

func TestAskNumbersAsksAndQuestions(t *testing.T) {
	l := newLog(t)
	turn := runningTurn(t, l)
	rec := 1
	n, out := ask(t, l, turn.ID,
		QuestionInput{Key: "db", Text: "Which database?", Header: "DB", Options: opts("pg", "mysql"), Recommended: &rec},
		QuestionInput{Key: "why", Text: "Why?"},
	)
	if n != 1 || len(out) != 2 || out[0].ID != 1 || out[1].ID != 2 || out[0].Ask != 1 || out[1].Ask != 1 {
		t.Fatalf("first ask = %d %+v", n, out)
	}
	if out[0].Recommended == nil || *out[0].Recommended != 1 || out[0].Key != "db" || out[0].Header != "DB" || out[0].AskedAt.IsZero() {
		t.Errorf("question = %+v", out[0])
	}
	// The stored recommended is a copy, not the caller's pointer.
	rec = 0
	if got := turns(t, l)[0].Questions[0]; *got.Recommended != 1 {
		t.Errorf("stored recommended follows the input pointer: %d", *got.Recommended)
	}
	n, out = ask(t, l, turn.ID, QuestionInput{Key: "more", Text: "More?", Multi: true})
	if n != 2 || out[0].ID != 3 || out[0].Ask != 2 || !out[0].Multi {
		t.Fatalf("second ask = %d %+v", n, out)
	}
}

func TestAskRejects(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	dup := opts("a", "a")
	five := 5
	neg := -1
	tests := []struct {
		name    string
		qs      []QuestionInput
		wantErr error
	}{
		{"no questions", nil, ErrInvalidQuestion},
		{"empty text", []QuestionInput{{Key: "k", Text: "  "}}, ErrInvalidQuestion},
		{"text too long", []QuestionInput{{Key: "k", Text: long(MaxQuestionTextBytes + 1)}}, ErrInvalidQuestion},
		{"text at the limit", []QuestionInput{{Key: "k", Text: long(MaxQuestionTextBytes)}}, nil},
		{"key too long", []QuestionInput{{Key: long(MaxQuestionKeyBytes + 1), Text: "q"}}, ErrInvalidQuestion},
		{"header too long", []QuestionInput{{Key: "k", Text: "q", Header: long(MaxQuestionHeaderBytes + 1)}}, ErrInvalidQuestion},
		{"too many options", []QuestionInput{{Key: "k", Text: "q", Options: make([]QuestionOption, MaxOptions+1)}}, ErrInvalidQuestion},
		{"empty label", []QuestionInput{{Key: "k", Text: "q", Options: []QuestionOption{{Label: ""}}}}, ErrInvalidQuestion},
		{"label too long", []QuestionInput{{Key: "k", Text: "q", Options: []QuestionOption{{Label: long(MaxOptionLabelBytes + 1)}}}}, ErrInvalidQuestion},
		{"description too long", []QuestionInput{{Key: "k", Text: "q", Options: []QuestionOption{{Label: "a", Description: long(MaxOptionDescriptionBytes + 1)}}}}, ErrInvalidQuestion},
		{"duplicate labels", []QuestionInput{{Key: "k", Text: "q", Options: dup}}, ErrInvalidQuestion},
		{"recommended out of range", []QuestionInput{{Key: "k", Text: "q", Options: opts("a"), Recommended: &five}}, ErrInvalidQuestion},
		{"recommended negative", []QuestionInput{{Key: "k", Text: "q", Options: opts("a"), Recommended: &neg}}, ErrInvalidQuestion},
		{"recommended without options", []QuestionInput{{Key: "k", Text: "q", Recommended: new(0)}}, ErrInvalidQuestion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newLog(t)
			turn := runningTurn(t, l)
			_, _, err := l.Ask(ns, thread, sitting, turn.ID, tt.qs)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestAskLimitsAndOwnership(t *testing.T) {
	l := newLog(t)
	turn := runningTurn(t, l)
	one := QuestionInput{Key: "k", Text: "q"}
	for range MaxQuestionsPerTurn {
		ask(t, l, turn.ID, one)
	}
	_, _, err := l.Ask(ns, thread, sitting, turn.ID, []QuestionInput{one})
	if !errors.Is(err, ErrTooManyQuestions) || !errors.Is(err, ErrInvalidQuestion) {
		t.Fatalf("21st question: %v", err)
	}
	if n := len(turns(t, l)[0].Questions); n != MaxQuestionsPerTurn {
		t.Errorf("stored %d questions", n)
	}

	l = newLog(t)
	turn = runningTurn(t, l)
	for name, call := range map[string]func() error{
		"other session": func() error {
			_, _, err := l.Ask(ns, thread, "coder-k3x9fa-2", turn.ID, []QuestionInput{one})
			return err
		},
		"unknown turn": func() error { _, _, err := l.Ask(ns, thread, sitting, 99, []QuestionInput{one}); return err },
	} {
		if err := call(); !errors.Is(err, ErrNotRunning) {
			t.Errorf("%s: %v, want ErrNotRunning", name, err)
		}
	}
	if err := l.Complete(ns, thread, sitting, turn.ID, Result{Outcome: OutcomeCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Ask(ns, thread, sitting, turn.ID, []QuestionInput{one}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("finished turn: %v, want ErrNotRunning", err)
	}

	l = newLog(t)
	turn = runningTurn(t, l)
	if _, _, err := l.Stop(ns, thread, "token:dev"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Ask(ns, thread, sitting, turn.ID, []QuestionInput{one}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stopping turn: %v, want ErrNotRunning", err)
	}
}

func TestAnswer(t *testing.T) {
	setup := func(t *testing.T) (*Log, Turn) {
		l := newLog(t)
		turn := runningTurn(t, l)
		ask(t, l, turn.ID,
			QuestionInput{Key: "single", Text: "one?", Options: opts("a", "b", "c")},
			QuestionInput{Key: "multi", Text: "many?", Options: opts("a", "b", "c"), Multi: true},
			QuestionInput{Key: "free", Text: "free?"},
		)
		return l, turn
	}
	long := strings.Repeat("x", MaxQuestionTextBytes+1)
	tests := []struct {
		name     string
		qid      int
		selected []string
		answer   string
		wantErr  error
	}{
		{"option", 1, []string{"b"}, "", nil},
		{"free text only", 3, nil, "  because  ", nil},
		{"option and text", 1, []string{"a"}, "with caveats", nil},
		{"multi", 2, []string{"a", "c"}, "", nil},
		{"nothing", 1, nil, "", ErrInvalidAnswer},
		{"blank text", 3, nil, "   ", ErrInvalidAnswer},
		{"unknown option", 1, []string{"z"}, "", ErrInvalidAnswer},
		{"single takes one", 1, []string{"a", "b"}, "", ErrInvalidAnswer},
		{"duplicate selection", 2, []string{"a", "a"}, "", ErrInvalidAnswer},
		{"option on a free question", 3, []string{"a"}, "", ErrInvalidAnswer},
		{"answer too long", 3, nil, long, ErrInvalidAnswer},
		{"unknown question", 9, nil, "x", ErrQuestionNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, turn := setup(t)
			q, err := l.Answer(ns, thread, turn.ID, tt.qid, tt.selected, tt.answer, "token:dev")
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.qid > 3 {
				return
			}
			stored := turns(t, l)[0].Questions[tt.qid-1]
			if tt.wantErr != nil {
				if stored.Answered() {
					t.Errorf("rejected answer was stored: %+v", stored)
				}
				return
			}
			if q.AnsweredBy != "token:dev" || q.AnsweredAt == nil || q.Expired || q.Answer != strings.TrimSpace(tt.answer) ||
				strings.Join(q.Selected, ",") != strings.Join(tt.selected, ",") {
				t.Errorf("answer = %+v", q)
			}
			if !stored.Answered() || stored.AnsweredBy != "token:dev" {
				t.Errorf("stored = %+v", stored)
			}
		})
	}

	t.Run("already answered", func(t *testing.T) {
		l, turn := setup(t)
		if _, err := l.Answer(ns, thread, turn.ID, 1, []string{"a"}, "", "token:dev"); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Answer(ns, thread, turn.ID, 1, []string{"b"}, "", "token:other"); !errors.Is(err, ErrNotWaiting) {
			t.Fatalf("second answer: %v, want ErrNotWaiting", err)
		}
		if got := turns(t, l)[0].Questions[0]; got.Selected[0] != "a" || got.AnsweredBy != "token:dev" {
			t.Errorf("second answer overwrote the first: %+v", got)
		}
	})
	t.Run("turn not running", func(t *testing.T) {
		l, turn := setup(t)
		if err := l.Complete(ns, thread, sitting, turn.ID, Result{Outcome: OutcomeCompleted}); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Answer(ns, thread, turn.ID, 1, []string{"a"}, "", "token:dev"); !errors.Is(err, ErrNotWaiting) {
			t.Fatalf("answer after the turn ended: %v, want ErrNotWaiting", err)
		}
		if _, err := l.Answer(ns, thread, 42, 1, []string{"a"}, "", "token:dev"); !errors.Is(err, ErrQuestionNotFound) {
			t.Fatalf("unknown turn: %v, want ErrQuestionNotFound", err)
		}
	})
}

func TestAskStateAndWaitAsk(t *testing.T) {
	l := newLog(t)
	turn := runningTurn(t, l)
	n, _ := ask(t, l, turn.ID,
		QuestionInput{Key: "a", Text: "a?", Options: opts("x")},
		QuestionInput{Key: "b", Text: "b?"},
	)

	qs, err := l.AskState(ns, thread, sitting, turn.ID, n)
	if err != nil || len(qs) != 2 {
		t.Fatalf("AskState = %v, %v", qs, err)
	}
	for name, call := range map[string]func() error{
		"unknown ask":   func() error { _, err := l.AskState(ns, thread, sitting, turn.ID, 7); return err },
		"unknown turn":  func() error { _, err := l.AskState(ns, thread, sitting, 7, n); return err },
		"other session": func() error { _, err := l.AskState(ns, thread, "other-1", turn.ID, n); return err },
	} {
		err := call()
		want := ErrQuestionNotFound
		if name == "other session" {
			want = ErrNotRunning
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}

	// Nothing answered: the wait times out, not done.
	start := time.Now()
	qs, done, err := l.WaitAsk(context.Background(), ns, thread, sitting, turn.ID, n, 50*time.Millisecond)
	if err != nil || done || len(qs) != 2 || time.Since(start) < 40*time.Millisecond {
		t.Fatalf("timeout wait = %v done=%v err=%v after %s", qs, done, err, time.Since(start))
	}

	// A Signal that leaves the ask incomplete does not end the wait; the last answer's Signal does.
	type result struct {
		qs   []Question
		done bool
		err  error
	}
	res := make(chan result, 1)
	go func() {
		qs, done, err := l.WaitAsk(context.Background(), ns, thread, sitting, turn.ID, n, 10*time.Second)
		res <- result{qs, done, err}
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := l.Answer(ns, thread, turn.ID, 1, []string{"x"}, "", "token:dev"); err != nil {
		t.Fatal(err)
	}
	l.Signal(ns, thread)
	select {
	case r := <-res:
		t.Fatalf("returned after a partial answer: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := l.Answer(ns, thread, turn.ID, 2, nil, "because", "token:dev"); err != nil {
		t.Fatal(err)
	}
	l.Signal(ns, thread)
	select {
	case r := <-res:
		if r.err != nil || !r.done || len(r.qs) != 2 || r.qs[1].Answer != "because" {
			t.Fatalf("wait result = %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitAsk did not wake on Signal")
	}

	// Already done: returns at once.
	if _, done, err := l.WaitAsk(context.Background(), ns, thread, sitting, turn.ID, n, time.Minute); err != nil || !done {
		t.Fatalf("done wait: done=%v err=%v", done, err)
	}

	// A cancelled context ends the wait.
	n2, _ := ask(t, l, turn.ID, QuestionInput{Key: "c", Text: "c?"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := l.WaitAsk(ctx, ns, thread, sitting, turn.ID, n2, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
}

func TestExpireAsk(t *testing.T) {
	l := newLog(t)
	turn := runningTurn(t, l)
	n, _ := ask(t, l, turn.ID,
		QuestionInput{Key: "a", Text: "a?", Options: opts("x")},
		QuestionInput{Key: "b", Text: "b?"},
	)
	other, _ := ask(t, l, turn.ID, QuestionInput{Key: "c", Text: "c?"})
	if _, err := l.Answer(ns, thread, turn.ID, 1, []string{"x"}, "", "token:dev"); err != nil {
		t.Fatal(err)
	}

	qs, err := l.ExpireAsk(ns, thread, sitting, turn.ID, n, "carry on", AnsweredByTimeout)
	if err != nil || len(qs) != 2 {
		t.Fatalf("ExpireAsk = %v, %v", qs, err)
	}
	if qs[0].Expired || qs[0].AnsweredBy != "token:dev" || qs[0].Selected[0] != "x" || qs[0].Answer != "" {
		t.Errorf("answered question changed: %+v", qs[0])
	}
	if !qs[1].Expired || qs[1].AnsweredBy != "timeout" || qs[1].Answer != "carry on" || !qs[1].Answered() {
		t.Errorf("expired question = %+v", qs[1])
	}
	// The other ask is untouched and the first is done.
	if got := turns(t, l)[0].Questions[2]; got.Answered() || got.Ask != other {
		t.Errorf("other ask = %+v", got)
	}
	if _, done, _ := l.WaitAsk(context.Background(), ns, thread, sitting, turn.ID, n, 0); !done {
		t.Error("expired ask is not done")
	}
	// Expiring again changes nothing.
	again, err := l.ExpireAsk(ns, thread, sitting, turn.ID, n, "later", AnsweredByTimeout)
	if err != nil || again[1].Answer != "carry on" {
		t.Errorf("second expire = %+v, %v", again, err)
	}
	// An expired question no longer takes answers.
	if _, err := l.Answer(ns, thread, turn.ID, 2, nil, "late", "token:dev"); !errors.Is(err, ErrNotWaiting) {
		t.Errorf("answer after expiry: %v, want ErrNotWaiting", err)
	}

	for name, call := range map[string]func() error{
		"unknown ask":   func() error { _, err := l.ExpireAsk(ns, thread, sitting, turn.ID, 9, "x", "timeout"); return err },
		"other session": func() error { _, err := l.ExpireAsk(ns, thread, "other-1", turn.ID, other, "x", "timeout"); return err },
		"long answer": func() error {
			_, err := l.ExpireAsk(ns, thread, sitting, turn.ID, other, strings.Repeat("x", MaxQuestionTextBytes+1), "timeout")
			return err
		},
	} {
		err := call()
		want := map[string]error{"unknown ask": ErrQuestionNotFound, "other session": ErrNotRunning, "long answer": ErrInvalidAnswer}[name]
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
}

func TestStopExpiresOpenQuestions(t *testing.T) {
	l := newLog(t)
	turn := runningTurn(t, l)
	ask(t, l, turn.ID, QuestionInput{Key: "a", Text: "a?"}, QuestionInput{Key: "b", Text: "b?"})
	if _, err := l.Answer(ns, thread, turn.ID, 1, nil, "yes", "token:dev"); err != nil {
		t.Fatal(err)
	}
	enqueue(t, l, "queued behind")

	stoppedRunning, cancelled, err := l.Stop(ns, thread, "token:other")
	if err != nil || !stoppedRunning || cancelled != 1 {
		t.Fatalf("Stop = %v %d %v", stoppedRunning, cancelled, err)
	}
	qs := turns(t, l)[0].Questions
	if qs[0].AnsweredBy != "token:dev" || qs[0].Expired {
		t.Errorf("answered question changed: %+v", qs[0])
	}
	if !qs[1].Expired || qs[1].AnsweredBy != "stop" || qs[1].Answer != "" || !qs[1].Answered() {
		t.Errorf("open question = %+v", qs[1])
	}
	if p := PendingQuestion(turns(t, l)); p != nil {
		t.Errorf("pending after stop: %+v", p)
	}
}

func TestRequeueClearsQuestions(t *testing.T) {
	l := newLog(t)
	turn := runningTurn(t, l)
	ask(t, l, turn.ID, QuestionInput{Key: "a", Text: "a?"})
	if n, err := l.Requeue(ns, thread, sitting); err != nil || n != 1 {
		t.Fatalf("Requeue = %d, %v", n, err)
	}
	got := turns(t, l)[0]
	if got.State != queued || len(got.Questions) != 0 {
		t.Errorf("requeued turn = %+v", got)
	}
	// The redone turn numbers its asks from 1 again.
	next(t, l, "coder-k3x9fa-2")
	n, out, err := l.Ask(ns, thread, "coder-k3x9fa-2", turn.ID, []QuestionInput{{Key: "a", Text: "a?"}})
	if err != nil || n != 1 || out[0].ID != 1 {
		t.Fatalf("ask after requeue = %d %+v %v", n, out, err)
	}
}

func TestPendingQuestion(t *testing.T) {
	l := newLog(t)
	if p := PendingQuestion(turns(t, l)); p != nil {
		t.Fatalf("empty thread: %+v", p)
	}
	turn := runningTurn(t, l)
	if p := PendingQuestion(turns(t, l)); p != nil {
		t.Fatalf("no questions: %+v", p)
	}
	ask(t, l, turn.ID, QuestionInput{Key: "a", Text: "a?"}, QuestionInput{Key: "b", Text: "b?"})
	ask(t, l, turn.ID, QuestionInput{Key: "c", Text: "c?"})
	p := PendingQuestion(turns(t, l))
	if p == nil || p.Turn != turn.ID || p.Question.ID != 1 || p.Remaining != 2 {
		t.Fatalf("pending = %+v", p)
	}
	if _, err := l.Answer(ns, thread, turn.ID, 1, nil, "x", "u"); err != nil {
		t.Fatal(err)
	}
	if p = PendingQuestion(turns(t, l)); p == nil || p.Question.ID != 2 || p.Remaining != 1 {
		t.Fatalf("after first answer = %+v", p)
	}
	if _, err := l.Answer(ns, thread, turn.ID, 2, nil, "x", "u"); err != nil {
		t.Fatal(err)
	}
	// The next ask's question is now the oldest open one.
	if p = PendingQuestion(turns(t, l)); p == nil || p.Question.ID != 3 || p.Remaining != 1 {
		t.Fatalf("second ask = %+v", p)
	}
	if err := l.Complete(ns, thread, sitting, turn.ID, Result{Outcome: OutcomeCompleted}); err != nil {
		t.Fatal(err)
	}
	if p = PendingQuestion(turns(t, l)); p != nil {
		t.Fatalf("finished turn still pending: %+v", p)
	}
}
