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
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

// Limits of questions and answers.
const (
	// MaxQuestionsPerTurn is the most questions one turn holds, over all its asks.
	MaxQuestionsPerTurn = 20
	// MaxQuestionTextBytes is the longest question text and the longest free-text answer.
	MaxQuestionTextBytes = 4000
	// MaxOptions is the most options of one question.
	MaxOptions = 20
	// MaxOptionLabelBytes and MaxOptionDescriptionBytes bound one option.
	MaxOptionLabelBytes       = 200
	MaxOptionDescriptionBytes = 500
	// MaxQuestionKeyBytes and MaxQuestionHeaderBytes bound the key and header of a question.
	MaxQuestionKeyBytes    = 200
	MaxQuestionHeaderBytes = 200

	// AnsweredByTimeout and AnsweredByStop are the Question.AnsweredBy of a question
	// that no person answered: the driver's question timeout ran out, or the thread was stopped.
	AnsweredByTimeout = "timeout"
	AnsweredByStop    = "stop"
)

var (
	// ErrInvalidQuestion is returned by Ask for questions that break the limits; every validation error wraps it.
	ErrInvalidQuestion = errors.New("invalid question")
	// ErrTooManyQuestions is returned by Ask when the turn would hold more than MaxQuestionsPerTurn questions.
	ErrTooManyQuestions = fmt.Errorf("%w: a turn holds at most %d questions", ErrInvalidQuestion, MaxQuestionsPerTurn)
	// ErrInvalidAnswer is returned by Answer and ExpireAsk for an answer that breaks the rules; every validation error wraps it.
	ErrInvalidAnswer = errors.New("invalid answer")
	// ErrNotWaiting is returned by Answer when the question is already answered or
	// expired, or its turn is not running.
	ErrNotWaiting = errors.New("question is not waiting for an answer")
	// ErrQuestionNotFound is returned when the turn, question or ask does not exist.
	ErrQuestionNotFound = errors.New("question not found")
)

// QuestionOption is one choice of a question.
type QuestionOption struct {
	// Label is at most MaxOptionLabelBytes bytes and unique within the question.
	Label string `json:"label"`
	// Description is at most MaxOptionDescriptionBytes bytes.
	Description string `json:"description,omitempty"`
}

// Question is a question the agent asked during a turn, and its answer.
type Question struct {
	// ID is 1-based and unique within the turn.
	ID int `json:"id"`
	// Ask is the 1-based number of the ask call within the turn; it groups questions.
	Ask int `json:"ask"`
	// Key is omp's question id inside its ask call.
	Key  string `json:"key"`
	Text string `json:"text"`
	// Header is a short title.
	Header string `json:"header,omitempty"`
	// Options are the choices; free text is always allowed besides them.
	Options []QuestionOption `json:"options,omitempty"`
	Multi   bool             `json:"multi,omitempty"`
	// Recommended is an index into Options.
	Recommended *int      `json:"recommended,omitempty"`
	AskedAt     time.Time `json:"askedAt"`
	// Selected are the chosen option labels.
	Selected []string `json:"selected,omitempty"`
	// Answer is the free-text answer.
	Answer string `json:"answer,omitempty"`
	// AnsweredBy is the principal that answered, or AnsweredByTimeout or AnsweredByStop.
	AnsweredBy string     `json:"answeredBy,omitempty"`
	AnsweredAt *time.Time `json:"answeredAt,omitempty"`
	// Expired is true when the timeout or a stop answered, not a person.
	Expired bool `json:"expired,omitempty"`
}

// Answered reports whether the question has an answer, from a person or an expiry.
func (q Question) Answered() bool { return q.AnsweredAt != nil }

// QuestionInput is one question of an ask call.
type QuestionInput struct {
	Key         string
	Text        string
	Header      string
	Options     []QuestionOption
	Multi       bool
	Recommended *int
}

// Pending is the oldest unanswered question of a thread.
type Pending struct {
	// Turn is the id of the running turn that asked.
	Turn     int
	Question Question
	// Remaining counts the unanswered questions of the same ask, this one included.
	Remaining int
}

// PendingQuestion returns the oldest unanswered question of a running turn, or
// nil when no running turn waits for an answer.
func PendingQuestion(ts []Turn) *Pending {
	for _, t := range ts {
		if t.State != v1alpha1.TurnStateRunning {
			continue
		}
		for _, q := range t.Questions {
			if q.Answered() {
				continue
			}
			p := &Pending{Turn: t.ID, Question: q}
			for _, o := range t.Questions {
				if o.Ask == q.Ask && !o.Answered() {
					p.Remaining++
				}
			}
			return p
		}
	}
	return nil
}

func validateQuestion(i int, in QuestionInput) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: question %d: %s", ErrInvalidQuestion, i+1, fmt.Sprintf(format, a...))
	}
	switch {
	case strings.TrimSpace(in.Text) == "" || len(in.Text) > MaxQuestionTextBytes:
		return bad("text must be 1 to %d bytes", MaxQuestionTextBytes)
	case len(in.Key) > MaxQuestionKeyBytes:
		return bad("key is longer than %d bytes", MaxQuestionKeyBytes)
	case len(in.Header) > MaxQuestionHeaderBytes:
		return bad("header is longer than %d bytes", MaxQuestionHeaderBytes)
	case len(in.Options) > MaxOptions:
		return bad("at most %d options", MaxOptions)
	}
	seen := map[string]bool{}
	for j, o := range in.Options {
		switch {
		case o.Label == "" || len(o.Label) > MaxOptionLabelBytes:
			return bad("option %d: label must be 1 to %d bytes", j+1, MaxOptionLabelBytes)
		case len(o.Description) > MaxOptionDescriptionBytes:
			return bad("option %d: description is longer than %d bytes", j+1, MaxOptionDescriptionBytes)
		case seen[o.Label]:
			return bad("option label %q is not unique", o.Label)
		}
		seen[o.Label] = true
	}
	if r := in.Recommended; r != nil && (*r < 0 || *r >= len(in.Options)) {
		return bad("recommended %d is not an option index", *r)
	}
	return nil
}

// Ask records one ask call of the running turn: qs become new questions with
// the next ids and the next ask number, and are returned with them. It returns
// ErrNotRunning unless the turn is running for session and was not asked to
// stop, ErrTooManyQuestions when the turn would hold more than
// MaxQuestionsPerTurn questions, and an error wrapping ErrInvalidQuestion for a
// question that breaks the limits (an empty call included).
func (l *Log) Ask(ns, thread, session string, turnID int, qs []QuestionInput) (ask int, out []Question, err error) {
	if len(qs) == 0 {
		return 0, nil, fmt.Errorf("%w: no questions", ErrInvalidQuestion)
	}
	for i, q := range qs {
		if err := validateQuestion(i, q); err != nil {
			return 0, nil, err
		}
	}
	var notRunning bool
	err = l.update(ns, thread, func(ts *[]Turn) (bool, error) {
		i := slices.IndexFunc(*ts, func(t Turn) bool { return t.ID == turnID })
		if i < 0 || (*ts)[i].State != v1alpha1.TurnStateRunning || (*ts)[i].Session != session || (*ts)[i].StopRequested {
			notRunning = true
			return false, nil
		}
		t := &(*ts)[i]
		if len(t.Questions)+len(qs) > MaxQuestionsPerTurn {
			return false, ErrTooManyQuestions
		}
		id := 0
		for _, q := range t.Questions {
			id, ask = max(id, q.ID), max(ask, q.Ask)
		}
		ask++
		now := l.now()
		for _, in := range qs {
			id++
			q := Question{
				ID: id, Ask: ask, Key: in.Key, Text: in.Text, Header: in.Header,
				Options: slices.Clone(in.Options), Multi: in.Multi, AskedAt: now,
			}
			if in.Recommended != nil {
				q.Recommended = new(*in.Recommended)
			}
			t.Questions = append(t.Questions, q)
			out = append(out, q)
		}
		return true, nil
	})
	if err != nil {
		return 0, nil, err
	}
	if notRunning {
		return 0, nil, ErrNotRunning
	}
	return ask, out, nil
}

// askOf returns the questions of the ask and whether all are answered. The turn
// must belong to session (ErrNotRunning otherwise); an unknown turn or ask is
// ErrQuestionNotFound.
func askOf(ts []Turn, session string, turnID, ask int) (qs []Question, done bool, err error) {
	i := slices.IndexFunc(ts, func(t Turn) bool { return t.ID == turnID })
	if i < 0 {
		return nil, false, ErrQuestionNotFound
	}
	if ts[i].Session != session {
		return nil, false, ErrNotRunning
	}
	done = true
	for _, q := range ts[i].Questions {
		if q.Ask != ask {
			continue
		}
		qs = append(qs, q)
		done = done && q.Answered()
	}
	if len(qs) == 0 {
		return nil, false, ErrQuestionNotFound
	}
	return qs, done, nil
}

// AskState returns the questions of an ask call with their answers so far; see askOf for the errors.
func (l *Log) AskState(ns, thread, session string, turnID, ask int) ([]Question, error) {
	ts, err := l.Turns(ns, thread)
	if err != nil {
		return nil, err
	}
	qs, _, err := askOf(ts, session, turnID, ask)
	return qs, err
}

// WaitAsk waits up to wait for every question of the ask to have an answer. It
// returns early with done when that is so, waking on Signal for the thread like
// Next, and returns the current state with done false when wait passes. A
// cancelled ctx ends the wait with ctx.Err(). Errors are those of AskState.
func (l *Log) WaitAsk(ctx context.Context, ns, thread, session string, turnID, ask int, wait time.Duration) (qs []Question, done bool, err error) {
	// Subscribe before the first look so a Signal in between is not missed.
	wake, cancel := l.subscribe(key(ns, thread))
	defer cancel()
	timer := time.NewTimer(max(wait, 0))
	defer timer.Stop()
	for {
		ts, err := l.Turns(ns, thread)
		if err != nil {
			return nil, false, err
		}
		qs, done, err := askOf(ts, session, turnID, ask)
		if err != nil || done {
			return qs, done, err
		}
		select {
		case <-wake:
		case <-timer.C:
			return qs, false, nil
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
}

// Answer records by's answer to question qid of turn turnID: selected option
// labels, free text, or both. It returns ErrQuestionNotFound for an unknown turn
// or question, ErrNotWaiting when the question is already answered or expired
// or its turn is not running, and an error wrapping ErrInvalidAnswer when
// selected is not a subset of the option labels, a question without Multi gets
// more than one label, the answer is over MaxQuestionTextBytes, or both are empty.
func (l *Log) Answer(ns, thread string, turnID, qid int, selected []string, answer, by string) (Question, error) {
	answer = strings.TrimSpace(answer)
	var (
		out        Question
		notFound   bool
		notWaiting bool
		invalid    error
	)
	err := l.update(ns, thread, func(ts *[]Turn) (bool, error) {
		i := slices.IndexFunc(*ts, func(t Turn) bool { return t.ID == turnID })
		if i < 0 {
			notFound = true
			return false, nil
		}
		t := &(*ts)[i]
		j := slices.IndexFunc(t.Questions, func(q Question) bool { return q.ID == qid })
		if j < 0 {
			notFound = true
			return false, nil
		}
		q := &t.Questions[j]
		if q.Answered() || t.State != v1alpha1.TurnStateRunning {
			notWaiting = true
			return false, nil
		}
		if invalid = validateAnswer(q, selected, answer); invalid != nil {
			return false, nil
		}
		now := l.now()
		q.Selected, q.Answer, q.AnsweredBy, q.AnsweredAt, q.Expired = slices.Clone(selected), answer, by, &now, false
		out = *q
		return true, nil
	})
	switch {
	case err != nil:
		return Question{}, err
	case notFound:
		return Question{}, ErrQuestionNotFound
	case notWaiting:
		return Question{}, ErrNotWaiting
	case invalid != nil:
		return Question{}, invalid
	}
	return out, nil
}

func validateAnswer(q *Question, selected []string, answer string) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidAnswer, fmt.Sprintf(format, a...))
	}
	switch {
	case len(selected) == 0 && answer == "":
		return bad("select an option or give an answer")
	case len(answer) > MaxQuestionTextBytes:
		return bad("answer is longer than %d bytes", MaxQuestionTextBytes)
	case !q.Multi && len(selected) > 1:
		return bad("the question takes one option")
	}
	seen := map[string]bool{}
	for _, s := range selected {
		if !slices.ContainsFunc(q.Options, func(o QuestionOption) bool { return o.Label == s }) {
			return bad("%q is not an option", s)
		}
		if seen[s] {
			return bad("option %q is selected twice", s)
		}
		seen[s] = true
	}
	return nil
}

// ExpireAsk answers every still-unanswered question of the ask with answer on
// behalf of by (AnsweredByTimeout for the driver's question timeout), marking
// them Expired, and returns the final state of the ask; answered questions are
// unchanged. Errors are those of AskState, plus ErrInvalidAnswer for an answer
// over MaxQuestionTextBytes.
func (l *Log) ExpireAsk(ns, thread, session string, turnID, ask int, answer, by string) ([]Question, error) {
	if len(answer) > MaxQuestionTextBytes {
		return nil, fmt.Errorf("%w: answer is longer than %d bytes", ErrInvalidAnswer, MaxQuestionTextBytes)
	}
	var out []Question
	err := l.update(ns, thread, func(ts *[]Turn) (bool, error) {
		qs, _, err := askOf(*ts, session, turnID, ask)
		if err != nil {
			return false, err
		}
		i := slices.IndexFunc(*ts, func(t Turn) bool { return t.ID == turnID })
		t := &(*ts)[i]
		changed := expireOpen(t.Questions, answer, by, l.now(), ask)
		if changed {
			qs, _, _ = askOf(*ts, session, turnID, ask)
		}
		out = qs
		return changed, nil
	})
	return out, err
}

// expireOpen expires the unanswered questions of qs: of the given ask, or of
// every ask when none is given. It reports whether it changed any.
func expireOpen(qs []Question, answer, by string, now time.Time, ask ...int) bool {
	changed := false
	for i := range qs {
		q := &qs[i]
		if q.Answered() || (len(ask) > 0 && q.Ask != ask[0]) {
			continue
		}
		at := now
		q.Answer, q.AnsweredBy, q.AnsweredAt, q.Expired = answer, by, &at, true
		changed = true
	}
	return changed
}
