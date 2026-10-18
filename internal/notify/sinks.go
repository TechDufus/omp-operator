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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
)

var jsonHeader = http.Header{"Content-Type": {"application/json"}}

func jsonRequest(url string, payload any) (request, error) {
	if url == "" {
		return request{}, errors.New("no URL configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return request{}, err
	}
	return request{url: url, body: body, header: jsonHeader.Clone()}, nil
}

type ntfySink struct{ url, topic string }

// NewNtfy returns an ntfy sink publishing JSON to baseURL (DefaultNtfyURL when
// empty) on the given topic.
func NewNtfy(baseURL, topic string) Sink {
	if baseURL == "" {
		baseURL = DefaultNtfyURL
	}
	return ntfySink{url: baseURL, topic: topic}
}

func (ntfySink) Type() string { return TypeNtfy }

func ntfyPriority(verdict string) int {
	switch verdict {
	case "ok":
		return 2
	case "needs_owner":
		return 4
	default: // fixed, inconclusive, failed and notices
		return 3
	}
}

func (s ntfySink) prepare(m Message) (request, error) {
	if s.topic == "" {
		return request{}, errors.New("no topic configured")
	}
	return jsonRequest(s.url, struct {
		Topic    string   `json:"topic"`
		Title    string   `json:"title"`
		Message  string   `json:"message"`
		Priority int      `json:"priority"`
		Tags     []string `json:"tags"`
		Click    string   `json:"click,omitempty"`
	}{s.topic, m.Title, truncateBytes(m.Summary, maxNtfyBytes), ntfyPriority(m.Verdict), []string{"eye"}, m.URL})
}

type slackSink struct{ url string }

// NewSlack returns a Slack incoming-webhook sink.
func NewSlack(webhookURL string) Sink { return slackSink{url: webhookURL} }

func (slackSink) Type() string { return TypeSlack }

func (s slackSink) prepare(m Message) (request, error) {
	return jsonRequest(s.url, map[string]string{
		"text": chatText("*", m.Title, "*", m.Summary, m.URL, maxSlack),
	})
}

type discordSink struct{ url string }

// NewDiscord returns a Discord webhook sink.
func NewDiscord(webhookURL string) Sink { return discordSink{url: webhookURL} }

func (discordSink) Type() string { return TypeDiscord }

func (s discordSink) prepare(m Message) (request, error) {
	return jsonRequest(s.url, map[string]string{
		"content": chatText("**", m.Title, "**", m.Summary, m.URL, maxDiscord),
	})
}

type webhookSink struct {
	url  string
	hmac []byte
}

// NewWebhook returns a generic JSON webhook sink. A non-empty hmacSecret adds
// X-Omp-Signature-256: sha256=<hex HMAC-SHA256 of the body>.
func NewWebhook(webhookURL string, hmacSecret []byte) Sink {
	return webhookSink{url: webhookURL, hmac: hmacSecret}
}

func (webhookSink) Type() string { return TypeWebhook }

func (s webhookSink) prepare(m Message) (request, error) {
	cmds, acts := m.ProposedCommands, m.Actions
	if cmds == nil {
		cmds = []string{}
	}
	if acts == nil {
		acts = []ActionRecord{}
	}
	req, err := jsonRequest(s.url, struct {
		Session          string         `json:"session"`
		Agent            string         `json:"agent"`
		Trigger          string         `json:"trigger"`
		Verdict          string         `json:"verdict"`
		Title            string         `json:"title"`
		Summary          string         `json:"summary"`
		ProposedCommands []string       `json:"proposedCommands"`
		Actions          []ActionRecord `json:"actions"`
		URL              string         `json:"url"`
	}{m.Session, m.Agent, m.Trigger, m.Verdict, m.Title, m.Summary, cmds, acts, m.URL})
	if err != nil {
		return request{}, err
	}
	if len(s.hmac) > 0 {
		mac := hmac.New(sha256.New, s.hmac)
		mac.Write(req.body)
		req.header.Set(SignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return req, nil
}
