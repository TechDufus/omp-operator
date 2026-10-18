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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// stubMise installs a fake `mise` that records its arguments and working directory and
// "installs" jq into $MISE_DATA_DIR. It returns the stub and its log file.
func stubMise(t *testing.T) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "mise"), filepath.Join(dir, "mise.log")
	script := "#!/bin/sh\necho \"$* in $(pwd)\" >> \"" + log + "\"\n" +
		"mkdir -p \"$MISE_DATA_DIR/installs/jq/1.7.1/bin\"\n" +
		"echo jq-binary > \"$MISE_DATA_DIR/installs/jq/1.7.1/bin/jq\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func toolsyncDriver(t *testing.T, withToml bool) (*fakeAPI, *Driver, Paths, string) {
	t.Helper()
	api := newFakeAPI(t, KindAgents)
	api.plan = agentapi.Plan{Toolchain: &agentapi.ToolchainPlan{Disable: []string{toolGitleaks}}}
	paths := testPaths(t)
	if err := os.MkdirAll(paths.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if withToml {
		writeFile(t, filepath.Join(paths.Workspace, "mise.toml"), "[tools]\njq = \"1.7.1\"\n")
	}
	d := api.driver(paths)
	bin, log := stubMise(t)
	d.MiseBin = bin
	return api, d, paths, log
}

func TestToolsyncNoMiseToml(t *testing.T) {
	api, d, _, log := toolsyncDriver(t, false)
	api.storedHash = "something"
	if err := d.Toolsync(context.Background()); err != nil {
		t.Fatal(err)
	}
	unlock := api.lock()
	defer unlock()
	if len(api.unchanged) != 1 || api.unchanged[0].Hash != "" {
		t.Fatalf("unchanged = %+v, want one report with an empty hash", api.unchanged)
	}
	if len(api.toolchainPut) != 0 {
		t.Fatal("nothing to upload")
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("mise must not run without a mise.toml")
	}
}

func TestToolsyncSameHashIsUnchanged(t *testing.T) {
	api, d, paths, log := toolsyncDriver(t, true)
	hash, err := ToolchainHash(paths.Workspace, []string{toolGitleaks})
	if err != nil || hash == "" {
		t.Fatalf("hash = %q, %v", hash, err)
	}
	api.storedHash = hash
	if err := d.Toolsync(context.Background()); err != nil {
		t.Fatal(err)
	}
	unlock := api.lock()
	defer unlock()
	if len(api.unchanged) != 1 || api.unchanged[0].Hash != hash || len(api.toolchainPut) != 0 {
		t.Fatalf("unchanged = %+v puts = %d", api.unchanged, len(api.toolchainPut))
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("mise must not run when the stored toolchain is current")
	}
}

func TestToolsyncChangedHashUploads(t *testing.T) {
	api, d, paths, log := toolsyncDriver(t, true)
	api.storedHash = "old-hash"
	if err := d.Toolsync(context.Background()); err != nil {
		t.Fatal(err)
	}
	hash, _ := ToolchainHash(paths.Workspace, []string{toolGitleaks})

	if got := strings.TrimSpace(readFile(t, log)); got != "install --yes in "+resolve(t, paths.Workspace) {
		t.Fatalf("mise invocation = %q", got)
	}
	unlock := api.lock()
	defer unlock()
	if len(api.unchanged) != 0 {
		t.Fatalf("unchanged must not be reported: %+v", api.unchanged)
	}
	upload, ok := api.toolchainPut[hash]
	if !ok || len(api.toolchainPut) != 1 {
		t.Fatalf("uploads = %v, want exactly one under %s", keys(api.toolchainPut), hash)
	}
	members := tarMembers(t, upload)
	if members["data/installs/jq/1.7.1/bin/jq"] != "jq-binary\n" {
		t.Fatalf("tarball members = %v", keys(members))
	}
}

func TestToolsyncMiseFailureUploadsNothing(t *testing.T) {
	api, d, _, _ := toolsyncDriver(t, true)
	api.storedHash = "old"
	fail := filepath.Join(t.TempDir(), "mise")
	if err := os.WriteFile(fail, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d.MiseBin = fail
	if err := d.Toolsync(context.Background()); err == nil || !strings.Contains(err.Error(), "mise install") {
		t.Fatalf("err = %v", err)
	}
	unlock := api.lock()
	defer unlock()
	if len(api.toolchainPut) != 0 || len(api.unchanged) != 0 {
		t.Fatal("a failed install must report nothing; the pod failure is the signal")
	}
}

func TestToolsyncNeedsAgentIdentity(t *testing.T) {
	api := newFakeAPI(t, KindSessions)
	if err := api.driver(testPaths(t)).Toolsync(context.Background()); err == nil {
		t.Fatal("toolsync with a session identity must fail")
	}
}

func resolve(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
