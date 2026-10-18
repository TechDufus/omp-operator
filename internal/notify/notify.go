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

// Package notify delivers session results and operator notices to ntfy, Slack,
// Discord and generic webhook sinks. It holds no Kubernetes client: callers
// resolve Secret values and pass them to the sink constructors.
package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TechDufus/omp-operator/internal/metrics"
)

const (
	// DefaultNtfyURL is the ntfy server used when none is configured.
	DefaultNtfyURL = "https://ntfy.sh"

	// SignatureHeader carries the webhook sink's HMAC.
	SignatureHeader = "X-Omp-Signature-256"

	attempts     = 3
	retryDelay   = 10 * time.Second
	httpTimeout  = 15 * time.Second
	ellipsis     = "…"
	maxNtfyBytes = 3800
	maxSlack     = 3000
	maxDiscord   = 2000
)

// Sink type names, as used in the notifications metric and the CRD.
const (
	TypeNtfy    = "ntfy"
	TypeSlack   = "slack"
	TypeDiscord = "discord"
	TypeWebhook = "webhook"
)

// ActionRecord is one action call made during a session.
type ActionRecord struct {
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	OK     bool      `json:"ok"`
	At     time.Time `json:"at"`
	Result string    `json:"result,omitempty"`
}

// Message is what a sink announces. Operator notices leave Verdict empty and
// need set only Agent, Title and Summary.
type Message struct {
	// Session is "<namespace>/<name>"; empty for notices.
	Session string
	Agent   string
	Trigger string
	// Verdict is ok, fixed, needs_owner, inconclusive or failed; empty for notices.
	Verdict          string
	Title            string
	Summary          string
	ProposedCommands []string
	Actions          []ActionRecord
	// URL links to the dashboard page; omitted from payloads when empty.
	URL string
}

// request is a fully prepared delivery.
type request struct {
	url    string
	body   []byte
	header http.Header
}

// Sink formats a Message for one destination.
type Sink interface {
	// Type returns the sink type name (TypeNtfy, ...).
	Type() string
	prepare(Message) (request, error)
}

// Sender delivers messages to sinks with retries.
type Sender struct {
	// Client performs the requests; defaults to a client with a 15 s timeout.
	Client *http.Client
	// RetryDelay is the pause between attempts; defaults to 10 s.
	RetryDelay time.Duration
}

// NewSender returns a Sender with production defaults.
func NewSender() *Sender {
	return &Sender{Client: &http.Client{Timeout: httpTimeout}, RetryDelay: retryDelay}
}

// Send delivers msg to sink, trying up to 3 times and waiting RetryDelay
// between attempts. Any transport error or non-2xx status is retried. The
// final outcome is counted in omp_operator_notifications_total. Errors never
// contain the destination URL, which can itself be a secret.
func (s *Sender) Send(ctx context.Context, sink Sink, msg Message) error {
	err := s.send(ctx, sink, msg)
	result := metrics.ResultOK
	if err != nil {
		result = metrics.ResultError
	}
	metrics.NotificationsTotal.WithLabelValues(sink.Type(), result).Inc()
	return err
}

func (s *Sender) send(ctx context.Context, sink Sink, msg Message) error {
	req, err := sink.prepare(msg)
	if err != nil {
		return fmt.Errorf("%s: %w", sink.Type(), err)
	}
	client, delay := s.Client, s.RetryDelay
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	if delay <= 0 {
		delay = retryDelay
	}
	var last error
	for i := range attempts {
		if i > 0 {
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return fmt.Errorf("%s: %w (last error: %w)", sink.Type(), ctx.Err(), last)
			case <-t.C:
			}
		}
		if last = post(ctx, client, req); last == nil {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("%s: %w", sink.Type(), last)
}

func post(ctx context.Context, client *http.Client, r request) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(r.body))
	if err != nil {
		return redactURL(err)
	}
	maps.Copy(req.Header, r.header)
	resp, err := client.Do(req)
	if err != nil {
		return redactURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	return nil
}

// redactURL drops the request URL that net/http embeds in its errors.
func redactURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// truncateBytes cuts s to at most limit bytes on a UTF-8 boundary, marking the
// cut with an ellipsis that counts toward the limit.
func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:max(cut, 0)] + ellipsis
}

// truncateRunes cuts s to at most limit characters, marking the cut with an ellipsis.
func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return string(r[:max(limit-1, 0)]) + ellipsis
}

// chatText builds "<open>title<close>\nsummary\nlink", skipping empty lines.
// When the text exceeds limit characters the summary is shortened so the link
// survives; a head too long on its own is cut as a whole.
func chatText(open, title, closing, summary, link string, limit int) string {
	head := open + title + closing
	tail := link
	budget := limit - utf8.RuneCountInString(head)
	if tail != "" {
		budget -= utf8.RuneCountInString(tail) + 1
	}
	if summary != "" {
		budget-- // newline before the summary
	}
	if budget < 0 {
		budget = 0
		summary = ""
	}
	summary = truncateRunes(summary, budget)
	parts := make([]string, 0, 3)
	for _, p := range []string{head, summary, tail} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return truncateRunes(strings.Join(parts, "\n"), limit)
}
