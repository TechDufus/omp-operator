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

package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// Target kinds: which family of agent API routes a driver talks to.
const (
	KindSessions = "sessions"
	KindAgents   = "agents"
)

// Target names the object a driver acts for: /v1/<Kind>/<Namespace>/<Name>/...
type Target struct {
	Kind      string
	Namespace string
	Name      string
}

// StatusError is a non-2xx answer of the agent API.
type StatusError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

// Client is the HTTP client of the agent API.
type Client struct {
	BaseURL string
	Target  Target
	// Token is the bearer token; when empty it is read once from TokenFile.
	Token     string
	TokenFile string
	HTTP      *http.Client
	// Attempts and RetryDelay govern the retries of idempotent calls on network errors,
	// HTTP 429 and HTTP 5xx.
	Attempts   int
	RetryDelay time.Duration

	tokenOnce sync.Once
	tokenErr  error
}

// NewClient returns a Client for target with the default retry policy.
func NewClient(baseURL, tokenFile string, target Target) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Target:     target,
		TokenFile:  tokenFile,
		HTTP:       &http.Client{},
		Attempts:   3,
		RetryDelay: 2 * time.Second,
	}
}

func (c *Client) path(suffix string) string {
	return "/v1/" + c.Target.Kind + "/" + url.PathEscape(c.Target.Namespace) + "/" +
		url.PathEscape(c.Target.Name) + suffix
}

func (c *Client) bearer() (string, error) {
	c.tokenOnce.Do(func() {
		if c.Token != "" {
			return
		}
		raw, err := os.ReadFile(c.TokenFile)
		if err != nil {
			c.tokenErr = fmt.Errorf("read API token: %w", err)
			return
		}
		c.Token = strings.TrimSpace(string(raw))
	})
	return c.Token, c.tokenErr
}

// bodyFunc yields a fresh request body per attempt.
type bodyFunc func() (io.ReadCloser, int64, error)

func bytesBody(b []byte) bodyFunc {
	return func() (io.ReadCloser, int64, error) {
		return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
	}
}

func fileBody(path string) bodyFunc {
	return func() (io.ReadCloser, int64, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, err
		}
		return f, st.Size(), nil
	}
}

func transient(err error) bool {
	if se, ok := errors.AsType[*StatusError](err); ok {
		return se.Status == http.StatusTooManyRequests || se.Status >= 500
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// do performs one request. A 2xx response is returned open; anything else is a *StatusError.
// retry enables the retry policy, which must only be used for idempotent requests.
func (c *Client) do(ctx context.Context, method, path, contentType string, body bodyFunc, retry bool) (*http.Response, error) {
	token, err := c.bearer()
	if err != nil {
		return nil, err
	}
	attempts := 1
	if retry && c.Attempts > 1 {
		attempts = c.Attempts
	}
	for attempt := 1; ; attempt++ {
		resp, err := c.once(ctx, method, path, contentType, token, body)
		if err == nil || attempt >= attempts || !transient(err) {
			return resp, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(c.RetryDelay):
		}
	}
}

func (c *Client) once(ctx context.Context, method, path, contentType, token string, body bodyFunc) (*http.Response, error) {
	var rc io.ReadCloser
	var size int64
	if body != nil {
		var err error
		if rc, size, err = body(); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rc)
	if err != nil {
		if rc != nil {
			_ = rc.Close()
		}
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &StatusError{Method: method, Path: path, Status: resp.StatusCode, Body: string(msg)}
	}
	return resp, nil
}

// doJSON sends in (when non-nil) as JSON and decodes the response into out (when non-nil).
func (c *Client) doJSON(ctx context.Context, method, path string, in, out any, retry bool) error {
	var body bodyFunc
	contentType := ""
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, contentType = bytesBody(raw), "application/json"
	}
	resp, err := c.do(ctx, method, path, contentType, body, retry)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// Plan fetches what prepare needs.
func (c *Client) Plan(ctx context.Context) (agentapi.Plan, error) {
	var plan agentapi.Plan
	err := c.doJSON(ctx, http.MethodGet, c.path("/plan"), nil, &plan, true)
	return plan, err
}

// Config fetches the session config.
func (c *Client) Config(ctx context.Context) (agentapi.SessionConfig, error) {
	var cfg agentapi.SessionConfig
	err := c.doJSON(ctx, http.MethodGet, c.path(""), nil, &cfg, true)
	return cfg, err
}

// Prepared reports the outcome of prepare.
func (c *Client) Prepared(ctx context.Context, p agentapi.Prepared) error {
	return c.doJSON(ctx, http.MethodPost, c.path("/prepared"), p, nil, true)
}

// Action runs one host tool through the operator. It is never retried: calls count against maxPerSession.
func (c *Client) Action(ctx context.Context, tool string, args json.RawMessage) (agentapi.ActionResult, error) {
	var res agentapi.ActionResult
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	resp, err := c.do(ctx, http.MethodPost, c.path("/actions/"+url.PathEscape(tool)), "application/json", bytesBody(args), false)
	if err != nil {
		return res, err
	}
	defer func() { _ = resp.Body.Close() }()
	err = json.NewDecoder(resp.Body).Decode(&res)
	return res, err
}

// Events posts progress lines.
func (c *Client) Events(ctx context.Context, events []agentapi.Event) error {
	return c.doJSON(ctx, http.MethodPost, c.path("/events"), agentapi.Events{Events: events}, nil, false)
}

// Complete reports how the run ended.
func (c *Client) Complete(ctx context.Context, done agentapi.Complete) error {
	return c.doJSON(ctx, http.MethodPost, c.path("/complete"), done, nil, true)
}

// PutTranscript uploads the (already redacted) transcript.
func (c *Client) PutTranscript(ctx context.Context, html []byte) error {
	resp, err := c.do(ctx, http.MethodPut, c.path("/transcript"), "text/html; charset=utf-8", bytesBody(html), true)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Tarball downloads GET <path>; found is false on HTTP 404. The caller closes the body.
func (c *Client) Tarball(ctx context.Context, suffix string) (body io.ReadCloser, found bool, err error) {
	resp, err := c.do(ctx, http.MethodGet, c.path(suffix), "", nil, true)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return resp.Body, true, nil
}

// PutFile uploads the file at path as a gzip tarball with PUT <suffix>.
func (c *Client) PutFile(ctx context.Context, suffix, file string) error {
	resp, err := c.do(ctx, http.MethodPut, c.path(suffix), "application/gzip", fileBody(file), true)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// StoredToolchainHash is GET /v1/agents/{ns}/{name}/toolchain.
func (c *Client) StoredToolchainHash(ctx context.Context) (string, error) {
	var h agentapi.ToolchainHash
	err := c.doJSON(ctx, http.MethodGet, c.path("/toolchain"), nil, &h, true)
	return h.Hash, err
}

// ToolchainUnchanged is POST /v1/agents/{ns}/{name}/toolchain/unchanged.
func (c *Client) ToolchainUnchanged(ctx context.Context, hash string) error {
	return c.doJSON(ctx, http.MethodPost, c.path("/toolchain/unchanged"), agentapi.ToolchainHash{Hash: hash}, nil, true)
}

// PutBundle uploads the git bundle at file with PUT <suffix>.
func (c *Client) PutBundle(ctx context.Context, suffix, file string) error {
	resp, err := c.do(ctx, http.MethodPut, c.path(suffix), "application/x-git-bundle", fileBody(file), true)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// NextTurn is GET thread/next: it long-polls up to wait (rounded up to whole seconds, 1 to 50)
// and returns the turn assigned to this sitting, or nil when there is none. A turn that was
// delivered but not completed is delivered again, so the call may be retried.
func (c *Client) NextTurn(ctx context.Context, wait time.Duration) (*agentapi.TurnAssignment, error) {
	seconds := min(max(int((wait+time.Second-1)/time.Second), 1), 50)
	resp, err := c.do(ctx, http.MethodGet, c.path("/thread/next?wait="+strconv.Itoa(seconds)), "", nil, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	var turn agentapi.TurnAssignment
	if err := json.NewDecoder(resp.Body).Decode(&turn); err != nil {
		return nil, fmt.Errorf("GET thread/next: decode response: %w", err)
	}
	return &turn, nil
}

// ThreadStop is GET thread/control: whether the running turn was asked to stop.
func (c *Client) ThreadStop(ctx context.Context) (bool, error) {
	var control struct {
		Stop bool `json:"stop"`
	}
	err := c.doJSON(ctx, http.MethodGet, c.path("/thread/control"), nil, &control, false)
	return control.Stop, err
}

// CompleteTurn is POST thread/turns/{id}/complete.
func (c *Client) CompleteTurn(ctx context.Context, id int, done agentapi.TurnComplete) error {
	return c.doJSON(ctx, http.MethodPost, c.path("/thread/turns/"+strconv.Itoa(id)+"/complete"), done, nil, true)
}

func askPath(turn int) string {
	return "/thread/turns/" + strconv.Itoa(turn) + "/asks"
}

// PostAsk is POST thread/turns/{id}/asks: it files the questions of one ask call. It is not
// retried: a repeated call would file the questions twice.
func (c *Client) PostAsk(ctx context.Context, turn int, req agentapi.AskRequest) (agentapi.AskState, error) {
	var st agentapi.AskState
	err := c.doJSON(ctx, http.MethodPost, c.path(askPath(turn)), req, &st, false)
	return st, err
}

// WaitAsk is GET thread/turns/{id}/asks/{ask}: it long-polls up to wait (rounded up to whole
// seconds, 1 to 50) and returns the answered state, or done=false when nothing changed.
func (c *Client) WaitAsk(ctx context.Context, turn, ask int, wait time.Duration) (st agentapi.AskState, done bool, err error) {
	seconds := min(max(int((wait+time.Second-1)/time.Second), 1), 50)
	path := c.path(askPath(turn) + "/" + strconv.Itoa(ask) + "?wait=" + strconv.Itoa(seconds))
	resp, err := c.do(ctx, http.MethodGet, path, "", nil, true)
	if err != nil {
		return st, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return st, false, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return st, false, fmt.Errorf("GET %s: decode response: %w", path, err)
	}
	return st, true, nil
}

// ExpireAsk is POST thread/turns/{id}/asks/{ask}/expire: the open questions of the ask get
// answer as their answer. It returns the final state; repeating it is harmless.
func (c *Client) ExpireAsk(ctx context.Context, turn, ask int, answer string) (agentapi.AskState, error) {
	var st agentapi.AskState
	path := c.path(askPath(turn) + "/" + strconv.Itoa(ask) + "/expire")
	err := c.doJSON(ctx, http.MethodPost, path, agentapi.AskExpire{Answer: answer}, &st, true)
	return st, err
}
