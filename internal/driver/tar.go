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
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ExtractTarGz unpacks a gzip tarball into dest (created when missing). It refuses
// absolute and ".." member names and links that point outside dest, skips device,
// FIFO and other special files, and fails once more than maxBytes of file content
// would be written. Writes go through os.Root, so even a symlink the archive itself
// planted cannot redirect a later write out of dest.
func ExtractTarGz(r io.Reader, dest string, maxBytes int64) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()
	return extractTar(gz, dest, maxBytes)
}

func extractTar(r io.Reader, dest string, maxBytes int64) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(absDest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	tr := tar.NewReader(r)
	var written int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name, err := memberName(h.Name)
		if err != nil {
			return err
		}
		if name == "" { // the archive root
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = extractDir(root, name, h)
		case tar.TypeReg:
			var n int64
			n, err = extractFile(root, name, h, tr, maxBytes-written)
			written += n
		case tar.TypeSymlink:
			err = extractSymlink(root, absDest, name, h)
		case tar.TypeLink:
			err = extractHardlink(root, name, h)
		default:
			// Devices, FIFOs, PAX/GNU extension headers and anything else are skipped.
		}
		if err != nil {
			return fmt.Errorf("extract %q: %w", h.Name, err)
		}
	}
}

// memberName validates a tar member name and returns it cleaned and relative;
// the archive root ("." or "./") yields "".
func memberName(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("refusing member name %q: NUL byte", raw)
	}
	if strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) {
		return "", fmt.Errorf("refusing absolute member name %q", raw)
	}
	for part := range strings.SplitSeq(filepath.ToSlash(raw), "/") {
		if part == ".." {
			return "", fmt.Errorf("refusing member name %q: it has a \"..\" element", raw)
		}
	}
	clean := path.Clean(filepath.ToSlash(raw))
	if clean == "." {
		return "", nil
	}
	return clean, nil
}

func extractDir(root *os.Root, name string, h *tar.Header) error {
	if st, err := root.Lstat(name); err == nil && !st.IsDir() {
		if err := root.RemoveAll(name); err != nil {
			return err
		}
	}
	if err := root.MkdirAll(name, 0o755); err != nil {
		return err
	}
	return root.Chmod(name, h.FileInfo().Mode().Perm()|0o700)
}

func prepareParent(root *os.Root, name string) error {
	if dir := path.Dir(name); dir != "." {
		return root.MkdirAll(dir, 0o755)
	}
	return nil
}

func replaceable(root *os.Root, name string) error {
	if _, err := root.Lstat(name); err == nil {
		return root.RemoveAll(name)
	}
	return nil
}

func extractFile(root *os.Root, name string, h *tar.Header, r io.Reader, budget int64) (int64, error) {
	if h.Size > budget {
		return 0, fmt.Errorf("archive exceeds the %d byte extraction limit", budget)
	}
	if err := prepareParent(root, name); err != nil {
		return 0, err
	}
	if err := replaceable(root, name); err != nil {
		return 0, err
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, h.FileInfo().Mode().Perm()|0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, budget+1))
	if err == nil && n > budget {
		err = fmt.Errorf("archive exceeds the %d byte extraction limit", budget)
	}
	return n, errors.Join(err, f.Close())
}

// linkInside reports whether a symlink at linkName (relative to absDest) pointing at target stays inside absDest.
func linkInside(absDest, linkName, target string) bool {
	var resolved string
	if filepath.IsAbs(target) {
		resolved = filepath.Clean(target)
	} else {
		resolved = filepath.Join(absDest, filepath.FromSlash(path.Dir(linkName)), filepath.FromSlash(target))
	}
	return resolved == absDest || strings.HasPrefix(resolved, absDest+string(os.PathSeparator))
}

func extractSymlink(root *os.Root, absDest, name string, h *tar.Header) error {
	if h.Linkname == "" || strings.ContainsRune(h.Linkname, 0) {
		return fmt.Errorf("refusing symlink with target %q", h.Linkname)
	}
	if !linkInside(absDest, name, h.Linkname) {
		return fmt.Errorf("refusing symlink to %q: points outside the extraction directory", h.Linkname)
	}
	if err := prepareParent(root, name); err != nil {
		return err
	}
	if err := replaceable(root, name); err != nil {
		return err
	}
	return root.Symlink(h.Linkname, name)
}

func extractHardlink(root *os.Root, name string, h *tar.Header) error {
	target, err := memberName(h.Linkname)
	if err != nil || target == "" {
		return fmt.Errorf("refusing hard link to %q", h.Linkname)
	}
	if err := prepareParent(root, name); err != nil {
		return err
	}
	if err := replaceable(root, name); err != nil {
		return err
	}
	return root.Link(target, name)
}

// WriteTarGz writes src as a gzip tarball. Member names are src-relative; a non-empty
// prefix is prepended (and gets a directory entry of its own). Regular files, directories
// and symlinks are archived; everything else is skipped. Files that vanish mid-walk are ignored.
func WriteTarGz(w io.Writer, src, prefix string) error {
	return writeTarGz(w, src, prefix, nil)
}

// PackMiseData writes the mise data directory dataDir as a toolchain tarball under the
// prefix "data". The shims directory is left out: mise's shims are symlinks to the mise
// binary outside the tarball, which extraction refuses; reshimMise recreates them.
func PackMiseData(w io.Writer, dataDir string) error {
	return writeTarGz(w, dataDir, filepath.Base(dataDir), func(rel string) bool { return rel == "shims" })
}

// writeTarGz is WriteTarGz leaving out every src-relative path skip reports (a skipped
// directory is left out with its contents).
func writeTarGz(w io.Writer, src, prefix string, skip func(rel string) bool) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	walkErr := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if skip != nil && rel != "." && skip(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := path.Join(prefix, filepath.ToSlash(rel))
		if rel == "." {
			if prefix == "" {
				return nil
			}
			name = prefix
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		mode := info.Mode()
		var link string
		switch {
		case mode.IsDir(), mode.IsRegular():
		case mode&fs.ModeSymlink != 0:
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		default:
			return nil
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = name
		if mode.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		var f *os.File
		if mode.IsRegular() {
			if f, err = os.Open(p); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			if f != nil {
				_ = f.Close()
			}
			return err
		}
		if f == nil {
			return nil
		}
		// The file may grow or shrink while it is archived; the entry must match its header.
		n, err := io.CopyN(tw, f, h.Size)
		if errors.Is(err, io.EOF) {
			_, err = io.CopyN(tw, zeroReader{}, h.Size-n)
		}
		return errors.Join(err, f.Close())
	})
	return errors.Join(walkErr, tw.Close(), gz.Close())
}

// zeroReader yields endless zero bytes.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
