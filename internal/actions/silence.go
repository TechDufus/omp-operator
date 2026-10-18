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

package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

const (
	maxMatchers       = 8
	maxMatcherValue   = 200
	minSilenceMinutes = 15

	// maxSilenceResponse bounds what is read back from Alertmanager.
	maxSilenceResponse = 1 << 20
)

var labelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

type silenceMatcher struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type silenceArgs struct {
	Matchers []silenceMatcher `json:"matchers"`
	Minutes  int              `json:"minutes"`
	Reason   string           `json:"reason"`
}

type amMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

type amSilence struct {
	Matchers  []amMatcher `json:"matchers"`
	StartsAt  string      `json:"startsAt"`
	EndsAt    string      `json:"endsAt"`
	CreatedBy string      `json:"createdBy"`
	Comment   string      `json:"comment"`
}

// silence creates an Alertmanager silence for exactly one alertname.
func (r *Runner) silence(ctx context.Context, c callCtx, spec *v1alpha1.AlertmanagerSilenceAction, raw json.RawMessage) outcome {
	var a silenceArgs
	if err := decodeArgs(raw, &a); err != nil {
		return refusal("", "%v", err)
	}
	if err := checkText("reason", a.Reason, reasonMax); err != nil {
		return refusal("", "%v", err)
	}
	if n := len(a.Matchers); n < 1 || n > maxMatchers {
		return refusal("", "matchers must have 1-%d entries", maxMatchers)
	}
	alertname := ""
	alertnames := 0
	var names []string
	for _, m := range a.Matchers {
		if !labelNameRE.MatchString(m.Name) {
			return refusal("", "matcher name %q is not a valid label name", m.Name)
		}
		if n := utf8.RuneCountInString(m.Value); n < 1 || n > maxMatcherValue {
			return refusal("", "matcher %q value must be 1-%d characters", m.Name, maxMatcherValue)
		}
		if slices.Contains(names, m.Name) {
			return refusal("", "matcher %q is given twice", m.Name)
		}
		names = append(names, m.Name)
		if m.Name == "alertname" {
			alertnames++
			alertname = m.Value
		}
	}
	if alertnames != 1 {
		return refusal("", "exactly one matcher must be on alertname")
	}
	target := "alertname=" + alertname
	if slices.Contains(protectedAlertnames(spec), alertname) {
		return refusal(target, "alert %q is protected and cannot be silenced", alertname)
	}
	maxMinutes := int(durationOr(spec.MaxDuration, defaultMaxSilence) / time.Minute)
	if a.Minutes < minSilenceMinutes || a.Minutes > maxMinutes {
		return refusal(target, "minutes must be between %d and %d", minSilenceMinutes, maxMinutes)
	}

	now := r.now().UTC()
	body := amSilence{
		StartsAt:  now.Format(time.RFC3339),
		EndsAt:    now.Add(time.Duration(a.Minutes) * time.Minute).Format(time.RFC3339),
		CreatedBy: fmt.Sprintf("omp-operator/%s/%s", c.session.Namespace, c.agent.Name),
		Comment:   fmt.Sprintf("%s (omp-operator session %s/%s)", a.Reason, c.session.Namespace, c.session.Name),
	}
	for _, m := range a.Matchers {
		body.Matchers = append(body.Matchers, amMatcher{Name: m.Name, Value: m.Value, IsRegex: false, IsEqual: true})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return failure(target, "encoding silence: %v", err)
	}
	endpoint := strings.TrimRight(spec.URL, "/") + "/api/v2/silences"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return failure(target, "building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if spec.AuthSecretRef != nil {
		tok, err := r.secretValue(ctx, c.action.Namespace, *spec.AuthSecretRef)
		if err != nil {
			return failure(target, "reading auth secret: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tok))
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return failure(target, "alertmanager request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSilenceResponse))
	if err != nil {
		return failure(target, "reading alertmanager response: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		return failure(target, "alertmanager answered HTTP %d: %s", resp.StatusCode, truncate(oneLine(string(data)), 300))
	}
	var out struct {
		SilenceID string `json:"silenceID"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.SilenceID == "" {
		return failure(target, "alertmanager answered without a silence id: %s", truncate(oneLine(string(data)), 300))
	}
	return okOutcome(target, "silence %s until %s", out.SilenceID, body.EndsAt)
}
