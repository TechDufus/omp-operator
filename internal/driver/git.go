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
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// Git auth types in agentapi.GitSource.AuthType.
const (
	authSSH   = "ssh"
	authToken = "token"

	defaultGitUser = "x-access-token"
	gitTimeout     = 10 * time.Minute
)

// gitClone clones src into dest. Credentials only ever travel in the environment of the
// git process, never in its argument list.
func (d *Driver) gitClone(ctx context.Context, src agentapi.GitSource, dest string) error {
	depth := src.Depth
	if depth <= 0 {
		depth = 1
	}
	args := []string{"clone", "--depth", strconv.Itoa(depth)}
	if src.Ref != "" {
		args = append(args, "--branch", src.Ref)
	}
	args = append(args, "--", src.URL, dest)

	env, cleanup, err := gitEnv(src)
	if err != nil {
		return err
	}
	defer cleanup()
	return d.git(ctx, "", env, args...)
}

// git runs git with extra environment variables. dir may be empty.
func (d *Driver) git(ctx context.Context, dir string, env []string, args ...string) error {
	_, err := d.gitOutput(ctx, dir, env, args...)
	return err
}

func (d *Driver) gitOutput(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	gctx, cancel := child(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(gctx, d.GitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// trustWorkspace lists the workspace as a safe git directory in $HOME/.gitconfig, for the
// driver, omp and every command omp runs. The kubelet creates the workspace volume owned by
// root, and git refuses to work in a repository owned by another user ("dubious ownership"),
// which the agent (uid 10001) would otherwise hit on its first git command.
func (d *Driver) trustWorkspace() error {
	if err := os.MkdirAll(d.Paths.Home, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d.Paths.Home, ".gitconfig"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "[safe]\n\tdirectory = %s\n", d.Paths.Workspace)
	return errors.Join(err, f.Close())
}

// gitEnv returns the environment variables that authenticate src, and a cleanup for
// the temporary files they refer to.
func gitEnv(src agentapi.GitSource) (env []string, cleanup func(), err error) {
	cleanup = func() {}
	switch src.AuthType {
	case "":
		return nil, cleanup, nil
	case authSSH:
		tmp, err := os.MkdirTemp("", "git-ssh-")
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { _ = os.RemoveAll(tmp) }
		// ssh refuses keys readable by group or others, and Secret volumes are group
		// readable under fsGroup: use a private copy.
		key, err := os.ReadFile(filepath.Join(src.AuthDir, agentapi.AuthSSHKeyFile))
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("read ssh key: %w", err)
		}
		if len(key) > 0 && key[len(key)-1] != '\n' {
			key = append(key, '\n')
		}
		keyFile := filepath.Join(tmp, "id")
		if err := os.WriteFile(keyFile, key, 0o600); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		knownHosts := filepath.Join(src.AuthDir, agentapi.AuthKnownHostsFile)
		sshCmd := fmt.Sprintf("ssh -i %s -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes",
			shellQuote(keyFile), shellQuote(knownHosts))
		return []string{"GIT_SSH_COMMAND=" + sshCmd}, cleanup, nil
	case authToken:
		raw, err := os.ReadFile(filepath.Join(src.AuthDir, agentapi.AuthTokenFile))
		if err != nil {
			return nil, cleanup, fmt.Errorf("read git token: %w", err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return nil, cleanup, errors.New("git token is empty")
		}
		user := src.Username
		if user == "" {
			user = defaultGitUser
		}
		header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
		return []string{
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=" + header,
		}, cleanup, nil
	default:
		return nil, cleanup, fmt.Errorf("unknown git auth type %q", src.AuthType)
	}
}

// shellQuote single-quotes s for the shell that runs GIT_SSH_COMMAND.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
