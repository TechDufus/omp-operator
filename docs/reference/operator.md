# Operator reference

## Manager flags

The operator binary (`/manager`) takes these flags, plus controller-runtime's zap logging flags
(`--zap-log-level` and friends). The chart sets the ones marked *chart*. `POD_NAMESPACE` (from the downward API) is required.

| Flag | Default | Chart | Purpose |
|---|---|---|---|
| `--metrics-bind-address` | `:9090` | `:9090` | Metrics endpoint; `0` disables it |
| `--metrics-secure` | `false` | `false` | Serve metrics over HTTPS with authn/authz |
| `--health-probe-bind-address` | `:8081` | `:8081` | `/healthz` and `/readyz` |
| `--web-bind-address` | `:8080` | `:8080` | Dashboard |
| `--hooks-bind-address` | `:8082` | `:8082` | Webhook receiver |
| `--agent-api-bind-address` | `:8083` | `:8083` | The API session and toolsync pods use |
| `--service-name` | `omp-operator` | `omp-operator` | Pods reach the API at `http://<service-name>.<POD_NAMESPACE>.svc:<agent-api port>` |
| `--data-dir` | `/data` | `/data` | Session files, continuity and toolchains |
| `--default-runtime-image` | empty | `runtimeImage` | Image used when an agent sets none |
| `--dashboard-url` | empty | `dashboard.url` | External dashboard URL, used for notification links |
| `--hooks-public-url` | empty | `hooks.publicURL` | External hooks URL, shown in the dashboard |
| `--gateway-clients-secret` | empty | `authGateway.clientsSecret`, only when `authGateway.enabled` | Enables managed gateway tokens and names the clients Secret |
| `--agent-namespaces` | empty (all) | `rbac.agentNamespaces` | Comma list of namespaces served |
| `--action-target-namespaces` | empty | `actions.targetNamespaces` | Comma list, or `*`: namespaces actions may target besides their own |
| `--access-config` | `/etc/omp-operator/access.yaml` | same | Dashboard access file. A missing file means no sign-in method |
| `--cloud-max-upload-bytes` | `1073741824` | `cloud.maxUploadBytes` | Largest repository bundle the [cloud API](cloud-api.md) accepts |

The operator runs one replica without leader election. The webhook, dashboard and agent API servers start after
the caches sync.

At startup it creates, when missing: the clients Secret (with managed tokens), and the Secret
`omp-operator-dashboard-session` (key `key`, 32 random bytes) in `POD_NAMESPACE`. Every 60 seconds it samples the free and total bytes of the
data volume. Only pods labelled `app.kubernetes.io/managed-by: omp-operator` are cached, and only in the served
namespaces; Secrets, ConfigMaps and action targets are read uncached.

## Ports

| Port | Name | Serves |
|---|---|---|
| 8080 | `web` | Dashboard, and the cloud API under `/api/` |
| 8081 | `health` | Probes (container port only, not in the Service) |
| 8082 | `hooks` | `POST /hooks/<namespace>/<trigger>` |
| 8083 | `agent` | Agent API |
| 9090 | `metrics` | Prometheus |

## Agent API

Used by pods; bearer-token authenticated (see [security](../security.md#api-tokens)); keep it cluster-internal.
Session routes (`/v1/sessions/{ns}/{name}...`) take the session's token, agent routes (`/v1/agents/{ns}/{name}...`)
the agent's toolsync token.

| Route | Purpose |
|---|---|
| `GET /v1/sessions/{ns}/{name}/plan` | Workspace, skills, toolchain and continuity plan |
| `GET /v1/sessions/{ns}/{name}` | The session's configuration: prompt, system prompt, model, tools, timeout, MCP, actions, redaction sources |
| `POST .../prepared` | Workspace revision and toolchain hash from `prepare` |
| `GET .../toolchain` | The stored toolchain tarball; 404 when none |
| `GET .../continuity`, `PUT .../continuity` | The continuity tarball (at most 64 MiB) |
| `POST .../actions/{tool}` | Run an action or `finish`; refusals are HTTP 200 with `isError`, an unknown tool is 404 |
| `POST .../events` | Progress events (at most 5000 lines kept) |
| `PUT .../transcript` | The HTML transcript (at most 64 MiB) |
| `POST .../complete` | Outcome, usage, error |
| `GET /v1/agents/{ns}/{name}/plan` | Workspace and toolchain sections |
| `GET /v1/agents/{ns}/{name}/toolchain` | The stored hash |
| `POST .../toolchain/unchanged` | The stored toolchain still matches |
| `PUT .../toolchain/{hash}` | Upload a toolchain tarball (at most 2 GiB) |

## Data directory

```text
<data-dir>/sessions/<session uid>/{events.jsonl, transcript.html, report.md}
<data-dir>/continuity/<namespace>/<agent>/<sha16 of key>.tar.gz
<data-dir>/toolchains/<namespace>/<agent>/<hash>.tar.gz        # current and previous
```

Files are written atomically. A session's directory is removed with the session; an agent's `continuity` and
`toolchains` directories are removed with the agent.

## RBAC

Cluster-scoped rules, from the code's markers (`config/rbac/role.yaml`, copied to the chart's `files/rules.yaml`):

| API group | Resources | Verbs |
|---|---|---|
| `omp.techdufus.com` | `ompagents`, `omptriggers`, `ompsessions`, `ompactions` | create, delete, get, list, patch, update, watch |
| `omp.techdufus.com` | the four kinds' `/status` | get, patch, update |
| `omp.techdufus.com` | the four kinds' `/finalizers` | update |
| `source.toolkit.fluxcd.io` | `gitrepositories`, `ocirepositories`, `helmrepositories` | get, list, patch, watch |
| `kustomize.toolkit.fluxcd.io` | `kustomizations` | get, patch |
| `helm.toolkit.fluxcd.io` | `helmreleases` | get, patch |
| `apps` | `deployments`, `statefulsets`, `daemonsets` | get, patch |
| core | `events` | create, patch |

Namespaced rules, rendered by the chart: pods (get, list, watch, create, delete), Secrets (get, create, update, patch, delete),
ConfigMaps (get), in each of `rbac.agentNamespaces` and the release namespace, or cluster-wide when that list is empty.

## Chart values

| Value | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/techdufus/omp-operator` | Operator image |
| `image.tag` | `""` (the chart's `appVersion`) | |
| `image.digest` | `""` | When set, the image is `repository@digest` and `tag` is ignored |
| `image.pullPolicy` | `IfNotPresent` | |
| `runtimeImage.repository` | `ghcr.io/techdufus/omp-operator-runtime` | Default agent image (`--default-runtime-image`) and the auth-gateway image |
| `runtimeImage.tag` | `""` (the chart's `appVersion`) | |
| `runtimeImage.digest` | `""` | As `image.digest` |
| `dashboard.url` | `""` | External dashboard URL; required with OIDC |
| `dashboard.oidc.issuerURL` | `""` | Setting it enables OIDC |
| `dashboard.oidc.clientID` | `""` | Required with `issuerURL` |
| `dashboard.oidc.clientSecret.existingSecret` | `""` | Required with `issuerURL` |
| `dashboard.oidc.clientSecret.key` | `client-secret` | Key in that Secret |
| `dashboard.oidc.scopes` | `[openid, email, profile, groups]` | |
| `dashboard.oidc.usernameClaim` | `email` | |
| `dashboard.oidc.groupsClaim` | `groups` | |
| `dashboard.basic.existingSecret` | `""` | Secret with key `htpasswd` (bcrypt entries only); setting it enables basic auth |
| `dashboard.access` | `[]` | Access rules, copied into the access file: `users`, `groups`, `namespaces`, `role` (`viewer` or `operator`) |
| `dashboard.apiTokens` | `[]` | Cloud API tokens: `{name, sha256}` or `{name, existingSecret, key}` (`key` defaults to `token`; the Secret key is mounted at `/etc/omp-operator/tokens/<name>/token`). Grant them in `access` as `token:<name>` |
| `cloud.maxUploadBytes` | `1073741824` | Largest repository bundle the cloud API accepts (`--cloud-max-upload-bytes`) |
| `hooks.publicURL` | `""` | External hooks URL (`--hooks-public-url`) |
| `actions.targetNamespaces` | `[]` | Extra namespaces actions may target; `["*"]` for any except the operator's |
| `persistence.storageClass` | `""` | Data PVC storage class (empty: cluster default) |
| `persistence.size` | `20Gi` | Data PVC size |
| `authGateway.enabled` | `false` | Install the auth-gateway and enable managed gateway tokens |
| `authGateway.clientsSecret` | `omp-auth-clients` | Name of the clients Secret |
| `authGateway.persistence.storageClass` | `""` | Gateway PVC storage class |
| `authGateway.persistence.size` | `1Gi` | Gateway PVC size |
| `rbac.agentNamespaces` | `[]` | Namespaces to serve; empty means cluster-wide |
| `resources` | requests 50m CPU and 128Mi, limit 512Mi memory | Operator container resources |
| `priorityClassName` | `""` | For the operator and the gateway |
| `nodeSelector` | `{}` | For the operator and the gateway |
| `tolerations` | `[]` | For the operator and the gateway |
