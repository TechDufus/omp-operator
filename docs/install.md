# Install

Helm is the only supported install path. The chart is published as a signed OCI artifact:

```sh
helm install omp oci://ghcr.io/techdufus/charts/omp-operator --version 0.1.0 \
  -n omp-system --create-namespace
```

The release name (`omp` here) only affects the names of the RBAC objects and the
`app.kubernetes.io/instance` label. The operator Deployment, Service and the other namespaced objects
have fixed names (`omp-operator`, `omp-auth`, ...), so install one release per namespace.

Images, all `linux/amd64`:

| Image | Used for |
|---|---|
| `ghcr.io/techdufus/omp-operator` | The operator (controllers, hooks, dashboard, agent API) |
| `ghcr.io/techdufus/omp-operator-runtime` | Session pods, toolsync pods and the auth-gateway. Holds `omp`, `mise`, `git`, `ssh`, `jq`, `python3` and `omp-operator-driver` |

Both tags default to the chart's `appVersion` (`v0.1.0` for chart `0.1.0`).

## What the chart installs

| Object | Notes |
|---|---|
| CRDs `ompagents`, `omptriggers`, `ompsessions`, `ompactions`, `ompthreads` | From the chart's `crds/` directory |
| Deployment `omp-operator` | 1 replica, strategy `Recreate`, no leader election. Runs as UID/GID 65532 with a read-only root filesystem, all capabilities dropped and the `RuntimeDefault` seccomp profile |
| Service `omp-operator` | Ports `web` 8080, `hooks` 8082, `agent` 8083, `metrics` 9090 |
| PVC `omp-operator-data` | Mounted at `/data`: session files, transcripts, continuity and toolchain tarballs |
| ConfigMap `omp-operator-access` | The dashboard access file rendered from `dashboard.*`. A checksum annotation restarts the operator when it changes |
| ServiceAccount `omp-operator` | |
| ClusterRole `<release>-omp-operator-manager` and its binding | Cluster-scoped rules generated from the code's RBAC markers: the five CRDs, Flux sources (get, list, watch, patch), Kustomizations and HelmReleases (get, patch), Deployments, StatefulSets and DaemonSets (get, patch), Events (create, patch) |
| Role or ClusterRole `<release>-omp-operator-namespaced` and bindings | Pods (get, list, watch, create, delete), Secrets (get, create, update, patch, delete), ConfigMaps (get). See [namespaces and RBAC](#namespaces-and-rbac) |
| With `authGateway.enabled`: Deployment, Service and PVC `omp-auth` | The model auth-gateway and its credential broker. See [model access](model-access.md) |

The chart creates **no Ingress, Gateway or NetworkPolicy**. See [Exposing the hooks and the
dashboard](#exposing-the-hooks-and-the-dashboard) and [security](security.md#network-policy).

At runtime the operator also creates, when missing, the Secret `omp-operator-dashboard-session` (the
dashboard's cookie-signing key) in its own namespace, and with `authGateway.enabled` the clients
Secret named by `authGateway.clientsSecret`.

## Values

Every key of `values.yaml`; the full table with defaults is in the
[operator reference](reference/operator.md#chart-values).

```yaml
image:        {repository: ghcr.io/techdufus/omp-operator, tag: "", digest: "", pullPolicy: IfNotPresent}
runtimeImage: {repository: ghcr.io/techdufus/omp-operator-runtime, tag: "", digest: ""}
dashboard:
  url: ""
  oidc: {issuerURL: "", clientID: "", clientSecret: {existingSecret: "", key: client-secret},
         scopes: [openid, email, profile, groups], usernameClaim: email, groupsClaim: groups}
  basic: {existingSecret: ""}
  access: []
hooks: {publicURL: ""}
actions: {targetNamespaces: []}
persistence: {storageClass: "", size: 20Gi}
authGateway: {enabled: false, clientsSecret: omp-auth-clients, persistence: {storageClass: "", size: 1Gi}}
rbac: {agentNamespaces: []}
resources: {requests: {cpu: 50m, memory: 128Mi}, limits: {memory: 512Mi}}
priorityClassName: ""
nodeSelector: {}
tolerations: []
```

A `digest` pins an image as `repository@digest` and takes precedence over `tag`.

## Namespaces and RBAC

`rbac.agentNamespaces` decides where agents may live.

- **Empty (default):** a ClusterRole and ClusterRoleBinding grant the namespaced rules (pods, Secrets,
  ConfigMaps) in every namespace, and the operator serves agents in any namespace.
- **A list:** a Role and RoleBinding with the same rules are created in each listed namespace *and in the
  release namespace* (the operator keeps its own Secrets there), and the operator is started with
  `--agent-namespaces=<list>`. Objects in other namespaces are ignored: their agents never get a status,
  and hook requests for their triggers answer 401. The listed namespaces must exist before you install.

```sh
kubectl create namespace platform
helm install omp oci://ghcr.io/techdufus/charts/omp-operator --version 0.1.0 \
  -n omp-system --create-namespace --set 'rbac.agentNamespaces={platform}'
```

The operator's own namespace is never a valid place for agents' action targets, see
[actions](actions.md#target-namespace-rule).

An agent that needs Kubernetes access from its tools sets `spec.serviceAccountName`. That account, and
whatever RBAC you grant it, are yours to create; the operator only mounts the token and writes a
kubeconfig at `/var/run/omp-operator/kubeconfig`. See [security](security.md).

## Runtime image

`runtimeImage` is the default image of agents (`--default-runtime-image`) and of the auth-gateway. An
agent may set `spec.image` to use its own. A custom image must contain `omp`, `omp-operator-driver`
(its entrypoint), `git`, and `mise` when the agent uses `toolchain`, and must run as UID 10001. Build it
`FROM` the published runtime image.

An agent with no `image` while `runtimeImage` is empty is not Ready (reason `NoImage`).

## Dashboard access

Without a sign-in method, every dashboard page except `/healthz` answers `503 dashboard access not
configured`. With a method but no `dashboard.access` rules, signed-in users see nothing. Configure both;
see [dashboard](dashboard.md).

Basic-auth fallback:

```sh
kubectl -n omp-system create secret generic omp-dashboard-basic \
  --from-literal=htpasswd="$(htpasswd -nBC 12 admin)"      # prompts for the password

helm upgrade omp oci://ghcr.io/techdufus/charts/omp-operator --version 0.1.0 -n omp-system --reuse-values \
  --set dashboard.basic.existingSecret=omp-dashboard-basic \
  --set 'dashboard.access[0].users[0]=basic:admin' \
  --set 'dashboard.access[0].namespaces[0]=*'
```

OIDC needs `dashboard.url` (the external URL; the redirect URL is `<url>/auth/callback`),
`dashboard.oidc.issuerURL`, `dashboard.oidc.clientID` and a Secret holding the client secret
(`dashboard.oidc.clientSecret.existingSecret`, key `dashboard.oidc.clientSecret.key`). The chart fails to
render when one of them is missing.

## Exposing the hooks and the dashboard

Both are plain HTTP Services; put your own Ingress, Gateway API route or tunnel in front and terminate
TLS there. The dashboard sets `Secure` cookies, so use HTTPS for OIDC sign-in.

| Service port | Serves | Expose it? |
|---|---|---|
| `hooks` 8082 | `POST /hooks/<namespace>/<trigger>` | Yes, to the systems that send webhooks. Set `hooks.publicURL` so the dashboard shows the full URL |
| `web` 8080 | The dashboard | Yes, to people. Set `dashboard.url` so notifications link to it |
| `agent` 8083 | The API session pods use | **No.** Cluster-internal only. See [security](security.md#network-policy) |
| `metrics` 9090 | Prometheus metrics, plain HTTP | To your monitoring system only |

## Storage

`persistence` sizes the PVC at `/data`. It holds transcripts, event logs, continuity tarballs (at most 64
MiB each) and toolchain tarballs (at most 2 GiB each, the current and the previous per agent). Losing it
loses transcripts, continuity and toolchains but no configuration. The metrics
`omp_operator_data_free_bytes` and `omp_operator_data_capacity_bytes` track it. The access mode is
`ReadWriteOnce` and the operator runs as a single replica.

## Actions that reach other namespaces

By default an action can only target its own namespace. `actions.targetNamespaces` is a list of extra
namespaces actions may target, or `["*"]` for any namespace except the operator's. See
[actions](actions.md#target-namespace-rule).

## Upgrade and uninstall

- Helm applies a chart's `crds/` directory on **install only**. Before `helm upgrade` to a release that
  changes the API, apply the CRDs yourself:

  ```sh
  helm pull oci://ghcr.io/techdufus/charts/omp-operator --version <new> --untar
  kubectl apply --server-side -f omp-operator/crds
  helm upgrade omp oci://ghcr.io/techdufus/charts/omp-operator --version <new> -n omp-system --reuse-values
  ```

- `helm uninstall` removes the operator, the data PVC and the auth-gateway PVC (so the stored gateway
  login), but leaves the CRDs and your custom resources.
- Delete `OmpAgent` objects *before* uninstalling: their finalizer needs the operator to remove their
  stored toolchain and continuity data.

## Verify the release signatures

Images and the chart are signed keylessly with cosign by the release workflow. For each artifact:

```sh
cosign verify <ref>@<digest> \
  --certificate-identity-regexp '^https://github\.com/TechDufus/omp-operator/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Use these references:

| Artifact | `<ref>` |
|---|---|
| Operator image | `ghcr.io/techdufus/omp-operator` |
| Runtime image | `ghcr.io/techdufus/omp-operator-runtime` |
| Chart | `ghcr.io/techdufus/charts/omp-operator` |

The digests are in the release workflow run's summary. For the chart, `helm pull oci://ghcr.io/techdufus/charts/omp-operator --version 0.1.0` prints its `Digest:`. To make the cluster
run exactly what you verified, set `image.digest` and `runtimeImage.digest`.
