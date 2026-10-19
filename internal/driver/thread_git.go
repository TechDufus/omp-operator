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
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/prompt"
	"github.com/TechDufus/omp-operator/internal/store"
)

// EnvGitpushAddr is the address of the push sidecar, set in the agent container when the
// thread's git host has push enabled.
const EnvGitpushAddr = "OMP_GITPUSH_ADDR"

// threadRun is what the turns of one sitting share.
type threadRun struct {
	cfg    agentapi.ThreadConfig
	red    *Redactor
	events *eventSink
	// uploaded is the branch tip of the result bundle the operator holds.
	uploaded string
}

// finalized is the state of the thread branch after a turn.
type finalized struct {
	Head       string
	Files      int
	Insertions int
	Deletions  int
	Pushed     bool
	PushError  string
}

// finalizeThread commits whatever the turn left in the workspace, keeps the thread branch on
// the newest commit, measures it against the base, stores it as the thread's result bundle
// and pushes it when the thread has a push sidecar. Problems become events: the turn's reply
// is worth more than a perfect branch.
func (d *Driver) finalizeThread(ctx context.Context, tr *threadRun, turnID int, reply string) finalized {
	ws, branch := d.Paths.Workspace, tr.cfg.Branch
	var out finalized
	fail := func(what string, err error) {
		tr.events.add(eventGit, what+": "+err.Error())
	}

	if err := d.git(ctx, ws, nil, "add", "-A"); err != nil {
		fail("git add", err)
	} else if code, err := d.gitExit(ctx, ws, "diff", "--cached", "--quiet"); err != nil {
		fail("git diff", err)
	} else if code == 1 {
		msg := prompt.TitleFromReply(tr.red.String(reply))
		if msg == "" {
			msg = fmt.Sprintf("omp-cloud: turn %d", turnID)
		}
		if err := d.git(ctx, ws, nil, "-c", "core.hooksPath=/dev/null", "commit", "-q", "--no-verify", "-m", msg); err != nil {
			fail("git commit", err)
		}
	}

	switch code, err := d.gitExit(ctx, ws, "merge-base", "--is-ancestor", "refs/heads/"+branch, "HEAD"); {
	case err != nil:
		fail("git merge-base", err)
	case code == 0:
		if err := d.git(ctx, ws, nil, "update-ref", "refs/heads/"+branch, "HEAD"); err != nil {
			fail("git update-ref", err)
		}
	default:
		tr.events.add(eventGit, fmt.Sprintf("HEAD is not on %s; the result stays at %s", branch, branch))
	}

	head, err := d.gitOutput(ctx, ws, nil, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		fail("git rev-parse", err)
		return out
	}
	out.Head = head
	base := tr.cfg.BaseCommit
	if base == "" {
		base = emptyTree
	}
	if stat, err := d.gitOutput(ctx, ws, nil, "diff", "--shortstat", base+".."+branch); err != nil {
		fail("git diff", err)
	} else {
		out.Files, out.Insertions, out.Deletions = parseShortstat(stat)
	}

	if head != tr.uploaded && head != tr.cfg.BaseCommit {
		if err := d.uploadResult(ctx, tr, head); err != nil {
			fail("result not stored", err)
		} else {
			tr.uploaded = head
		}
	}
	if d.PushAddr != "" {
		out.Pushed, out.PushError = d.push(ctx, head)
		out.PushError = truncate(tr.red.String(out.PushError), maxEventError)
	}
	return out
}

// uploadResult bundles the thread branch (without the base's history) and stores it on the operator.
func (d *Driver) uploadResult(ctx context.Context, tr *threadRun, head string) error {
	tmp, err := os.MkdirTemp("", "result-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	file := filepath.Join(tmp, "result.bundle")
	args := []string{"bundle", "create", file, "refs/heads/" + tr.cfg.Branch}
	if tr.cfg.BaseCommit != "" {
		args = append(args, "^"+tr.cfg.BaseCommit)
	}
	if err := d.git(ctx, d.Paths.Workspace, nil, args...); err != nil {
		return err
	}
	st, err := os.Stat(file)
	if err != nil {
		return err
	}
	if st.Size() > store.MaxResultBytes {
		return fmt.Errorf("the result bundle of %s is %d bytes, over the %d byte limit", head, st.Size(), store.MaxResultBytes)
	}
	uctx, cancel := child(ctx, tarballTimeout)
	defer cancel()
	return d.API.PutBundle(uctx, "/thread/result", file)
}

// push asks the push sidecar to publish head. The sidecar owns the credentials, the remote and
// the branch name; the agent container only names a commit.
func (d *Driver) push(ctx context.Context, head string) (pushed bool, pushError string) {
	body, _ := json.Marshal(map[string]string{"head": head})
	pctx, cancel := child(ctx, gitTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodPost, "http://"+d.PushAddr+"/push", bytes.NewReader(body))
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "push sidecar: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusOK {
		return true, ""
	}
	var answer struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &answer) == nil && answer.Error != "" {
		return false, answer.Error
	}
	return false, fmt.Sprintf("push sidecar: HTTP %d", resp.StatusCode)
}

// gitExit runs git and returns its exit code; err is set only when git could not run at all.
func (d *Driver) gitExit(ctx context.Context, dir string, args ...string) (int, error) {
	gctx, cancel := child(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(gctx, d.GitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	err := cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode(), nil
	}
	return 0, err
}

var shortstatRE = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?`)

// parseShortstat reads files changed, insertions and deletions from `git diff --shortstat`.
func parseShortstat(s string) (files, insertions, deletions int) {
	m := shortstatRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0
	}
	num := func(v string) int {
		n, _ := strconv.Atoi(v)
		return n
	}
	return num(m[1]), num(m[2]), num(m[3])
}

// resultTip is the tip of the stored result prepare fetched, empty when the thread has none.
func (d *Driver) resultTip(ctx context.Context) string {
	tip, err := d.gitOutput(ctx, d.Paths.Workspace, nil, "rev-parse", "--verify", "-q", refResult+"^{commit}")
	if err != nil {
		return ""
	}
	return tip
}

// subUsage is after − before, the usage of one turn out of the session's running totals.
func subUsage(after, before agentapi.Usage) agentapi.Usage {
	cost := func(v string) float64 {
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return agentapi.Usage{
		InputTokens:      max(after.InputTokens-before.InputTokens, 0),
		OutputTokens:     max(after.OutputTokens-before.OutputTokens, 0),
		CacheReadTokens:  max(after.CacheReadTokens-before.CacheReadTokens, 0),
		CacheWriteTokens: max(after.CacheWriteTokens-before.CacheWriteTokens, 0),
		// Rounded to 1e-8 dollars: the difference of two sums must not print float noise.
		Cost: strconv.FormatFloat(math.Round(max(cost(after.Cost)-cost(before.Cost), 0)*1e8)/1e8, 'f', -1, 64),
	}
}
