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

//nolint:goconst // test fixtures repeat literals on purpose
package actions

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

type silenceReq struct {
	Matchers []silenceMatcher `json:"matchers"`
	Minutes  int              `json:"minutes"`
	Reason   string           `json:"reason"`
}

func alertnameOnly(v string) []silenceMatcher { return []silenceMatcher{{"alertname", v}} }

func TestSilenceCreates(t *testing.T) {
	var (
		gotAuth, gotPath, gotCT string
		gotBody                 amSilence
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth, gotPath, gotCT = req.Header.Get("Authorization"), req.URL.Path, req.Header.Get("Content-Type")
		b, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"silenceID":"abc-123"}`))
	}))
	defer srv.Close()

	act := newAction("silence", func(s *v1alpha1.OmpActionSpec) {
		s.AlertmanagerSilence = &v1alpha1.AlertmanagerSilenceAction{
			URL:           srv.URL + "/",
			AuthSecretRef: &v1alpha1.SecretKeyRef{Name: "am", Key: "token"},
		}
	})
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "am"}, Data: map[string][]byte{"token": []byte("s3cret\n")}}
	r, c := newRunner(t, nil, newSession(testSession), act, sec)

	res := call(t, r, newAgent("silence"), "silence", silenceReq{
		Matchers: []silenceMatcher{{"alertname", "HighLatency"}, {"namespace", "web"}}, Minutes: 60, Reason: "known deploy",
	})
	wantEnd := fixedNow.Add(time.Hour).Format(time.RFC3339)
	if res.IsError || res.Text != "silence abc-123 until "+wantEnd {
		t.Fatalf("result = %+v", res)
	}
	if gotPath != "/api/v2/silences" || gotAuth != "Bearer s3cret" || gotCT != "application/json" {
		t.Fatalf("path %q auth %q content-type %q", gotPath, gotAuth, gotCT)
	}
	if gotBody.CreatedBy != "omp-operator/apps/triage" || gotBody.Comment != "known deploy (omp-operator session apps/s1)" ||
		gotBody.StartsAt != fixedNow.Format(time.RFC3339) || gotBody.EndsAt != wantEnd {
		t.Fatalf("body = %+v", gotBody)
	}
	if len(gotBody.Matchers) != 2 || gotBody.Matchers[0] != (amMatcher{Name: "alertname", Value: "HighLatency", IsRegex: false, IsEqual: true}) {
		t.Fatalf("matchers = %+v", gotBody.Matchers)
	}
	if recs := getSession(t, c).Status.Actions; len(recs) != 1 || !recs[0].OK || recs[0].Target != "alertname=HighLatency" {
		t.Fatalf("status.actions = %+v", recs)
	}
}

func TestSilenceRefusals(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"silenceID":"x"}`))
	}))
	defer srv.Close()

	long := strings.Repeat("v", 201)
	tests := []struct {
		name string
		req  silenceReq
		want string
	}{
		{"14 minutes", silenceReq{alertnameOnly("A"), 14, "r"}, "minutes must be between 15 and 1440"},
		{"over max duration", silenceReq{alertnameOnly("A"), 1441, "r"}, "minutes must be between 15 and 1440"},
		{"no alertname", silenceReq{[]silenceMatcher{{"namespace", "web"}}, 30, "r"}, "exactly one matcher must be on alertname"},
		{"duplicate alertname", silenceReq{[]silenceMatcher{{"alertname", "A"}, {"alertname", "B"}}, 30, "r"}, `matcher "alertname" is given twice`},
		{"protected Watchdog", silenceReq{alertnameOnly("Watchdog"), 30, "r"}, "protected"},
		{"protected InfoInhibitor", silenceReq{alertnameOnly("InfoInhibitor"), 30, "r"}, "protected"},
		{"bad label name", silenceReq{[]silenceMatcher{{"alertname", "A"}, {"bad-name", "x"}}, 30, "r"}, `"bad-name" is not a valid label name`},
		{"empty value", silenceReq{[]silenceMatcher{{"alertname", "A"}, {"ns", ""}}, 30, "r"}, "value must be 1-200 characters"},
		{"long value", silenceReq{[]silenceMatcher{{"alertname", "A"}, {"ns", long}}, 30, "r"}, "value must be 1-200 characters"},
		{"no matchers", silenceReq{nil, 30, "r"}, "matchers must have 1-8 entries"},
		{"nine matchers", silenceReq{[]silenceMatcher{{"alertname", "A"}, {"a", "1"}, {"b", "1"}, {"c", "1"}, {"d", "1"}, {"e", "1"}, {"f", "1"}, {"g", "1"}, {"h", "1"}}, 30, "r"}, "matchers must have 1-8 entries"},
		{"no reason", silenceReq{alertnameOnly("A"), 30, ""}, "reason must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			act := newAction("silence", func(s *v1alpha1.OmpActionSpec) {
				s.AlertmanagerSilence = &v1alpha1.AlertmanagerSilenceAction{URL: srv.URL}
			})
			r, _ := newRunner(t, nil, newSession(testSession), act)
			res := call(t, r, newAgent("silence"), "silence", tt.req)
			if !res.IsError || !strings.HasPrefix(res.Text, "refused: ") || !strings.Contains(res.Text, tt.want) {
				t.Fatalf("result = %+v, want refusal containing %q", res, tt.want)
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatalf("alertmanager was called %d times for refused requests", hits.Load())
	}
}

func TestSilenceBoundsHonourSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"silenceID":"x"}`))
	}))
	defer srv.Close()
	act := newAction("silence", func(s *v1alpha1.OmpActionSpec) {
		s.AlertmanagerSilence = &v1alpha1.AlertmanagerSilenceAction{
			URL:                 srv.URL,
			MaxDuration:         metav1.Duration{Duration: 2 * time.Hour},
			ProtectedAlertnames: []string{"Critical"},
		}
	})
	act.Spec.MaxPerSession = 10
	r, _ := newRunner(t, nil, newSession(testSession), act)
	agent := newAgent("silence")
	if res := call(t, r, agent, "silence", silenceReq{alertnameOnly("A"), 120, "r"}); res.IsError {
		t.Fatalf("120 minutes under a 2h max = %+v", res)
	}
	if res := call(t, r, agent, "silence", silenceReq{alertnameOnly("A"), 121, "r"}); !res.IsError {
		t.Fatalf("121 minutes under a 2h max = %+v", res)
	}
	if res := call(t, r, agent, "silence", silenceReq{alertnameOnly("Critical"), 30, "r"}); !res.IsError {
		t.Fatalf("custom protected alertname silenced: %+v", res)
	}
	if res := call(t, r, agent, "silence", silenceReq{alertnameOnly("Watchdog"), 30, "r"}); res.IsError {
		t.Fatalf("the default protected list must be replaced by the spec's: %+v", res)
	}
}

func TestSilenceAlertmanagerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	act := newAction("silence", func(s *v1alpha1.OmpActionSpec) {
		s.AlertmanagerSilence = &v1alpha1.AlertmanagerSilenceAction{URL: srv.URL}
	})
	r, _ := newRunner(t, nil, newSession(testSession), act)
	res := call(t, r, newAgent("silence"), "silence", silenceReq{alertnameOnly("A"), 30, "r"})
	if !res.IsError || !strings.Contains(res.Text, "HTTP 500") {
		t.Fatalf("result = %+v", res)
	}
}
