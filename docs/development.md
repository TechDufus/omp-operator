# Development

omp-operator is a [kubebuilder](https://book.kubebuilder.io) v4 project: Go 1.27, controller-runtime v0.25.
Tools are pinned in `mise.toml`; **every repo command is a mise task, there is no Makefile**. Install
[mise](https://mise.jdx.dev), then in the repository:

```sh
mise install        # Go, helm, kind, kubectl, yq, kubebuilder, golangci-lint
mise tasks          # list the tasks
mise run ci         # lint, unit + envtest + chart tests, helm lint
```

Docker is needed for the image and e2e tasks.

## Tasks

mise runs a task's independent dependencies in parallel, so every task that writes generated files is chained
behind the others.

| Task | Depends on | What it does |
|---|---|---|
| `generate` | | controller-gen: DeepCopy methods |
| `manifests` | `generate` | controller-gen: CRDs into `config/crd/bases` and the manager ClusterRole into `config/rbac/role.yaml` |
| `chart-sync` | `manifests` | Copies the CRDs to `charts/omp-operator/crds/` and the `rules` of the ClusterRole to `charts/omp-operator/files/rules.yaml` |
| `verify-generated` | `chart-sync` | Fails when `git status --porcelain` shows anything, i.e. generated files are stale. Run it after committing |
| `lint` | `generate` | `golangci-lint run` |
| `test` | `manifests` | `go vet`, then the unit, envtest and chart tests of every package except `test/e2e` and `test/ompcompat`, with coverage in `cover.out`. Provides the envtest control plane through `setup-envtest` |
| `ci` | `lint`, `test`, `chart-sync` | The above, then `helm lint charts/omp-operator` |
| `build` | `generate` | Builds `bin/manager` and `bin/omp-operator-driver` |
| `run` | `manifests` | Runs the manager against your current kubeconfig (set `POD_NAMESPACE`; see below) |
| `docker-build` | | Controller image (`IMG`, default `omp-operator:dev`) |
| `docker-build-runtime` | | Runtime image (`RUNTIME_IMG`, default `omp-operator-runtime:dev`) |
| `docker-build-e2e` | | e2e image on `debian:trixie-slim` with git: the driver, `test/fakeomp` installed as `omp` and `test/gitserver` as `omp-e2e-gitserver` (`E2E_IMG`, default `omp-operator-e2e:dev`) |
| `omp:download` | | Downloads the pinned omp release (`OMP_VERSION`) to `bin/omp-linux-x64` and checks it against the release's `SHA256SUMS.txt` |
| `ompcompat` | `omp:download` | Runs `test/ompcompat` against the downloaded omp |
| `test-e2e` | `omp:download`, `docker-build`, `docker-build-e2e` | Creates a kind cluster `omp-operator-e2e` (unless it exists) with its own kubeconfig in `bin/` (your `~/.kube/config` is never touched), runs the e2e suite with `OMP_BIN` set to the downloaded omp, and deletes the cluster on exit |
| `dev:up` | | Creates (unless it exists) or updates the long-lived kind cluster `omp-dev`; see [dev cluster](#dev-cluster) |
| `dev:login [anthropic\|openai-codex]` | | Logs the dev cluster's auth gateway in to a subscription |
| `dev:status` | | Operator pods, agents, triggers, sessions and threads of the dev cluster |
| `dev:github-token` | | Stores your `gh auth token` as Secret `omp-dev/github-token`, which agent `coder` pushes with (no docker needed) |
| `dev:client` | | Links `clients/omp-cloud` into `~/.omp/agent/extensions` and writes `~/.omp/agent/cloud.json` for the dev cluster |
| `dev:down` | | Deletes the dev cluster and `bin/dev` |

`mise run run` starts the manager, which needs `POD_NAMESPACE` (it refuses to start without it) and a writable
data directory (the default `/data` usually is not). With explicit flags:
`POD_NAMESPACE=omp-system go run ./cmd --data-dir=/tmp/omp-data`. All flags are listed in the
[operator reference](reference/operator.md#manager-flags).

## Dev cluster

`mise run dev:up` keeps a kind cluster `omp-dev` around for real use. It builds the three images with a fresh
`dev-<epoch>` tag, loads them into kind, installs the chart with `hack/dev/values.yaml` (auth gateway on, agents
in namespace `omp-dev`, the cloud API token from `bin/dev/token`) and applies every other manifest in `hack/dev/`
(a NodePort Service, the sample agent `hello`, and the cloud agents `coder` and `coder-fake`). It is idempotent:
run it again after a code change. Everything uses `bin/dev/kubeconfig`, never `~/.kube/config`:

- dashboard and cloud API `http://127.0.0.1:30080`, user `dev`, password in `bin/dev/password`
- webhooks `http://127.0.0.1:30082/hooks/<namespace>/<trigger>`
- `bin/dev/token` is the cloud API bearer token (principal `token:dev`, operator in every namespace)

`mise run dev:login` logs the gateway in to a Claude subscription (`openai-codex` for ChatGPT); the login
survives `dev:up` and lasts until `dev:down`.

To use cloud agents from your own omp: `mise run dev:github-token` (so `coder` can push `omp-cloud/*` branches
to GitHub), then `mise run dev:client`. In any omp session inside a git repository, `/cloud <task>` hands the
conversation to `coder`; `/cloud help` lists the other verbs. `coder-fake` runs fakeomp on the e2e image for
trying the flow without a model (set `"agent": "coder-fake"` in `~/.omp/agent/cloud.json`).

## Layout

```text
api/v1alpha1/        the five kinds
cmd/main.go          the manager: controllers, hooks, dashboard and agent API servers
cmd/driver/          omp-operator-driver: prepare, run and toolsync inside pods, and the gitpush sidecar of cloud thread pods
internal/controller/ one reconciler per kind
internal/hooks/      the webhook handler        internal/episodes/  grouping and budget decisions (pure)
internal/adapters/   alertmanager, grafana, generic    internal/celx/ CEL environments and cache
internal/hookauth/   bearer, basic, hmac        internal/schedule/  5-field cron (pure)
internal/prompt/     templates, fences, system prompt    internal/actions/ action types and execution
internal/agentapi/   the API session and toolsync pods call    internal/podspec/ session and toolsync pods
internal/agentspec/  OmpAgent validation        internal/gateway/   managed gateway tokens and clients Secret
internal/store/      files under --data-dir     internal/notify/    sinks and notices
internal/dashboard/  web UI and access rules    internal/metrics/   collectors
internal/driver/     what runs in the pod       internal/config/    operator-wide options
test/fakeomp/        scripted fake `omp --mode rpc`
test/fakeprovider/  fake OpenAI chat-completions provider
test/ompcompat/      tests against the real pinned omp
test/chart/          `helm template` assertions
test/e2e/            kind end-to-end suite
charts/omp-operator/ the Helm chart
images/runtime/      runtime image          images/e2e/  e2e image
```

## Tests

- **Unit tests** are table-driven and sit beside the code. The pure packages (`episodes`, `schedule`,
  `prompt`, `celx`, `adapters`, `hookauth`) have no cluster dependency.
- **envtest** (`internal/controller`) runs the reconcilers against a real API server and etcd. `mise run test` finds them.
- **Chart tests** (`test/chart`) run `helm template` with different values and assert the render. They are part of `mise run test`.
- **`test/fakeomp`** is a scripted stand-in for `omp --mode rpc`, driven by the environment variable
  `FAKEOMP_SCRIPT` (JSON: `mode` of `normal`, `exit-early`, `no-finish`, `garbage` or `hang`; `calls`, a list of
  `{tool, args}`; and `finish`). `FAKEOMP_ECHO_ENV` names an environment variable whose value fakeomp leaks
  on purpose into its events and results, so the redaction tests can check it is scrubbed. The driver tests and the e2e suite run against it.
  A prompt that starts with `FAKEOMP:` carries the script of that one turn as JSON and replaces the calls and
  `finish` for it, which is how cloud thread turns are scripted: `files` (relative path to content, written into
  the working directory), `checkFiles` and `checkEnv` (fakeomp writes `fakeomp-check.txt` there: `cwd: <dir>`,
  then `<path>: present|absent` and `<NAME>: set|unset` lines; paths may be absolute), `reply` (default
  `fake reply`) and `mode` (`hang` or `exit-early`, as above). Whatever the turn, `get_last_assistant_text`
  answers with the reply, followed by the `FAKEOMP_ECHO_ENV` value when that is set, and the same text is sent as
  an assistant `message_end` event. fakeomp skips the `finish` call when the client registered host tools and
  `finish` is not one of them, as in cloud threads. When the session directory holds a real omp session (a `*.jsonl`
  whose first lines carry a `{"type":"session"}` header, such as a restored handoff conversation), `open_session`
  continues it, appending to that file and reporting its id and path, as omp does; otherwise it uses `fake.jsonl`.
- **`test/gitserver`** is a smart-HTTP git server for the e2e suite: `git-http-backend` behind Basic auth
  (`x-access-token:$GITSERVER_TOKEN`) over the bare repositories listed in `GITSERVER_REPOS`, rooted at
  `GITSERVER_ROOT` (default `/repos`), on `:8080`. It runs from the e2e image as `omp-e2e-gitserver`.
- **`test/fakeprovider`** (no build tag) is a fake OpenAI chat-completions provider shared by the ompcompat and e2e
  suites: `fakeprovider.New(t, opts...)` starts an httptest server that streams a reply (`WithReply`, default
  `done`) or, with `WithToolCall(tool, jsonArgs)`, calls a tool until the conversation holds a tool result. It counts
  and records requests (`Count`, `Requests`, `RawRequests`), and `ModelsYAML()` is the `models.yml` that registers it
  as provider `fake`, model `fake-model` (`fakeprovider.Model`).
- **`test/ompcompat`** (build tag `ompcompat`, `mise run ompcompat`) checks the driver's assumptions against
  the real pinned omp, including that the built-in tool list matches `actions.ReservedToolNames`. It also loads
  `clients/omp-cloud` into the real omp against a fake cloud API: `/cloud do it` must reach the extension (not the model),
  upload a full-mode handoff whose conversation carries a new session id, and that conversation must resume in a fresh
  omp at the same working directory. Two omp behaviours the tests pin: extension notifications only reach an RPC client
  without `--no-ui`, and `open_session` cancels a resume whose working directory differs from the session's. When
  you bump the omp version (`OMP_VERSION` in `mise.toml`, `images/runtime/Dockerfile`, and the `omp-rpc`
  module in `go.mod`), this is the suite that tells you what changed.
- **e2e** (build tag `e2e`, `mise run test-e2e`) installs the chart on kind with the e2e image and exercises
  webhooks, schedules, actions, continuity, the dashboard and metrics. The `cloud threads` spec runs the real pinned omp
  (`$OMP_BIN`, from `omp:download`) with `clients/omp-cloud` in a throw-away HOME and repository under
  `bin/e2e-client/`, against the cloud API of the kind cluster. Its repository's `origin` is the in-cluster git
  server (`gitserver` in namespace `e2e`, reached from this machine through a port-forward and a repo-local
  `url.<forward>.insteadOf`), and the owner's omp talks to `test/fakeprovider`. The cloud agent `cloud-fake` runs
  fakeomp with `cloud.idleTimeout: 1m` (the shortest the API accepts), so the spec waits out two idle timeouts to
  check the cold restore. It covers the handoff, the warm follow-up, redaction, the cold restore, `/cloud pull`,
  `/cloud resume`, `/cloud stop`, API authentication, the dashboard thread page and the thread metrics.

## The driver in cloud threads

A sitting of an `OmpThread` (a session with `spec.threadRef`) uses the same three driver commands, with these differences:

- **`prepare`** rebuilds the user's workspace in the pod: `git init`, fetch the base commit from the remote (the
  branch the base came from, else the commit by id), then the tip: the previous sitting's stored result if there is
  one, else the uploaded handoff bundle, else the base. The thread branch `omp-cloud/<thread>` is checked out
  there, `origin` is set without credentials, shipped secret files are copied (mode 0600) and kept out of commits
  with `.git/info/exclude`, the shipped omp setup is extracted into `$HOME/.omp/agent`, and the mise toolchain
  comes from the agent's cache (`toolchains/<hash>`) or is installed and cached. A failed install is an event.
- **`run`** merges the pod's overrides into the user's own `config.yml` (model roles of providers the gateway does
  not serve are mapped to the thread's model; computer use, collab rooms and plugin auto-updates are off) and
  starts one omp with no tool flag when the agent allows every built-in tool. It then serves the thread's turns
  one after another: after each turn it commits what changed, keeps the branch on the newest commit, stores the
  result bundle on the operator and, with a push sidecar, asks it to push. It ends the sitting after
  `cloud.idleTimeout` without a turn, or when no further turn would fit in `cloud.maxSitting`. A turn that ends
  for auth or retryable reasons (or a pod that is terminating) stays running on the operator and is delivered to the
  next sitting; the work is committed, bundled and its conversation stored first.
- **`gitpush`** is the sidecar of pods whose git host has `push`. It listens on `127.0.0.1:8090` and serves only
  `POST /push {"head": "<commit>"}`. The remote URL, branch and credentials come from its own environment
  (`OMP_GITPUSH_URL`, `OMP_GITPUSH_BRANCH`, `OMP_GITPUSH_WORKSPACE`, `OMP_GITPUSH_AUTH`, `OMP_GITPUSH_AUTHDIR`,
  `OMP_GITPUSH_USERNAME`); a request can only name a commit. It force-pushes from a private bare repository
  that borrows the workspace's objects (git alternates) and runs git with a clean environment, so nothing in the
  agent's `.git/config` can redirect or intercept the push. Errors are at most 300 characters, with the token removed.

`OMP_GITPUSH_ADDR` in the agent container tells `run` where the sidecar is; without it a turn only stores its result.

## Generated files

`api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/*`, `config/rbac/role.yaml`,
`charts/omp-operator/crds/*` and `charts/omp-operator/files/rules.yaml` are generated; never edit them. After
changing the API types or a `+kubebuilder:rbac` marker, run `mise run chart-sync` and commit the result. CI
runs `mise run verify-generated` and fails otherwise. The chart's namespaced rules (pods, Secrets, ConfigMaps)
are not generated; they live in `charts/omp-operator/templates/_helpers.tpl`.

## CI and releases

`.github/workflows/ci.yml` runs three jobs on every push to `main` and every pull request: `ci` (`mise run ci` then
`mise run verify-generated`), `ompcompat` and `e2e`. Every Action is pinned by commit SHA.

A release is a signed tag matching `v*`. `.github/workflows/release.yml` then builds and pushes the two images
to `ghcr.io/techdufus/`, signs them by digest with keyless cosign, packages the chart with
`--version <tag without v> --app-version <tag>`, pushes it to `oci://ghcr.io/techdufus/charts`, signs it,
writes the three digests to the job summary and creates a GitHub release with the chart attached. GHCR
packages are private on first push; make each public once in its package settings. See
[install](install.md#verify-the-release-signatures) for verification.
