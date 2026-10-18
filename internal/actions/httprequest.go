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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

// httpRequest validates the agent's arguments against the action's schema, renders the
// URL and body, and sends one request that must stay under allowedURLPrefix.
func (r *Runner) httpRequest(ctx context.Context, c callCtx, spec *v1alpha1.HTTPRequestAction, raw json.RawMessage) outcome {
	sch, err := compileParameters(spec.Parameters.Raw)
	if err != nil {
		return failure("", "action parameters are invalid: %v", err)
	}
	args, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return refusal("", "arguments are not valid JSON: %v", err)
	}
	if err := sch.Validate(args); err != nil {
		return refusal("", "arguments rejected by the schema: %s", truncate(oneLine(err.Error()), 500))
	}

	data := map[string]any{"Args": args}
	rendered, err := renderTemplate("url", spec.URL, data)
	if err != nil {
		return failure("", "rendering url: %v", err)
	}
	target, err := CheckURL(spec.AllowedURLPrefix, rendered)
	if err != nil {
		return refusal("", "url is not allowed: %v", err)
	}
	label := string(spec.Method) + " " + redactedURL(target)
	body, err := renderTemplate("body", spec.Body, data)
	if err != nil {
		return failure(label, "rendering body: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, string(spec.Method), target.String(), strings.NewReader(body))
	if err != nil {
		return failure(label, "building request: %v", err)
	}
	for _, h := range spec.Headers {
		var v string
		switch {
		case h.Value != nil:
			v = *h.Value
		case h.SecretRef != nil:
			if v, err = r.secretValue(ctx, c.action.Namespace, *h.SecretRef); err != nil {
				return failure(label, "reading header %s: %v", h.Name, err)
			}
		}
		req.Header.Set(h.Name, v)
	}

	resp, err := r.httpClient().Do(req)
	if err != nil {
		msg := err.Error()
		if ue := new(url.Error); errors.As(err, &ue) {
			msg = ue.Err.Error()
		}
		return failure(label, "request failed: %s", msg)
	}
	defer func() { _ = resp.Body.Close() }()
	limit := responseMaxBytes(spec)
	buf, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)))
	if err != nil {
		return failure(label, "reading response: %v", err)
	}
	if len(buf) == limit {
		buf = trimPartialRune(buf)
	}
	out := okOutcome(label, "HTTP %d\n%s", resp.StatusCode, buf)
	if resp.StatusCode >= 400 {
		out.isErr = true
		out.result = "error"
	}
	return out
}

// trimPartialRune drops an incomplete UTF-8 sequence at the end of b.
func trimPartialRune(b []byte) []byte {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i]
			}
			return b
		}
	}
	return b
}

func renderTemplate(name, text string, data any) (string, error) {
	t, err := parseTemplate(name, text)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func redactedURL(u *url.URL) string {
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

// CheckURL parses rendered and returns the URL to request when it stays under prefix:
// scheme, host and port are equal to the prefix's, there is no userinfo, fragment,
// backslash or encoded slash, and the cleaned path equals the prefix path or continues it
// after a "/". The returned URL carries the cleaned path.
func CheckURL(prefix, rendered string) (*url.URL, error) {
	p, err := url.Parse(prefix)
	if err != nil {
		return nil, fmt.Errorf("allowedURLPrefix: %w", err)
	}
	if strings.ContainsAny(rendered, "\\ \t\r\n") {
		return nil, errors.New("contains whitespace or a backslash")
	}
	u, err := url.Parse(rendered)
	if err != nil {
		return nil, err
	}
	if u.Scheme != p.Scheme {
		return nil, fmt.Errorf("scheme must be %s", p.Scheme)
	}
	if u.User != nil {
		return nil, errors.New("userinfo is not allowed")
	}
	if u.Fragment != "" || strings.Contains(rendered, "#") {
		return nil, errors.New("fragments are not allowed")
	}
	if !strings.EqualFold(u.Hostname(), p.Hostname()) || effectivePort(u) != effectivePort(p) {
		return nil, fmt.Errorf("host must be %s", p.Host)
	}
	esc := strings.ToLower(u.EscapedPath())
	if strings.Contains(esc, "%2f") || strings.Contains(esc, "%5c") {
		return nil, errors.New("encoded slashes are not allowed in the path")
	}
	cleaned := path.Clean("/" + u.Path)
	base := strings.TrimRight(path.Clean("/"+p.Path), "/")
	if cleaned != base && !strings.HasPrefix(cleaned, base+"/") {
		return nil, fmt.Errorf("path must be %s or below it", orSlash(base))
	}
	if strings.HasSuffix(u.Path, "/") && cleaned != "/" {
		cleaned += "/"
	}
	out := *u
	out.Path = cleaned
	out.RawPath = ""
	return &out, nil
}

func orSlash(s string) string {
	if s == "" {
		return "/"
	}
	return s
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
