# Sessions

An `OmpSession` is one run of an agent. Triggers create them; you can create them by hand:

```yaml
apiVersion: omp.techdufus.com/v1alpha1
kind: OmpSession
metadata: {name: hello, namespace: platform}
spec:
  agentRef: {name: alert-triage}
  prompt: Report whether $DEMO_TOKEN is set (yes or no only), then call finish with verdict ok.
```

`kubectl get ompsession` shows agent, trigger, phase, verdict and title. Fields: see the
[OmpSession reference](reference/ompsession.md).

## Lifecycle

```text
Pending ──► Running ──► Succeeded
   ▲           │
   └─ retry ◄──┤──► Failed
```

A session is `Pending` until all of these hold, checked in this order:

1. `status.retryAt`, if set after a retryable failure, has passed.
2. The agent is Ready. Otherwise the session waits, rechecking every 60 seconds; after 30 minutes it
   fails with `agent not ready: <reason>`.
3. For an agent with a [managed gateway token](model-access.md#point-an-agent-at-it), the running operator
   has not written the gateway clients Secret within the last 2 minutes.
4. No toolsync pod of the agent is unfinished, and no other session of the same cloud thread is `Running`.
5. The agent has a free slot: it has fewer than `limits.maxConcurrentSessions` `Running` sessions, and the
   session ranks within the free slots among the Pending sessions that could start now: the lowest `priority`
   number first, then the oldest. A session waiting for its `retryAt` does not take a slot. Otherwise it
   rechecks every 10 seconds.
6. A [Flux workspace](#workspace) has an artifact (otherwise `WorkspaceReady=False`, retried every 60
   seconds, failing after 30 minutes). A cloud sitting skips the agent's workspace, but still waits for Flux
   skill sources.
7. A needed [toolchain sync](#toolchain) has been attempted (a cloud sitting skips it).

Then the operator creates the token Secret `<session>-token` (32 random bytes, hex, owned by the session,
reused across attempts) and pod `<session>-<attempt>`, and the session becomes `Running`, with
`status.attempts` incremented and `startTime` and `podName` set.

**The agent runs `limits.maxConcurrentSessions` sessions at a time** (default 1, at most 20); the free slots go
to the first Pending sessions in queue order. The sittings of one cloud thread never run together. For more
parallelism than that, define more agents.

Priorities run from 0 to 3, lower first. Defaults: manual sessions 1, critical webhook events 1, other
webhook events 2, schedules 3.

A session is deleted `sessionRetention` (default `720h`) after it completed. Its finalizer
`omp.techdufus.com/cleanup` removes its files from the operator's data volume, and removes the continuity
tarball when no other session of the agent uses the same key.

## The pod

One pod per attempt, `restartPolicy: Never`, `enableServiceLinks: false`, `activeDeadlineSeconds` =
`limits.sessionTimeout` + 10 minutes. Two containers use the agent's image:

- Init container `prepare` (`omp-operator-driver prepare`) fetches the session's plan from the operator
  and builds the environment: workspace, skills, toolchain, continuity, kubeconfig, links. It then
  reports `workspaceRevision` and the workspace's toolchain hash.
- Main container `agent` (`omp-operator-driver run`) writes omp's configuration, starts
  `omp --mode rpc` in the workspace, sends the prompt, serves action calls, and uploads the transcript.

Volumes: emptyDirs at `/workspace` (2Gi), `/opt/mise` (2Gi), `/home/agent` (1Gi), `/tmp` (1Gi) and
`/var/run/omp-operator`; the session token Secret at `/var/run/omp-operator/token` in both containers.
Only the init container sees git credentials and ConfigMap skill sources; only the main container sees
credentials, MCP secrets, the gateway token and the `KUBECONFIG`. Details and the security context are in
[security](security.md#session-pods).

Environment in both containers: `OMP_OPERATOR_SESSION=<ns>/<name>`, `OMP_OPERATOR_API`,
`OMP_OPERATOR_WORKSPACE=/workspace`, `HOME=/home/agent`, `PATH` with `/opt/mise/data/shims` first (so the
tools a [toolchain](#toolchain) provides resolve), and mise settings that turn automatic installs
off (`MISE_AUTO_INSTALL=false` and friends, `MISE_TRUSTED_CONFIG_PATHS=/workspace`, `MISE_DISABLE_TOOLS` from
`toolchain.disable`). Cloud sittings set `HOME`, `OMP_OPERATOR_WORKSPACE` and `MISE_TRUSTED_CONFIG_PATHS` to the
thread's mirrored paths.

omp is started as:

```text
omp --mode rpc --no-ui --approval-mode yolo --tools <tools.allow> --max-time <sessionTimeout> \
    --session-dir /home/agent/session --append-system-prompt /tmp/system-prompt.md
```

With an empty `tools.allow` the driver passes `--no-tools`. omp's own interactive approvals are off
(`yolo`), so the boundary of what the agent can do is the tool allow-list, its credentials, its service
account and its actions. The generated `config.yml` maps omp's model roles to `model.default` (roles
`default` and `task`) and `model.small`, falling back to `default` (roles `smol` and `tiny`), and sets
`defaultThinkingLevel` from `model.thinking`.

The system prompt appended to omp's is, in order: `spec.instructions`, an *Available credentials* section
(`- <name>: <description> (environment variable $<env>)` or `(file <path>)`; never values), and an
*Actions you may call* section (`<tool>: <description>`). Every action and the built-in `finish` are
registered as top-level tools.

### Finishing

The agent ends a session by calling `finish`:

| Argument | Limit |
|---|---|
| `verdict` | `ok`, `fixed`, `needs_owner` or `inconclusive` |
| `title` | 1-80 characters |
| `summary` | 1-3000 characters |
| `report` | 1-20000 characters, stored as the session's report, shown in the dashboard |
| `proposed_commands` | At most 10 commands of at most 500 characters |

The first valid call wins; a second returns `finish already recorded`. If omp ends its turn without
calling `finish`, the driver sends one reminder; if there is still none, the session fails with `agent ended
without calling finish`.

The driver also reports progress events (the agent's tool calls, omp notices and retries; the newest 5000
lines are kept and the dashboard shows the last 200), the final token usage and cost (`status.usage`),
and an HTML transcript (at most 64 MiB).

## Retries

How an attempt ended decides the session's fate. The pod reports an `outcome`:

| Driver outcome | When |
|---|---|
| `auth` | The model endpoint answered HTTP 401 or 403 |
| `retryable` | omp sent no ready frame within 10 minutes; omp exited before finishing its turn; the provider answered HTTP 429 or 5xx |
| `error` | The session timeout (`session timed out`), an aborted turn, or anything else |
| `completed` | The turn ended normally |

| Situation | Result |
|---|---|
| A verdict was recorded | `Succeeded` |
| `completed`, no verdict | `Failed`: `agent ended without calling finish` |
| `error` | `Failed`: `agent reported an error`, with the driver's message in `status.error` |
| `retryable` | Back to `Pending` with `retryAt` = now + `limits.retryDelay` while `attempts < limits.maxAttempts`, else `Failed`: `attempts exhausted (N): ...` |
| `auth` | The same, plus the `auth` notice and the auth-failure metric |
| The pod hit its deadline | `Failed`: `session timed out: pod deadline exceeded` |
| The pod ended or vanished without reporting | Retried as `retryable`, with the pod's termination detail |
| The pod is not `Running` within 10 minutes | The pod is deleted and the attempt is retried |

A recorded `finish` always wins: the session ends `Succeeded` even if the pod failed afterwards.

When a session reaches `Succeeded` or `Failed`, the operator sets `completionTime`, records
`omp_operator_sessions_total` and `omp_operator_session_duration_seconds`, notifies the
[sinks](notifications.md) whose `on` includes the verdict (or `failed`), and sets the `Notified`
condition. A failed delivery is recorded in that condition and not retried.

## Workspace

`spec.workspace` on the agent is the agent's working directory; absent means an empty directory. Exactly
one source:

- `git`: a shallow `git clone --depth <depth>` (default 1), with `--branch <ref>` when `ref` is set (branch
  or tag). `auth` is either an SSH key with a known_hosts file, cloned with strict host key checking, or a
  token (with `username`, default `x-access-token`) sent as an HTTP basic `Authorization` header. The
  credentials are mounted only in the init container and are not passed on the command line.
- `fluxSource`: a Flux `GitRepository` or `OCIRepository` (`kind`, `namespace`, `name`). The operator
  waits for the source's `status.artifact`. The init container downloads the artifact (5-minute timeout, at
  most 1 GiB), checks its sha256 digest and extracts it, refusing absolute paths, `..` and links that
  escape.

`links` creates symlinks (`path` relative to the workspace, `target` absolute), for example to expose
a mounted volume. `<workspace>/.omp/skills` is loaded by omp natively. The revision actually used is stored
in the session's `status.workspaceRevision`.

## Skills

`spec.skills` lists sources copied in order into `~/.omp/agent/skills/<skill>/SKILL.md`; a later skill of the
same name replaces an earlier one. Each `SKILL.md` needs omp's `description` front matter, and skills are one
directory deep.

- `git`: `<path>/*/SKILL.md` (path default `skills`), with optional `ref` and `auth` as above.
- `configMap`: each key `<skill>.md` becomes `<skill>/SKILL.md`.
- `fluxSource`: `<path>/*/SKILL.md` of a `GitRepository` or `OCIRepository` artifact (path default `skills`).

## Toolchain

An agent with `spec.toolchain` (an empty object enables it; `disable` lists mise tool short names to skip)
gets the workspace's `mise.toml` tools without installing them in every session. The operator syncs them once:

1. A *toolsync* pod `<agent>-toolsync-<unix seconds>` (same image, `prepare` as init container, no
   service-account token, 15-minute deadline) runs `mise install --yes` in the workspace, and uploads the
   `/opt/mise/data` tarball to the operator under a hash of `mise.toml`, `mise.lock` and `disable`. Without
   a `mise.toml` it just reports an empty hash.
2. Session pods download the stored toolchain into `/opt/mise` and run with automatic installs disabled.
3. When a session's workspace has a different hash than the stored one, the agent's
   `status.toolchain.stale` is set; the *next* session waits for a fresh sync first. A toolsync pod
   starts when the toolchain was never synced or is stale, and the last attempt is older than 6 hours.

If a sync fails, `ToolchainReady=False` is set with the error, the agent's sinks get the `toolchain` notice
(at most every 6 hours), and sessions continue with whatever toolchain is stored. The operator keeps the
newest tarball and the previous one for each agent.

## Continuity

By default every session starts a fresh omp session. Sessions of one agent that share a
`spec.continuityKey` share omp's conversation history. Triggers set the key through
[`session.resume`](webhooks.md#session-priority-and-continuity); on a manual session you set it yourself.

When a key is set, `prepare` downloads the stored history (none yet is fine) into `/home/agent/session`, the
driver opens it with omp's `open_session` before the prompt, and after the turn uploads the directory as
`continuity/<namespace>/<agent>/<sha16 of key>.tar.gz`. History over 64 MiB is not stored (an event notes
it). `status.continuity.restored` and `stored` report what happened. With `resume: episode`, a later alert
in the same episode of a group finds the agent's earlier conversation, so the prompt can be short.

## Status

| Field | Owner | Meaning |
|---|---|---|
| `phase`, `attempts`, `retryAt`, `podName`, `startTime`, `completionTime`, `conditions` | Session controller | Lifecycle |
| `outcome`, `verdict`, `title`, `summary`, `proposedCommands`, `error`, `actions`, `usage`, `workspaceRevision`, `continuity` | The in-pod driver, through the operator's API | Result |

`status.actions` lists the agent's action calls (action, target, ok, time, a 300-character result; at most 20).
`status.usage` has input, output, cache-read and cache-write tokens and the cost as a decimal string.
Conditions: `Notified`, `WorkspaceReady`.
