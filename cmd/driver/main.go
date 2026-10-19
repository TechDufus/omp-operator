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

// Command omp-operator-driver is the in-pod half of omp-operator. It is the init
// container ("prepare") and main container ("run" or "toolsync") of session and
// toolsync pods, and the push sidecar ("gitpush") of cloud thread pods.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/TechDufus/omp-operator/internal/driver"
)

const usage = `omp-operator-driver runs inside omp-operator session and toolsync pods.

Usage:
  omp-operator-driver prepare    fetch the workspace, skills, toolchain and continuity state
  omp-operator-driver run        run omp for one session and report the outcome
  omp-operator-driver toolsync   install the workspace's mise tools and upload them
  omp-operator-driver gitpush    sidecar of cloud thread pods: push the thread branch on request

Environment:
  OMP_OPERATOR_API        base URL of the operator's agent API
  OMP_OPERATOR_SESSION    <namespace>/<name> of the session (prepare, run)
  OMP_OPERATOR_AGENT      <namespace>/<name> of the agent (toolsync, and prepare in toolsync pods)
  OMP_OPERATOR_WORKSPACE  workspace directory (default /workspace)
  OMP_BIN                 omp executable for run (default: omp on PATH)
  OMP_GITPUSH_ADDR        host:port of the push sidecar (run, cloud threads with push)

gitpush listens on 127.0.0.1:8090 and is configured by OMP_GITPUSH_URL, OMP_GITPUSH_BRANCH,
OMP_GITPUSH_WORKSPACE, OMP_GITPUSH_AUTH (token|ssh), OMP_GITPUSH_AUTHDIR and OMP_GITPUSH_USERNAME.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	case driver.CmdPrepare, driver.CmdRun, driver.CmdToolsync, driver.CmdGitpush:
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if args[0] == driver.CmdGitpush {
		if err := driver.Gitpush(ctx, os.Getenv, slog.New(slog.NewTextHandler(os.Stderr, nil))); err != nil {
			fmt.Fprintf(os.Stderr, "omp-operator-driver %s: %v\n", args[0], err)
			return 1
		}
		return 0
	}

	d, err := driver.FromEnv(args[0], os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "omp-operator-driver %s: %v\n", args[0], err)
		return 2
	}

	switch args[0] {
	case driver.CmdPrepare:
		err = d.Prepare(ctx)
	case driver.CmdRun:
		err = d.Run(ctx)
	case driver.CmdToolsync:
		err = d.Toolsync(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "omp-operator-driver %s: %v\n", args[0], err)
		return 1
	}
	return 0
}
