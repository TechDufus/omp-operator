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

package hookauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	token       = "s3cret"
	user        = "alice"
	pass        = "pw"
	hmacSecret  = "hmac-secret"
	sigHeader   = "X-Sig"
	tsHeader    = "X-Ts"
	grafanaSig  = "X-Grafana-Alerting-Signature"
	grafanaTime = "X-Grafana-Alerting-Timestamp"
)

var testNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func newReq(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/hooks/ns/t", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func sign(secret string, parts ...string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	for _, p := range parts {
		mac.Write([]byte(p))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func TestBearer(t *testing.T) {
	tests := []struct {
		name   string
		header string
		token  string
		want   bool
	}{
		{"good", "Bearer " + token, token, true},
		{"scheme case-insensitive", "bearer " + token, token, true},
		{"bad", "Bearer nope", token, false},
		{"prefix of token", "Bearer s3cre", token, false},
		{"missing", "", token, false},
		{"wrong scheme", "Basic " + token, token, false},
		{"no scheme", token, token, false},
		{"empty configured token", "Bearer ", "", false},
		{"empty configured token no header", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newReq(map[string]string{"Authorization": tc.header})
			if got := (Bearer{Token: tc.token}).Authenticate(r, nil, testNow); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBasic(t *testing.T) {
	tests := []struct {
		name       string
		user, pass string
		set        bool
		cfgUser    string
		cfgPass    string
		want       bool
	}{
		{"good", user, pass, true, user, pass, true},
		{"bad password", user, "bad", true, user, pass, false},
		{"bad user", "bob", pass, true, user, pass, false},
		{"missing", "", "", false, user, pass, false},
		{"empty creds configured", "", "", true, "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newReq(nil)
			if tc.set {
				r.SetBasicAuth(tc.user, tc.pass)
			}
			if got := (Basic{Username: tc.cfgUser, Password: tc.cfgPass}).Authenticate(r, nil, testNow); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHMAC(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	good := sign(hmacSecret, string(body))
	ts := func(d time.Duration) string { return strconv.FormatInt(testNow.Add(d).Unix(), 10) }
	hub := func(sig string) map[string]string { return map[string]string{DefaultHMACHeader: sig} }
	grafana := func(d time.Duration) map[string]string {
		return map[string]string{grafanaSig: sign(hmacSecret, ts(d), ":", string(body)), grafanaTime: ts(d)}
	}
	defaults := NewHMAC([]byte(hmacSecret), "", "", "", 0)
	grafanaAuth := NewHMAC([]byte(hmacSecret), grafanaSig, "", grafanaTime, 0)
	custom := NewHMAC([]byte(hmacSecret), sigHeader, "", "", 0)

	tests := []struct {
		name string
		auth HMAC
		hdrs map[string]string
		body []byte
		want bool
	}{
		{"default header with sha256= prefix", defaults, hub("sha256=" + good), nil, true},
		{"default header uppercase hex", defaults, hub("sha256=" + strings.ToUpper(good)), nil, true},
		{"default header missing prefix", defaults, hub(good), nil, false},
		{
			"explicit default header name defaults prefix too",
			NewHMAC([]byte(hmacSecret), "x-hub-signature-256", "", "", 0), hub("sha256=" + good), nil, true,
		},
		{"custom header no prefix", custom, map[string]string{sigHeader: good}, nil, true},
		{"custom header rejects sha256= when no prefix configured", custom, map[string]string{sigHeader: "sha256=" + good}, nil, false},
		{
			"custom header with custom prefix",
			NewHMAC([]byte(hmacSecret), sigHeader, "v1=", "", 0), map[string]string{sigHeader: "v1=" + good}, nil, true,
		},
		{"wrong secret", defaults, hub("sha256=" + sign("other", string(body))), nil, false},
		{"tampered body", defaults, hub("sha256=" + good), []byte(`{"id":"2"}`), false},
		{"missing header", defaults, nil, nil, false},
		{"non-hex signature", defaults, hub("sha256=zzzz"), nil, false},
		{"truncated signature", defaults, hub("sha256=" + good[:32]), nil, false},
		{"empty secret", NewHMAC(nil, "", "", "", 0), hub("sha256=" + sign("", string(body))), nil, false},
		{"grafana style within skew", grafanaAuth, grafana(-time.Minute), nil, true},
		{"grafana style at skew boundary", grafanaAuth, grafana(-DefaultMaxSkew), nil, true},
		{"grafana style stale timestamp", grafanaAuth, grafana(-6 * time.Minute), nil, false},
		{"grafana style future timestamp outside skew", grafanaAuth, grafana(10 * time.Minute), nil, false},
		{
			"custom maxSkew widens window",
			NewHMAC([]byte(hmacSecret), sigHeader, "", tsHeader, time.Hour),
			map[string]string{sigHeader: sign(hmacSecret, ts(-30*time.Minute), ":", string(body)), tsHeader: ts(-30 * time.Minute)},
			nil, true,
		},
		{
			"timestamp not covered by signature",
			NewHMAC([]byte(hmacSecret), sigHeader, "", tsHeader, 0),
			map[string]string{sigHeader: good, tsHeader: ts(0)}, nil, false,
		},
		{
			"timestamp header missing",
			NewHMAC([]byte(hmacSecret), sigHeader, "", tsHeader, 0),
			map[string]string{sigHeader: sign(hmacSecret, ":", string(body))}, nil, false,
		},
		{
			"timestamp header not a number",
			NewHMAC([]byte(hmacSecret), sigHeader, "", tsHeader, 0),
			map[string]string{sigHeader: sign(hmacSecret, "soon:", string(body)), tsHeader: "soon"}, nil, false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := body
			if tc.body != nil {
				b = tc.body
			}
			if got := tc.auth.Authenticate(newReq(tc.hdrs), b, testNow); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewHMACDefaults(t *testing.T) {
	tests := []struct {
		name                   string
		header, prefix         string
		wantHeader, wantPrefix string
		skew, wantSkew         time.Duration
	}{
		{"all empty", "", "", DefaultHMACHeader, DefaultHMACPrefix, 0, DefaultMaxSkew},
		{"other header", sigHeader, "", sigHeader, "", 0, DefaultMaxSkew},
		{"explicit prefix kept", sigHeader, "v0=", sigHeader, "v0=", time.Minute, time.Minute},
		{"empty header explicit prefix", "", "t=", DefaultHMACHeader, "t=", 0, DefaultMaxSkew},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHMAC([]byte("s"), tc.header, tc.prefix, tsHeader, tc.skew)
			if h.Header != tc.wantHeader || h.Prefix != tc.wantPrefix || h.MaxSkew != tc.wantSkew || h.TimestampHeader != tsHeader {
				t.Fatalf("got %+v", h)
			}
		})
	}
}
