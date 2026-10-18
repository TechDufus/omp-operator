# OmpAction reference

`omp.techdufus.com/v1alpha1`, kind `OmpAction`, short name `ompaction`, namespaced. `kubectl get ompaction` shows
`Type` (`status.type`), `Ready` and `Age`.

Behaviour, examples and the trust model are in [actions](../actions.md).

## spec

Exactly one of the four types is set.

| Field | Type | Default | Description |
|---|---|---|---|
| `description` | string | required | The tool description the model sees |
| `maxPerSession` | int | `3` | Calls of this action per session. Minimum 1 |
| `kubernetesRolloutRestart` | object | | See below |
| `fluxReconcile` | object | | See below |
| `alertmanagerSilence` | object | | See below |
| `httpRequest` | object | | See below |

### kubernetesRolloutRestart

| Field | Type | Default | Description |
|---|---|---|---|
| `kinds` | []enum | `[Deployment]` | `Deployment`, `StatefulSet`, `DaemonSet` |
| `namespaces.allow` | []string | computed: the action's own namespace | `*` matches any |
| `namespaces.deny` | []string | | Never allowed |
| `cooldown` | duration | `6h` | Minimum time between successful restarts of one target |
| `waitTimeout` | duration | `5m` | How long to wait for the rollout |

Agent arguments: `kind`, `namespace`, `name`, `reason` (1-300 characters).

### fluxReconcile

| Field | Type | Default | Description |
|---|---|---|---|
| `kinds` | []enum | `[Kustomization, HelmRelease, GitRepository, OCIRepository]` | May also include `HelmRepository` |
| `namespaces` | as above | as above | |
| `waitTimeout` | duration | `5m` | |

Agent arguments: `kind`, `namespace`, `name`, `with_source` (bool), `reason`.

### alertmanagerSilence

| Field | Type | Default | Description |
|---|---|---|---|
| `url` | string | required | Absolute http(s) base URL, no userinfo, query or fragment |
| `authSecretRef` | `{name, key}` | | Optional bearer token |
| `maxDuration` | duration | `24h` | Longest silence |
| `protectedAlertnames` | []string | `[Watchdog, InfoInhibitor]` | Never silenceable |

Agent arguments: `matchers` (1-8 `{name, value}`, exactly one on `alertname`), `minutes` (15 to `maxDuration`), `reason`.

### httpRequest

| Field | Type | Default | Description |
|---|---|---|---|
| `method` | enum | required | `GET`, `POST`, `PUT`, `PATCH`, `DELETE` |
| `url` | string | required | Go template over `.Args` |
| `allowedURLPrefix` | string | required | Absolute http(s) URL, no userinfo, query or fragment, that the rendered URL must stay under |
| `headers` | [{name, value or secretRef}] | | At most 32 |
| `body` | string | | Go template over `.Args`; functions `json` and `urlquery` |
| `parameters` | object | required | JSON Schema with `type: object` validating the agent's arguments |
| `responseMaxBytes` | int | `65536` | Response cut length. Minimum 1 |

## status

| Field | Description |
|---|---|
| `type` | The member set: `kubernetesRolloutRestart`, `fluxReconcile`, `alertmanagerSilence` or `httpRequest` |
| `conditions` | `Ready` |

`Ready` is true when the name is valid and not reserved, exactly one type is set, templates parse, the JSON Schema
compiles, URLs are valid and referenced Secrets exist. Reasons: `InvalidName`, `ReservedName`, `InvalidSpec`,
`MissingSecret`. Re-checked every 5 minutes.

## Reserved names

An `OmpAction` name must match `^[a-z][a-z0-9-]{0,62}$`. The agent sees the action as a tool whose name is the
action name with `-` replaced by `_`; that tool name must not be one of these, which are the tool names of the
pinned omp plus the operator's `finish`:

`ask`, `ast_edit`, `ast_grep`, `bash`, `browser`, `checkpoint`, `context_notes`, `debug`, `edit`, `eval`,
`exit_plan_mode`, `find`, `finish`, `generate_image`, `github`, `glob`, `goal`, `grep`, `ida`, `init_experiment`,
`irc`, `learn`, `log_experiment`, `lsp`, `manage_skill`, `memory_edit`, `new_context`, `read`, `recall`, `reflect`,
`report_finding`, `resolve`, `retain`, `rewind`, `run_experiment`, `security_scan`, `submit_result`, `task`,
`think`, `todo`, `update_notes`, `wait`, `web_search`, `write`, `yield`.

(So the action named `web-search` is reserved because it would become `web_search`.)
