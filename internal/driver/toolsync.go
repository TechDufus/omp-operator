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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	miseTOML = "mise.toml"
	miseLock = "mise.lock"

	// miseTimeout bounds `mise install`; the pod's activeDeadlineSeconds is 900.
	miseTimeout = 14 * time.Minute
)

// ToolchainHash identifies the toolchain a workspace asks for: the sha256 of mise.toml,
// "\n", mise.lock (empty when absent), "\n", and the sorted, comma-joined disable list.
// It is empty when the workspace has no mise.toml.
func ToolchainHash(workspace string, disable []string) (string, error) {
	toml, err := os.ReadFile(filepath.Join(workspace, miseTOML))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	lock, err := os.ReadFile(filepath.Join(workspace, miseLock))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	sorted := slices.Clone(disable)
	slices.Sort(sorted)
	h := sha256.New()
	h.Write(toml)
	h.Write([]byte("\n"))
	h.Write(lock)
	h.Write([]byte("\n"))
	h.Write([]byte(strings.Join(sorted, ",")))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Toolsync is the main command of a toolsync pod. prepare has already cloned the workspace.
// It reports "unchanged" when the workspace has no mise.toml or the stored toolchain
// already has the workspace's hash; otherwise it installs the tools with mise and uploads
// the mise data directory under the new hash.
func (d *Driver) Toolsync(ctx context.Context) error {
	if d.API.Target.Kind != KindAgents {
		return errors.New("toolsync needs an agent identity")
	}
	cctx, cancel := child(ctx, controlTimeout)
	plan, err := d.API.Plan(cctx)
	cancel()
	if err != nil {
		return fmt.Errorf("fetch plan: %w", err)
	}
	var disable []string
	if plan.Toolchain != nil {
		disable = plan.Toolchain.Disable
	}
	hash, err := ToolchainHash(d.Paths.Workspace, disable)
	if err != nil {
		return fmt.Errorf("hash toolchain: %w", err)
	}
	if hash == "" {
		d.Log.Info("workspace has no mise.toml; nothing to sync")
		return d.unchanged(ctx, "")
	}
	cctx, cancel = child(ctx, controlTimeout)
	stored, err := d.API.StoredToolchainHash(cctx)
	cancel()
	if err != nil {
		return fmt.Errorf("fetch stored toolchain hash: %w", err)
	}
	if stored == hash {
		d.Log.Info("stored toolchain is current", "hash", hash)
		return d.unchanged(ctx, hash)
	}

	d.Log.Info("installing toolchain", "hash", hash)
	if err := d.miseInstall(ctx, miseTimeout); err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "toolchain-*.tar.gz")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := WriteTarGz(tmp, d.Paths.MiseDataDir, filepath.Base(d.Paths.MiseDataDir)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pack %s: %w", d.Paths.MiseDataDir, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	uctx, cancel := child(ctx, tarballTimeout)
	defer cancel()
	if err := d.API.PutFile(uctx, "/toolchain/"+hash, tmp.Name()); err != nil {
		return fmt.Errorf("upload toolchain: %w", err)
	}
	d.Log.Info("toolchain uploaded", "hash", hash)
	return nil
}

func (d *Driver) unchanged(ctx context.Context, hash string) error {
	cctx, cancel := child(ctx, controlTimeout)
	defer cancel()
	if err := d.API.ToolchainUnchanged(cctx, hash); err != nil {
		return fmt.Errorf("report unchanged toolchain: %w", err)
	}
	return nil
}

// miseInstall runs `mise install` in the workspace; env is added to the process environment.
func (d *Driver) miseInstall(ctx context.Context, timeout time.Duration, env ...string) error {
	if err := os.MkdirAll(d.Paths.MiseDataDir, 0o755); err != nil {
		return err
	}
	ictx, cancel := child(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ictx, d.MiseBin, "install", "--yes")
	cmd.Dir = d.Paths.Workspace
	cmd.Env = append(os.Environ(),
		"HOME="+d.Paths.Home,
		"MISE_DATA_DIR="+d.Paths.MiseDataDir,
		"MISE_YES=1",
	)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout, cmd.Stderr = d.Stderr, d.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mise install: %w", err)
	}
	return nil
}
