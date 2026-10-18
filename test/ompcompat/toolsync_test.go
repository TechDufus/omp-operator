//go:build ompcompat

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

package ompcompat

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechDufus/omp-operator/internal/driver"
)

// tarNames lists the member names of a tar.gz.
func tarNames(t *testing.T, raw []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

// TestRealToolsync is plan test 5: the driver's toolsync with the real mise installs a
// workspace's tools into the mise data directory and uploads it under the toolchain hash.
// It needs mise on PATH and network access to mise's tool sources.
func TestRealToolsync(t *testing.T) {
	if _, err := exec.LookPath("mise"); err != nil {
		t.Fatalf("mise must be on PATH (mise run ompcompat provides it): %v", err)
	}
	paths := testPaths(t)
	// Keep mise away from the developer's own configuration, caches and trust database.
	root := filepath.Dir(paths.Workspace)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "xdg-cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("MISE_CACHE_DIR", filepath.Join(root, "mise-cache"))
	t.Setenv("MISE_STATE_DIR", filepath.Join(root, "mise-state"))
	t.Setenv("MISE_TRUSTED_CONFIG_PATHS", paths.Workspace)
	writeFile(t, filepath.Join(paths.Workspace, "mise.toml"), "[tools]\njq = \"1.7.1\"\n")

	api := newAgentAPI(t, driver.KindAgents)
	d := api.driver(t, driver.KindAgents, paths)
	d.Stderr = testWriter{t}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if err := d.Toolsync(ctx); err != nil {
		t.Fatalf("Toolsync: %v", err)
	}

	hash, err := driver.ToolchainHash(paths.Workspace, nil)
	if err != nil || hash == "" {
		t.Fatalf("hash = %q, %v", hash, err)
	}
	api.snapshot(func() {
		if len(api.unchanged) != 0 {
			t.Fatalf("toolsync reported unchanged: %+v", api.unchanged)
		}
		upload, ok := api.toolchainPut[hash]
		if !ok || len(api.toolchainPut) != 1 {
			t.Fatalf("uploads = %d, want exactly one under %s", len(api.toolchainPut), hash)
		}
		names := tarNames(t, upload)
		t.Logf("toolchain tarball: %d members", len(names))
		found := false
		for _, name := range names {
			if !strings.HasPrefix(name, "data/") && name != "data" {
				t.Errorf("tarball member %q is outside the data/ root", name)
			}
			found = found || strings.HasSuffix(strings.TrimSuffix(name, "/"), "installs/jq/1.7.1")
		}
		if !found {
			t.Errorf("tarball has no installs/jq/1.7.1 (members: %v)", names)
		}
	})
}

// testWriter sends a subprocess's output to the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
