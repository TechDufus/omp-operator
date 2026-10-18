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
| `docker-build-e2e` | | e2e image: the driver plus `test/fakeomp` installed as `omp` (`E2E_IMG`, default `omp-operator-e2e:dev`) |
| `ompcompat` | | Downloads the pinned omp release into `bin/`, checks it against the release's `SHA256SUMS.txt`, and runs `test/ompcompat` against it |
| `test-e2e` | `docker-build`, `docker-build-e2e` | Creates a kind cluster `omp-operator-e2e` (unless it exists), runs the e2e suite, and deletes the cluster on exit |

`mise run run` starts the manager, which needs `POD_NAMESPACE` (it refuses to start without it) and a writable
data directory (the default `/data` usually is not). With explicit flags:
`POD_NAMESPACE=omp-system go run ./cmd --data-dir=/tmp/omp-data`. All flags are listed in the
[operator reference](reference/operator.md#manager-flags).

## Layout

```text
api/v1alpha1/        the four kinds
cmd/main.go          the manager: controllers, hooks, dashboard and agent API servers
cmd/driver/          omp-operator-driver: prepare, run and toolsync inside pods
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
- **`test/ompcompat`** (build tag `ompcompat`, `mise run ompcompat`) checks the driver's assumptions against
  the real pinned omp, including that the built-in tool list matches `actions.ReservedToolNames`. When
  you bump the omp version (`OMP_VERSION` in `mise.toml`, `images/runtime/Dockerfile`, and the `omp-rpc`
  module in `go.mod`), this is the suite that tells you what changed.
- **e2e** (build tag `e2e`, `mise run test-e2e`) installs the chart on kind with the e2e image and exercises
  webhooks, schedules, actions, continuity, the dashboard and metrics.

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
