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

// Command gitserver is a smart-HTTP git server for the e2e suite. It serves bare repositories
// through git-http-backend behind Basic authentication (user x-access-token and the token).
//
// Environment:
//
//	GITSERVER_ROOT    directory holding the repositories (default /repos)
//	GITSERVER_TOKEN   the password; required
//	GITSERVER_REPOS   comma-separated repository names; each <name>.git is created bare with
//	                  http.receivepack enabled when missing
package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	backend    = "/usr/lib/git-core/git-http-backend"
	listenAddr = ":8080"
	basicUser  = "x-access-token"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	root := os.Getenv("GITSERVER_ROOT")
	if root == "" {
		root = "/repos"
	}
	token := os.Getenv("GITSERVER_TOKEN")
	if token == "" {
		return errors.New("GITSERVER_TOKEN is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for name := range strings.SplitSeq(os.Getenv("GITSERVER_REPOS"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			if err := initRepo(root, name); err != nil {
				return err
			}
		}
	}

	handler := &cgi.Handler{
		Path: backend,
		Root: "/",
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=e2e",
			"HOME=" + root,
		},
	}
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           authenticate(token, handler),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("serving %s on %s", root, listenAddr)
	return srv.ListenAndServe()
}

// initRepo creates <root>/<name>.git as a bare repository that accepts pushes over HTTP.
func initRepo(root, name string) error {
	dir := filepath.Join(root, name+".git")
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	for _, args := range [][]string{
		{"init", "--bare", "-q", "--initial-branch=main", dir},
		{"-C", dir, "config", "http.receivepack", "true"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

// authenticate requires Basic x-access-token:<token>; the comparison is constant-time.
func authenticate(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(basicUser))
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(token))
		if !ok || userOK&passOK != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="gitserver"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
