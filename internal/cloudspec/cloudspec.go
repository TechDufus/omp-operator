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

// Package cloudspec holds the pure rules of cloud threads that the thread
// turn log, the cloud API, the agent API, the pod builder, the controllers and
// the driver share. It does no I/O and imports only the API types, so every one
// of them can depend on it.
package cloudspec

import (
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
)

// Default paths of a thread pod when the user's paths cannot be mirrored.
const (
	DefaultHome      = "/home/agent"
	DefaultWorkspace = "/workspace"
)

// Defaults of v1alpha1.CloudSpec, for a spec the API server has not defaulted.
const (
	DefaultIdleTimeout     = 10 * time.Minute
	DefaultMaxSitting      = 4 * time.Hour
	DefaultThreadRetention = 168 * time.Hour
)

// IdleTimeout is c.IdleTimeout, or DefaultIdleTimeout when unset.
func IdleTimeout(c *v1alpha1.CloudSpec) time.Duration {
	return orDefault(c.IdleTimeout.Duration, DefaultIdleTimeout)
}

// MaxSitting is c.MaxSitting, or DefaultMaxSitting when unset.
func MaxSitting(c *v1alpha1.CloudSpec) time.Duration {
	return orDefault(c.MaxSitting.Duration, DefaultMaxSitting)
}

// ThreadRetention is c.ThreadRetention, or DefaultThreadRetention when unset.
func ThreadRetention(c *v1alpha1.CloudSpec) time.Duration {
	return orDefault(c.ThreadRetention.Duration, DefaultThreadRetention)
}

func orDefault(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

// ContinuityKey is the continuityKey of a thread's sittings; they share omp history through it.
func ContinuityKey(ns, thread string) string { return "thread/" + ns + "/" + thread }

// Branch is the result branch of a thread.
func Branch(thread string) string { return "omp-cloud/" + thread }

var podPathPattern = regexp.MustCompile(`^(/[A-Za-z0-9._@+-]+)+$`)

// systemDirs are the directories a thread's home or workspace may not be, or be under.
var systemDirs = []string{
	"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32",
	"/opt", "/proc", "/run", "/sbin", "/sys", "/tmp", "/usr", "/var",
}

const maxPodPathBytes = 512

// ValidPodPath reports whether p can be mounted in a thread pod: absolute,
// clean, at most 512 bytes, made of [A-Za-z0-9._@+-] segments, not "/", and not
// a system directory or inside one.
func ValidPodPath(p string) bool {
	if len(p) > maxPodPathBytes || p == "/" || path.Clean(p) != p || !podPathPattern.MatchString(p) {
		return false
	}
	for _, d := range systemDirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return false
		}
	}
	return true
}

// PodPaths returns the home and workspace a thread pod uses: the requested paths
// when both are valid, distinct, and the workspace is not a parent of the home;
// otherwise DefaultHome and DefaultWorkspace for both.
func PodPaths(p v1alpha1.ThreadPaths) (home, workspace string) {
	if ValidPodPath(p.Home) && ValidPodPath(p.Workspace) &&
		p.Home != p.Workspace && !strings.HasPrefix(p.Home, p.Workspace+"/") {
		return p.Home, p.Workspace
	}
	return DefaultHome, DefaultWorkspace
}

// MatchGitHost finds the cloud.git host that serves rawURL, a git remote URL as
// configured on the user's machine (scp-like user@host:path, ssh://, http:// or
// https://), by its lowercased host name.
//
// When a host matches, fetchURL is the credential-free URL to fetch and push
// through with that host's auth:
//   - token auth: <scheme>://<host>[:port]/<path>, where scheme is http or https
//     as written, or https for ssh and scp-like remotes; the port is kept only
//     for http(s) remotes;
//   - SSH auth: ssh://git@<host>[:port]/<path>; the port is kept only for ssh://
//     remotes.
//
// Without a match ok is false and fetchURL is rawURL when it is an http(s) URL
// (the base may be public), else empty.
func MatchGitHost(hosts []v1alpha1.CloudGitHost, rawURL string) (host *v1alpha1.CloudGitHost, fetchURL string, ok bool) {
	r, parsed := parseRemote(rawURL)
	if parsed {
		for i := range hosts {
			if hosts[i].Host != r.host {
				continue
			}
			if hosts[i].Auth.TokenSecretRef != nil {
				return &hosts[i], tokenURL(r), true
			}
			return &hosts[i], sshURL(r), true
		}
	}
	if parsed && (r.scheme == schemeHTTP || r.scheme == schemeHTTPS) {
		return nil, rawURL, false
	}
	return nil, "", false
}

// URL schemes of git remotes.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
	schemeSSH   = "ssh"
)

// remote is a parsed git remote URL.
type remote struct {
	scheme string // http, https, ssh, or "ssh" for scp-like
	scp    bool
	host   string // lowercased, without port
	port   string // as written, "" when absent
	path   string // without the leading slash
}

func parseRemote(raw string) (remote, bool) {
	if !strings.Contains(raw, "://") {
		return parseSCP(raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return remote{}, false
	}
	switch u.Scheme {
	case schemeHTTP, schemeHTTPS, schemeSSH:
	default:
		return remote{}, false
	}
	return remote{
		scheme: u.Scheme,
		host:   strings.ToLower(u.Hostname()),
		port:   u.Port(),
		path:   strings.TrimPrefix(u.EscapedPath(), "/"),
	}, true
}

// parseSCP parses [user@]host:path.
func parseSCP(raw string) (remote, bool) {
	rest := raw
	if _, after, ok := strings.CutLast(raw, "@"); ok {
		rest = after
	}
	host, p, found := strings.Cut(rest, ":")
	if !found || host == "" || p == "" || strings.ContainsAny(host, "/\\") {
		return remote{}, false
	}
	return remote{scheme: schemeSSH, scp: true, host: strings.ToLower(host), path: strings.TrimPrefix(p, "/")}, true
}

func tokenURL(r remote) string {
	scheme, port := schemeHTTPS, ""
	if r.scheme == schemeHTTP || r.scheme == schemeHTTPS {
		scheme, port = r.scheme, r.port
	}
	return scheme + "://" + hostPort(r.host, port) + "/" + r.path
}

func sshURL(r remote) string {
	port := ""
	if r.scheme == schemeSSH && !r.scp {
		port = r.port
	}
	return "ssh://git@" + hostPort(r.host, port) + "/" + r.path
}

func hostPort(host, port string) string {
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	if port != "" {
		return host + ":" + port
	}
	return host
}
