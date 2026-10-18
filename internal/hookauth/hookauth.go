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

// Package hookauth verifies webhook credentials (bearer, basic, HMAC) against
// already-resolved secret material. All secret comparisons are constant-time
// and verification reports only success or failure, never which part failed.
package hookauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultHMACHeader is the signature header used when none is configured.
	DefaultHMACHeader = "X-Hub-Signature-256"
	// DefaultHMACPrefix is the signature prefix for DefaultHMACHeader.
	DefaultHMACPrefix = "sha256="
	// DefaultMaxSkew is the allowed timestamp skew when none is configured.
	DefaultMaxSkew = 5 * time.Minute
)

// Authenticator verifies one request. body is the complete request body as
// read by the caller; now is the verification time.
type Authenticator interface {
	Authenticate(r *http.Request, body []byte, now time.Time) bool
}

// equal compares two strings in constant time irrespective of length.
func equal(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// Bearer authenticates "Authorization: Bearer <token>".
type Bearer struct {
	Token string
}

// Authenticate implements Authenticator. An empty configured token never matches.
func (b Bearer) Authenticate(r *http.Request, _ []byte, _ time.Time) bool {
	auth := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(auth, " ")
	ok := found && strings.EqualFold(scheme, "Bearer")
	match := equal(strings.TrimSpace(token), b.Token)
	return ok && match && b.Token != ""
}

// Basic authenticates HTTP basic credentials.
type Basic struct {
	Username string
	Password string
}

// Authenticate implements Authenticator. An empty configured username or
// password never matches.
func (b Basic) Authenticate(r *http.Request, _ []byte, _ time.Time) bool {
	user, pass, ok := r.BasicAuth()
	userMatch := equal(user, b.Username)
	passMatch := equal(pass, b.Password)
	return ok && userMatch && passMatch && b.Username != "" && b.Password != ""
}

// HMAC authenticates a hex-encoded HMAC-SHA256 signature of the request body.
// With TimestampHeader set, the signed text is "<timestamp>:<body>" where the
// timestamp is the header's Unix-seconds value (Grafana's scheme), and the
// timestamp must be within MaxSkew of now.
type HMAC struct {
	Secret          []byte
	Header          string
	Prefix          string
	TimestampHeader string
	MaxSkew         time.Duration
}

// NewHMAC builds an HMAC authenticator from the OmpTrigger spec fields and
// applies their defaults: an empty header becomes X-Hub-Signature-256, and an
// empty prefix becomes "sha256=" for that header and "" for any other. An
// empty header with an explicit prefix keeps that prefix. A zero maxSkew
// becomes 5 minutes.
func NewHMAC(secret []byte, header, prefix, timestampHeader string, maxSkew time.Duration) HMAC {
	if header == "" {
		header = DefaultHMACHeader
	}
	if prefix == "" && http.CanonicalHeaderKey(header) == DefaultHMACHeader {
		prefix = DefaultHMACPrefix
	}
	if maxSkew <= 0 {
		maxSkew = DefaultMaxSkew
	}
	return HMAC{Secret: secret, Header: header, Prefix: prefix, TimestampHeader: timestampHeader, MaxSkew: maxSkew}
}

// Authenticate implements Authenticator. The fields are used as given; use
// NewHMAC to apply defaults. An empty secret never matches.
func (h HMAC) Authenticate(r *http.Request, body []byte, now time.Time) bool {
	if len(h.Secret) == 0 || h.Header == "" {
		return false
	}
	sigHex, hasPrefix := strings.CutPrefix(strings.TrimSpace(r.Header.Get(h.Header)), h.Prefix)
	got, err := hex.DecodeString(sigHex)
	if !hasPrefix || err != nil || len(got) != sha256.Size {
		return false
	}

	mac := hmac.New(sha256.New, h.Secret)
	if h.TimestampHeader != "" {
		raw := strings.TrimSpace(r.Header.Get(h.TimestampHeader))
		secs, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || !withinSkew(time.Unix(secs, 0), now, h.MaxSkew) {
			return false
		}
		// Sign the header value exactly as sent, not its re-formatted form.
		mac.Write([]byte(raw))
		mac.Write([]byte{':'})
	}
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), got)
}

func withinSkew(ts, now time.Time, skew time.Duration) bool {
	d := now.Sub(ts)
	if d < 0 {
		d = -d
	}
	return d <= skew
}
