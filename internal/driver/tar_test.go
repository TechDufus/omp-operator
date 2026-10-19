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
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractPathSafety(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
		wantErr string
	}{
		{"absolute path", []tarEntry{{Name: "/etc/evil", Body: "x"}}, "absolute"},
		{"dot dot", []tarEntry{{Name: "../evil", Body: "x"}}, ".."},
		{"nested dot dot", []tarEntry{{Name: "a/../../evil", Body: "x"}}, ".."},
		{"absolute symlink outside", []tarEntry{{Name: "l", Type: tar.TypeSymlink, Linkname: "/etc/passwd"}}, "outside"},
		{"relative symlink outside", []tarEntry{{Name: "d/l", Type: tar.TypeSymlink, Linkname: "../../x"}}, "outside"},
		{"hard link outside", []tarEntry{{Name: "h", Type: tar.TypeLink, Linkname: "../x"}}, ".."},
		{"hard link absolute", []tarEntry{{Name: "h", Type: tar.TypeLink, Linkname: "/etc/passwd"}}, "hard link"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "dest")
			err := ExtractTarGz(bytes.NewReader(buildTarGz(t, tc.entries...)), dest, 1<<20)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			ents, _ := os.ReadDir(parent)
			if len(ents) != 1 {
				t.Fatalf("something was created next to dest: %v", ents)
			}
		})
	}
}

func TestExtractAcceptsSafeArchive(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dest")
	raw := buildTarGz(t,
		tarEntry{Name: "./", Type: tar.TypeDir},
		tarEntry{Name: "bin/", Type: tar.TypeDir, Mode: 0o755},
		tarEntry{Name: "./bin/tool", Body: shebang, Mode: 0o755},
		tarEntry{Name: "bin/alias", Type: tar.TypeSymlink, Linkname: "tool"},
		tarEntry{Name: "bin/up", Type: tar.TypeSymlink, Linkname: "../bin/tool"},
		tarEntry{Name: "bin/abs", Type: tar.TypeSymlink, Linkname: filepath.Join(dest, "bin", "tool")},
		tarEntry{Name: "bin/hard", Type: tar.TypeLink, Linkname: "bin/tool"},
		tarEntry{Name: "dev/null", Type: tar.TypeChar},
		tarEntry{Name: "pipe", Type: tar.TypeFifo},
		tarEntry{Name: "plain.txt", Body: "plain"},
	)
	if err := ExtractTarGz(bytes.NewReader(raw), dest, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dest, "bin", "alias")); got != shebang {
		t.Fatalf("symlink target content = %q", got)
	}
	if got := readFile(t, filepath.Join(dest, "bin", "hard")); got != shebang {
		t.Fatalf("hard link content = %q", got)
	}
	st, err := os.Stat(filepath.Join(dest, "bin", "tool"))
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("tool must stay executable: %v %v", st, err)
	}
	for _, skipped := range []string{"dev/null", "pipe"} {
		if _, err := os.Lstat(filepath.Join(dest, skipped)); err == nil {
			t.Fatalf("%s must be skipped", skipped)
		}
	}
	if got := readFile(t, filepath.Join(dest, "plain.txt")); got != "plain" {
		t.Fatalf("plain.txt = %q", got)
	}
}

func TestExtractSizeLimit(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dest")
	raw := buildTarGz(t, tarEntry{Name: "big", Body: strings.Repeat("x", 100)})
	err := ExtractTarGz(bytes.NewReader(raw), dest, 50)
	if err == nil || !strings.Contains(err.Error(), "extraction limit") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractCannotWriteThroughPlantedSymlink(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	// "l" points to "." (inside), so "l/.." only escapes lexically after resolution.
	raw := buildTarGz(t,
		tarEntry{Name: "l", Type: tar.TypeSymlink, Linkname: "."},
		tarEntry{Name: "m", Type: tar.TypeSymlink, Linkname: "l/.."},
	)
	err := ExtractTarGz(bytes.NewReader(raw), dest, 1<<20)
	if err != nil {
		t.Fatalf("lexically inside links are extracted: %v", err)
	}
	raw = buildTarGz(t, tarEntry{Name: "m/escape.txt", Body: "pwned"})
	if err := ExtractTarGz(bytes.NewReader(raw), dest, 1<<20); err == nil {
		t.Fatal("writing through the planted symlink must fail")
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Fatal("file escaped the extraction directory")
	}
}

func TestWriteTarGzRoundTrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	writeFile(t, filepath.Join(src, "installs", "jq", "1.7.1", "bin", "jq"), "jq-binary")
	if err := os.Symlink("1.7.1", filepath.Join(src, "installs", "jq", "latest")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteTarGz(&buf, src, "data"); err != nil {
		t.Fatal(err)
	}
	members := tarMembers(t, buf.Bytes())
	if members["data/installs/jq/1.7.1/bin/jq"] != "jq-binary" {
		t.Fatalf("members = %v", members)
	}
	dest := filepath.Join(t.TempDir(), "mise")
	if err := ExtractTarGz(&buf, dest, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dest, "data", "installs", "jq", "latest", "bin", "jq")); got != "jq-binary" {
		t.Fatalf("through the symlink: %q", got)
	}

	// Without a prefix names are src-relative.
	buf.Reset()
	if err := WriteTarGz(&buf, src, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := tarMembers(t, buf.Bytes())["installs/jq/1.7.1/bin/jq"]; !ok {
		t.Fatal("unprefixed tarball must be src-relative")
	}
}

// mise's shims are symlinks to the mise binary outside the data directory; a toolchain
// tarball carrying them could never be extracted.
func TestPackMiseDataLeavesOutShims(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	writeFile(t, filepath.Join(data, "installs", "go", "1.27.2", "bin", "go"), "go-binary")
	if err := os.MkdirAll(filepath.Join(data, "shims"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/local/bin/mise", filepath.Join(data, "shims", "go")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := PackMiseData(&buf, data); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "mise")
	if err := ExtractTarGz(&buf, dest, 1<<20); err != nil {
		t.Fatalf("a packed toolchain must extract: %v", err)
	}
	if got := readFile(t, filepath.Join(dest, "data", "installs", "go", "1.27.2", "bin", "go")); got != "go-binary" {
		t.Fatalf("installs: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dest, "data", "shims")); err == nil {
		t.Fatal("shims must not be packed")
	}
}
