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

// Package driver is the in-pod half of omp-operator: the three subcommands of
// the omp-operator-driver binary. Prepare (init container) fetches the
// workspace, skills, toolchain and continuity state; Run (main container)
// drives `omp --mode rpc` for one session; Toolsync (main container of a
// toolsync pod) installs the workspace's mise tools and uploads them. All three
// talk to the operator through the agent API (internal/agentapi).
package driver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TechDufus/omp-operator/internal/agentapi"
)

// Defaults for the tunable timings and limits of Driver.
const (
	// DefaultReadyTimeout is how long omp has to emit its ready frame.
	DefaultReadyTimeout = 10 * time.Minute
	// DefaultEventInterval is how often buffered progress events are posted.
	DefaultEventInterval = 5 * time.Second
	// DefaultPromptGrace is added to the session timeout before the driver aborts a prompt itself;
	// omp's own --max-time normally ends the run first.
	DefaultPromptGrace = 30 * time.Second
	// DefaultNudgeMax caps the one nudge prompt sent when the agent ended without calling finish.
	DefaultNudgeMax = 5 * time.Minute
	// DefaultContinuityMax is the largest continuity tarball the driver uploads.
	DefaultContinuityMax int64 = 64 << 20
	// DefaultDownloadTimeout bounds one Flux artifact download.
	DefaultDownloadTimeout = 5 * time.Minute
	// DefaultDownloadMax is the largest Flux artifact accepted.
	DefaultDownloadMax int64 = 1 << 30
	// DefaultSessionTimeout applies when the session config carries no timeout.
	DefaultSessionTimeout = 30 * time.Minute
	// DefaultControlInterval is how often a thread sitting asks the operator whether the running turn was stopped.
	DefaultControlInterval = 5 * time.Second

	// extractMax bounds the bytes one extraction may write (zip-bomb guard).
	extractMax int64 = 8 << 30
	// controlTimeout bounds one small API call.
	controlTimeout = time.Minute
	// tarballTimeout bounds a toolchain or continuity transfer.
	tarballTimeout = 30 * time.Minute
)

// Paths locates everything the driver reads and writes. DefaultPaths is the in-pod
// layout from internal/agentapi; tests relocate it under a temporary directory.
type Paths struct {
	Token        string // bearer token for the agent API
	RunDir       string // scratch directory shared by the pod's containers
	Kubeconfig   string // written by prepare when the plan wants one
	Workspace    string // the agent's working directory
	Home         string // $HOME of the agent user
	SessionDir   string // omp --session-dir; continuity packs and unpacks it
	SkillsDir    string // <SkillsDir>/<skill>/SKILL.md
	OmpAgentDir  string // config.yml, models.yml, mcp.json
	MiseDir      string // toolchain root (the tarball unpacks here)
	MiseDataDir  string // MISE_DATA_DIR; toolsync packs it
	SystemPrompt string // file given to --append-system-prompt
	Transcript   string // export_html output

	ThreadSecrets string // where the thread's shipped secret files are mounted (init container)

	SAToken     string // service-account token file, referenced by the kubeconfig
	SACA        string // service-account CA bundle
	SANamespace string // file holding the pod's namespace
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// DefaultPaths returns the in-pod layout.
func DefaultPaths() Paths {
	return Paths{
		Token:        agentapi.TokenFile,
		RunDir:       agentapi.RunDir,
		Kubeconfig:   agentapi.KubeconfigFile,
		Workspace:    agentapi.WorkspaceDir,
		Home:         agentapi.HomeDir,
		SessionDir:   agentapi.SessionDir,
		SkillsDir:    agentapi.SkillsDir,
		OmpAgentDir:  agentapi.OmpAgentDir,
		MiseDir:      agentapi.MiseDir,
		MiseDataDir:  agentapi.MiseDataDir,
		SystemPrompt: agentapi.SystemPromptFile,
		Transcript:   agentapi.TranscriptFile,

		ThreadSecrets: agentapi.ThreadSecretsDir,
		SAToken:       saDir + "/token",
		SACA:          saDir + "/ca.crt",
		SANamespace:   saDir + "/namespace",
	}
}

// WithHome returns p relocated so that home is $HOME: the session, skills and omp
// config directories follow it.
func (p Paths) WithHome(home string) Paths {
	p.Home = home
	p.SessionDir = filepath.Join(home, "session")
	p.OmpAgentDir = filepath.Join(home, ".omp", "agent")
	p.SkillsDir = filepath.Join(p.OmpAgentDir, "skills")
	return p
}

// Driver bundles the API client, the layout and the tunables of one driver run.
// Construct it with New or FromEnv and override fields before calling a subcommand.
type Driver struct {
	API   *Client
	Paths Paths
	Log   *slog.Logger
	// Stderr receives omp's and git's diagnostics (redacted for omp). Defaults to os.Stderr.
	Stderr io.Writer

	// OmpBin is the omp executable ($OMP_BIN, else "omp" on PATH).
	OmpBin string
	// GitBin and MiseBin are the executables prepare and toolsync run.
	GitBin  string
	MiseBin string
	// PushAddr is the host:port of the push sidecar ($OMP_GITPUSH_ADDR); empty means the thread
	// has no push, and finalizing a turn only stores the result on the operator.
	PushAddr string

	ReadyTimeout    time.Duration
	EventInterval   time.Duration
	PromptGrace     time.Duration
	NudgeMax        time.Duration
	ControlInterval time.Duration
	ContinuityMax   int64
	DownloadTimeout time.Duration
	DownloadMax     int64
}

// New returns a Driver with the default timings.
func New(api *Client, paths Paths, log *slog.Logger) *Driver {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	ompBin := os.Getenv("OMP_BIN")
	if ompBin == "" {
		ompBin = "omp"
	}
	return &Driver{
		API:             api,
		Paths:           paths,
		Log:             log,
		Stderr:          os.Stderr,
		OmpBin:          ompBin,
		GitBin:          "git",
		MiseBin:         "mise",
		ReadyTimeout:    DefaultReadyTimeout,
		EventInterval:   DefaultEventInterval,
		PromptGrace:     DefaultPromptGrace,
		NudgeMax:        DefaultNudgeMax,
		ControlInterval: DefaultControlInterval,
		ContinuityMax:   DefaultContinuityMax,
		DownloadTimeout: DefaultDownloadTimeout,
		DownloadMax:     DefaultDownloadMax,
		PushAddr:        os.Getenv(EnvGitpushAddr),
	}
}

// Subcommands the binary accepts.
const (
	CmdPrepare  = "prepare"
	CmdRun      = "run"
	CmdToolsync = "toolsync"
	// CmdGitpush is the push sidecar of thread pods; it needs no agent API, see Gitpush.
	CmdGitpush = "gitpush"
)

// FromEnv builds the Driver of one subcommand from the pod environment: the API base URL
// from $OMP_OPERATOR_API, the identity from $OMP_OPERATOR_SESSION or $OMP_OPERATOR_AGENT,
// the token from agentapi.TokenFile, $HOME and $OMP_OPERATOR_WORKSPACE for the layout.
func FromEnv(cmd string, getenv func(string) string) (*Driver, error) {
	base := getenv(agentapi.EnvAPI)
	if base == "" {
		return nil, fmt.Errorf("%s is not set", agentapi.EnvAPI)
	}
	session, agent := getenv(agentapi.EnvSession), getenv(agentapi.EnvAgent)
	var target Target
	var err error
	switch {
	case cmd == CmdToolsync || (cmd == CmdPrepare && session == "" && agent != ""):
		if agent == "" {
			return nil, fmt.Errorf("%s is not set", agentapi.EnvAgent)
		}
		target, err = ParseTarget(KindAgents, agent)
	default:
		if session == "" {
			return nil, fmt.Errorf("%s is not set", agentapi.EnvSession)
		}
		target, err = ParseTarget(KindSessions, session)
	}
	if err != nil {
		return nil, err
	}
	paths := DefaultPaths()
	if home := getenv("HOME"); home != "" && home != paths.Home {
		paths = paths.WithHome(home)
	}
	if ws := getenv(agentapi.EnvWorkspace); ws != "" {
		paths.Workspace = ws
	}
	return New(NewClient(base, paths.Token, target), paths, nil), nil
}

// ParseTarget parses "<namespace>/<name>".
func ParseTarget(kind, v string) (Target, error) {
	ns, name, ok := strings.Cut(v, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return Target{}, fmt.Errorf("invalid identity %q, want <namespace>/<name>", v)
	}
	return Target{Kind: kind, Namespace: ns, Name: name}, nil
}

// child returns a context for one bounded operation.
func child(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}
