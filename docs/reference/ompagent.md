# OmpAgent reference

`omp.techdufus.com/v1alpha1`, kind `OmpAgent`, short name `ompagent`, namespaced. `kubectl get ompagent`
shows `Ready`, `Model` (`spec.model.default`) and `Age`.

"Computed default" means the operator applies the default when it uses the field and the field stays empty on
the object. "Union" means exactly one member must be set; the CRD enforces it and the reconciler re-checks it
(`InvalidSpec`).

For background see [concepts](../concepts.md), [sessions](../sessions.md) and [model access](../model-access.md).

## spec

| Field | Type | Default | Description |
|---|---|---|---|
| `image` | string | computed: the operator's `--default-runtime-image` | Runtime image. Must contain `omp`, `omp-operator-driver` and `git` (and `mise` with `toolchain`). Empty with no operator default: `NoImage` |
| `serviceAccountName` | string | unset | Set: pods run as it with its token mounted, `KUBECONFIG` set. Unset: the namespace's `default` account, no token |
| `model` | [ModelSpec](#model) | required | |
| `instructions` | string | | Appended to omp's system prompt |
| `tools.allow` | []string | `[read, grep, glob, bash, todo]` | omp built-in tools the agent may use. Empty passes `--no-tools` |
| `skills` | [][SkillSource](#skills) | | At most 32. Copied in order |
| `workspace` | [WorkspaceSpec](#workspace) | empty directory | |
| `credentials` | [][Credential](#credentials) | | At most 64 |
| `mcpServers` | [][MCPServer](#mcpservers) | | At most 32 |
| `actions` | [{name}] | | `OmpAction`s of this namespace. Each listed once, each must be Ready |
| `toolchain` | `{disable: []string}` | unset (off) | Serve the workspace's `mise` tools from the operator. `disable` lists mise tool short names to skip. `toolchain: {}` turns it on |
| `limits` | [LimitsSpec](#limits) | `{}` | |
| `notify` | [][NotifySink](#notify) | | At most 16 |
| `sessionRetention` | duration | `720h` | How long a finished session is kept |
| `pod` | [PodOverrides](#pod) | | Merged into the main container of session pods |

### model

| Field | Type | Default | Description |
|---|---|---|---|
| `default` | string | required | `provider/id`, for example `anthropic/claude-sonnet-5-5` |
| `small` | string | = `default` | `provider/id` for small tasks (omp roles `smol` and `tiny`) |
| `thinking` | enum | `medium` | `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max` |
| `gateway` | object | unset | Route model calls through an omp auth-gateway. Unset: provider keys come from `credentials` |
| `gateway.url` | string | required in `gateway` | Absolute http(s) base URL, for example `http://omp-auth.omp-system.svc:4000` |
| `gateway.tokenSecretRef` | `{name, key}` | unset | Your own gateway token. Unset: an operator-managed token. See [model access](../model-access.md) |
| `gateway.allowedModels` | []string | computed: `["<provider of default>/*"]` | Model patterns the managed token may use |

### skills

Each entry is a union of `git`, `configMap`, `fluxSource`.

| Member | Fields |
|---|---|
| `git` | `url` (required), `ref` (branch or tag; empty means the default branch), `path` (default `skills`), `auth` ([GitAuth](#gitauth)) |
| `configMap` | `name`. Each key `<skill>.md` becomes `<skill>/SKILL.md` |
| `fluxSource` | `kind` (`GitRepository` or `OCIRepository`), `namespace`, `name` (all required), `path` (default `skills`) |

A later skill with the same name replaces an earlier one.

### workspace

A union of `git` and `fluxSource`, plus `links`.

| Field | Type | Default | Description |
|---|---|---|---|
| `git.url` | string | required | |
| `git.ref` | string | default branch | Branch or tag |
| `git.depth` | int | `1` | Minimum 1 |
| `git.auth` | [GitAuth](#gitauth) | none | |
| `fluxSource` | `{kind, namespace, name}` | | `kind` is `GitRepository` or `OCIRepository`. The session waits for the source's artifact |
| `links` | [{path, target}] | | At most 64 symlinks. `path` is relative to the workspace and must not contain `..`; `target` is absolute |

### gitAuth

A union of the SSH pair and a token.

| Field | Description |
|---|---|
| `sshKeySecretRef` + `knownHostsSecretRef` | `{name, key}` each, set together |
| `tokenSecretRef` | `{name, key}` of an access token for HTTPS remotes |
| `username` | Default `x-access-token`; the HTTP basic user paired with the token |

### credentials

| Field | Description |
|---|---|
| `name` | DNS label (at most 63 characters), unique in the agent. Identifies the credential in the prompt |
| `description` | Required. What it is for; shown to the agent |
| `secretRef` | `{name, key}` in this namespace |
| `env` | Environment variable name, exactly one of `env` and `file` |
| `file` | Absolute path the value is mounted at |

Reserved: `env` named `HOME`, `PATH`, `KUBECONFIG` or starting with `OMP_` or `MISE_`; a `file` under `/workspace`,
`/home/agent`, `/tmp`, `/opt/mise` or `/var/run/omp-operator`. Two credentials cannot share an `env` name or a
file path (they may share a directory). Violations: `InvalidCredential`. Duplicate credential, MCP server or
action names: `DuplicateName`.

### mcpServers

| Field | Description |
|---|---|
| `name` | DNS label, unique |
| `type` | `stdio`, `http` or `sse`. Computed default: `stdio` when `command` is set, `http` when `url` is |
| `command` | stdio: `[0]` is the binary (it must exist in the image), the rest are arguments |
| `env` | stdio: at most 64 `{name, value}` or `{name, secretRef}` |
| `headers` | http and sse: at most 32 `{name, value}` or `{name, secretRef}` |
| `url` | http and sse: absolute http(s) URL |

A stdio server takes no `url` or headers; an http or sse server takes no `command` or `env`. Secret values reach
omp through `OMP_MCP_<n>_<m>` environment variables of the main container and are redacted like credentials.

### limits

| Field | Default | Description |
|---|---|---|
| `sessionTimeout` | `30m` | Time one attempt may run (passed to omp as `--max-time`; the pod deadline is this plus 10 minutes) |
| `maxAttempts` | `4` | Attempts before a retryable session fails. Minimum 1 |
| `retryDelay` | `15m` | Wait before a retry |
| `eventSessionsPerDay` | `12` | Webhook-created sessions of this agent in a rolling 24 hours. Minimum 1 |
| `nonCriticalSessionsPerDay` | `8` | The part of those whose severity is not `critical`. Minimum 1 |

### notify

| Field | Description |
|---|---|
| `type` | `ntfy`, `slack`, `discord` or `webhook`; the matching member must be set |
| `on` | Results that notify the sink: `ok`, `fixed`, `needs_owner`, `inconclusive`, `failed`. Default all |
| `ntfy` | `url` (default `https://ntfy.sh`), `topicSecretRef` |
| `slack` | `webhookURLSecretRef` |
| `discord` | `webhookURLSecretRef` |
| `webhook` | `urlSecretRef`, `hmacSecretRef` (optional, signs the body into `X-Omp-Signature-256`) |

See [notifications](../notifications.md).

### pod

| Field | Description |
|---|---|
| `resources` | Resource requirements of the main container |
| `env` | Added to the main container's environment |
| `volumes` | Added to the pod |
| `volumeMounts` | Added to the main container |
| `nodeSelector`, `tolerations` | Scheduling |
| `labels` | Added to the pod |

## status

| Field | Description |
|---|---|
| `observedGeneration` | Spec generation last reconciled |
| `conditions` | `Ready` and `ToolchainReady` |
| `toolchain` | `hash`, `syncedAt`, `stale`, `lastAttemptAt`, `lastError` |
| `notices` | When each rate-limited notice (`auth`, `cap:...`, `toolchain`, `workspace`) was last sent |

`Ready` is true when: the namespace is served; an image is resolvable; every union holds; every referenced Secret,
ConfigMap and Flux source exists; every listed action exists and is Ready; a gateway agent has a token
(`tokenSecretRef`, or managed tokens enabled); credential and MCP names are unique and
credentials follow the reserved-name rules. Reasons: `NamespaceNotAllowed`, `NoImage`, `InvalidSpec`, `MissingSecret`,
`MissingConfigMap`, `MissingSource`, `ActionNotReady`, `NoGatewayToken`, `DuplicateName`, `InvalidCredential`.
Re-checked every 5 minutes.

The agent has the finalizer `omp.techdufus.com/cleanup`, which removes its clients entry, its stored toolchain and
its continuity data.
