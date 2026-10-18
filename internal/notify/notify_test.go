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

package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/TechDufus/omp-operator/internal/metrics"
)

type captured struct {
	mu     sync.Mutex
	bodies [][]byte
	header []http.Header
}

func (c *captured) add(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, b)
	c.header = append(c.header, r.Header.Clone())
}

// server records every request and answers with the given statuses in order,
// repeating the last one.
func server(t *testing.T, statuses ...int) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.add(r)
		c.mu.Lock()
		n := len(c.bodies)
		c.mu.Unlock()
		w.WriteHeader(statuses[min(n, len(statuses))-1])
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func fastSender() *Sender { return &Sender{RetryDelay: time.Millisecond} }

var full = Message{
	Session:          "prod/web-20261018-101500-ab12",
	Agent:            "web",
	Trigger:          "alerts",
	Verdict:          "needs_owner",
	Title:            "Disk full",
	Summary:          "/var is at 99%",
	ProposedCommands: []string{"kubectl delete pod x"},
	Actions:          []ActionRecord{{Action: "restart", Target: "prod/web", OK: true, At: time.Date(2026, 10, 18, 10, 15, 0, 0, time.UTC), Result: "restarted"}},
	URL:              "https://dash.example/sessions/prod/web-1",
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, b)
	}
	return m
}

func TestNtfy(t *testing.T) {
	srv, c := server(t, 200)
	if err := fastSender().Send(t.Context(), NewNtfy(srv.URL, full.Trigger), full); err != nil {
		t.Fatal(err)
	}
	got := decode(t, c.bodies[0])
	want := map[string]any{
		"topic": full.Trigger, "title": full.Title, "message": full.Summary,
		"priority": float64(4), "tags": []any{"eye"}, "click": full.URL,
	}
	if len(got) != len(want) {
		t.Errorf("payload keys = %v, want %v", got, want)
	}
	for k, w := range want {
		if g, _ := json.Marshal(got[k]); string(g) != mustJSON(w) {
			t.Errorf("%s = %s, want %s", k, g, mustJSON(w))
		}
	}
	if ct := c.header[0].Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestNtfyPriorityAndOptionalClick(t *testing.T) {
	for verdict, want := range map[string]float64{
		"ok": 2, "fixed": 3, "inconclusive": 3, full.Verdict: 4, "failed": 3, "": 3,
	} {
		srv, c := server(t, 200)
		m := Message{Verdict: verdict, Title: "t", Summary: "s"}
		if err := fastSender().Send(t.Context(), NewNtfy(srv.URL, "x"), m); err != nil {
			t.Fatal(err)
		}
		got := decode(t, c.bodies[0])
		if got["priority"] != want {
			t.Errorf("verdict %q priority = %v, want %v", verdict, got["priority"], want)
		}
		if _, ok := got["click"]; ok {
			t.Errorf("verdict %q: click present without URL", verdict)
		}
	}
}

func TestNtfyDefaultsAndValidation(t *testing.T) {
	if s := NewNtfy("", "t").(ntfySink); s.url != "https://ntfy.sh" {
		t.Errorf("default url = %q", s.url)
	}
	if _, err := NewNtfy("http://x", "").prepare(full); err == nil {
		t.Error("empty topic accepted")
	}
}

func TestNtfyTruncatesOnUTF8Boundary(t *testing.T) {
	// "é" is 2 bytes, so a cut at byte 3797 would split a character.
	summary := strings.Repeat("é", 3000)
	srv, c := server(t, 200)
	if err := fastSender().Send(t.Context(), NewNtfy(srv.URL, "x"), Message{Title: "t", Summary: summary}); err != nil {
		t.Fatal(err)
	}
	msg := decode(t, c.bodies[0])["message"].(string)
	if len(msg) > 3800 || !utf8.ValidString(msg) || !strings.HasSuffix(msg, "…") {
		t.Fatalf("message len=%d valid=%v", len(msg), utf8.ValidString(msg))
	}
	if !strings.HasPrefix(msg, "éé") || strings.Contains(strings.TrimSuffix(msg, "…"), "\ufffd") {
		t.Error("message body corrupted")
	}
}

func TestSlackAndDiscord(t *testing.T) {
	tests := []struct {
		name string
		sink func(string) Sink
		key  string
		text func(title, summary, link string) string
	}{
		{"slack", NewSlack, "text", func(t, s, l string) string { return "*" + t + "*\n" + s + "\n" + l }},
		{"discord", NewDiscord, "content", func(t, s, l string) string { return "**" + t + "**\n" + s + "\n" + l }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, c := server(t, 204)
			if err := fastSender().Send(t.Context(), tc.sink(srv.URL), full); err != nil {
				t.Fatal(err)
			}
			got := decode(t, c.bodies[0])
			if len(got) != 1 || got[tc.key] != tc.text(full.Title, full.Summary, full.URL) {
				t.Errorf("payload = %v", got)
			}
		})
	}
}

func TestChatLinkOmittedWhenURLEmpty(t *testing.T) {
	for _, s := range []Sink{NewSlack("http://x"), NewDiscord("http://x")} {
		req, err := s.prepare(Message{Title: "T", Summary: "S"})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range decode(t, req.body) {
			if strings.HasSuffix(v.(string), "\n") || strings.Count(v.(string), "\n") != 1 {
				t.Errorf("%s text = %q, want exactly title and summary lines", s.Type(), v)
			}
		}
	}
}

func TestChatTruncationKeepsLinkAndLimit(t *testing.T) {
	long := strings.Repeat("日", 5000) // multi-byte: limits count characters
	tests := []struct {
		sink  Sink
		key   string
		limit int
	}{
		{NewSlack("http://x"), "text", 3000},
		{NewDiscord("http://x"), "content", 2000},
	}
	for _, tc := range tests {
		t.Run(tc.sink.Type(), func(t *testing.T) {
			req, err := tc.sink.prepare(Message{Title: "T", Summary: long, URL: "https://dash/x"})
			if err != nil {
				t.Fatal(err)
			}
			text := decode(t, req.body)[tc.key].(string)
			if n := utf8.RuneCountInString(text); n != tc.limit {
				t.Errorf("length = %d characters, want %d", n, tc.limit)
			}
			if !strings.HasSuffix(text, "…\nhttps://dash/x") {
				t.Errorf("link lost: ...%q", text[len(text)-30:])
			}
		})
	}
}

func TestChatOversizedTitleStillBounded(t *testing.T) {
	req, _ := NewDiscord("http://x").prepare(Message{Title: strings.Repeat("a", 5000), Summary: "s", URL: "u"})
	if n := utf8.RuneCountInString(decode(t, req.body)["content"].(string)); n != 2000 {
		t.Errorf("length = %d, want 2000", n)
	}
}

func TestWebhookPayloadAndSignature(t *testing.T) {
	secret := []byte("s3cret")
	srv, c := server(t, 200)
	if err := fastSender().Send(t.Context(), NewWebhook(srv.URL, secret), full); err != nil {
		t.Fatal(err)
	}
	body := c.bodies[0]
	got := decode(t, body)
	for k, want := range map[string]string{
		"session": full.Session, "agent": full.Agent, "trigger": full.Trigger, "verdict": full.Verdict,
		"title": full.Title, "summary": full.Summary, "url": full.URL,
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	if mustJSON(got["proposedCommands"]) != `["kubectl delete pod x"]` {
		t.Errorf("proposedCommands = %v", got["proposedCommands"])
	}
	acts := got["actions"].([]any)[0].(map[string]any)
	if acts["action"] != "restart" || acts["target"] != "prod/web" || acts["ok"] != true ||
		acts["at"] != "2026-10-18T10:15:00Z" || acts["result"] != "restarted" {
		t.Errorf("actions[0] = %v", acts)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); c.header[0].Get(SignatureHeader) != want {
		t.Errorf("%s = %q, want %q", SignatureHeader, c.header[0].Get(SignatureHeader), want)
	}
}

func TestWebhookNoSecretNoHeaderAndEmptyListsNotNull(t *testing.T) {
	srv, c := server(t, 200)
	if err := fastSender().Send(t.Context(), NewWebhook(srv.URL, nil), Message{Agent: "a", Title: "notice"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.header[0][SignatureHeader]; ok {
		t.Error("signature header sent without secret")
	}
	got := decode(t, c.bodies[0])
	if mustJSON(got["proposedCommands"]) != "[]" || mustJSON(got["actions"]) != "[]" {
		t.Errorf("lists = %v %v, want []", got["proposedCommands"], got["actions"])
	}
}

func TestRetryOn500ThenSuccess(t *testing.T) {
	srv, c := server(t, 500, 502, 200)
	before := testutil.ToFloat64(metrics.NotificationsTotal.WithLabelValues("slack", "ok"))
	if err := fastSender().Send(t.Context(), NewSlack(srv.URL), full); err != nil {
		t.Fatal(err)
	}
	if len(c.bodies) != 3 {
		t.Fatalf("requests = %d, want 3", len(c.bodies))
	}
	if got := testutil.ToFloat64(metrics.NotificationsTotal.WithLabelValues("slack", "ok")) - before; got != 1 {
		t.Errorf("ok counter delta = %v, want exactly 1 per Send", got)
	}
}

func TestGivesUpAfterThreeAttempts(t *testing.T) {
	srv, c := server(t, 500)
	before := testutil.ToFloat64(metrics.NotificationsTotal.WithLabelValues("discord", "error"))
	err := fastSender().Send(t.Context(), NewDiscord(srv.URL), full)
	if err == nil {
		t.Fatal("want error")
	}
	if len(c.bodies) != 3 {
		t.Errorf("requests = %d, want 3", len(c.bodies))
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error leaks URL: %v", err)
	}
	if got := testutil.ToFloat64(metrics.NotificationsTotal.WithLabelValues("discord", "error")) - before; got != 1 {
		t.Errorf("error counter delta = %v, want 1", got)
	}
}

func TestTransportErrorDoesNotLeakURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/secret-token"
	srv.Close()
	err := fastSender().Send(t.Context(), NewSlack(url), full)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("err = %v", err)
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	srv, c := server(t, 500)
	ctx, cancel := context.WithCancel(t.Context())
	s := &Sender{RetryDelay: time.Hour}
	go func() {
		for {
			c.mu.Lock()
			n := len(c.bodies)
			c.mu.Unlock()
			if n >= 1 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	done := make(chan error, 1)
	go func() { done <- s.Send(ctx, NewSlack(srv.URL), full) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not honour context cancellation")
	}
	if len(c.bodies) != 1 {
		t.Errorf("requests = %d, want 1", len(c.bodies))
	}
}

func TestMissingURLFailsWithoutRequest(t *testing.T) {
	if err := fastSender().Send(t.Context(), NewWebhook("", nil), full); err == nil {
		t.Error("want error for empty URL")
	}
}
