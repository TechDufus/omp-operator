# OmpSession reference

`omp.techdufus.com/v1alpha1`, kind `OmpSession`, short name `ompsession`, namespaced. `kubectl get ompsession`
shows `Agent`, `Trigger`, `Phase`, `Verdict`, `Title` and `Age`.

See [sessions](../sessions.md) for how they run.

## spec

| Field | Type | Default | Description |
|---|---|---|---|
| `agentRef.name` | string | required | The `OmpAgent` that runs the session |
| `triggerRef.name` | string | | The creating `OmpTrigger`, if any |
| `prompt` | string | required | The text sent to omp |
| `priority` | int 0-3 | computed: 1 | Lower runs first among the agent's Pending sessions |
| `continuityKey` | string | | Sessions of one agent with the same key share omp history |
| `event` | `{key, groupId, name, severity}` | | Set on webhook sessions |
| `threadRef.name` | string | | The `OmpThread` this session is a sitting of. Its prompt is the first queued message; the sitting then takes messages from the thread until it idles out. See [OmpThread](ompthread.md) |

## status

| Field | Description |
|---|---|
| `phase` | `Pending`, `Running`, `Succeeded`, `Failed` |
| `attempts`, `retryAt`, `podName`, `startTime`, `completionTime` | Lifecycle |
| `workspaceRevision` | The Flux artifact revision or the git commit the workspace was prepared from |
| `outcome` | How the latest attempt ended: `completed`, `retryable`, `auth`, `error` |
| `verdict` | `ok`, `fixed`, `needs_owner`, `inconclusive` |
| `title` | At most 80 characters |
| `summary` | At most 3000 characters |
| `proposedCommands` | At most 10, each at most 500 characters |
| `error` | At most 1000 characters; why the session failed or the last attempt's failure |
| `actions` | At most 20 of `{action, target, ok, at, result (at most 300 characters)}` |
| `usage` | `inputTokens`, `outputTokens`, `cacheReadTokens`, `cacheWriteTokens`, `cost` (decimal string) |
| `continuity` | `restored` and `stored` |
| `conditions` | `Notified`, `WorkspaceReady` |

## Labels

All with the prefix `omp.techdufus.com/`. Webhook and schedule sessions carry them; a manual session carries
only what you set.

| Label | Value |
|---|---|
| `agent` | Agent name |
| `trigger` | Trigger name |
| `kind` | `webhook`, `schedule`, `manual` or `cloud` |
| `group` | First 16 hex of sha256 of the event key (webhook sessions) |
| `severity` | Event severity (webhook sessions) |
| `continuity` | First 16 hex of sha256 of `continuityKey` |
| `thread` | Thread name (cloud sittings) |

## Naming and ownership

| Kind of session | Name |
|---|---|
| Webhook | `<trigger>-<YYYYMMDD-HHMMSS>-<4 hex>` |
| Schedule | `<trigger>-<YYYYMMDD-HHMM>` (UTC slot) |
| Manual | Yours |

A session is owned by its `OmpAgent` and, when it has one, its `OmpTrigger`. It carries the finalizer
`omp.techdufus.com/cleanup`, which deletes its stored files (events, transcript, report) and, when no other
session of the agent has the same `continuity` label, the continuity tarball. It is deleted `completionTime` +
the agent's `sessionRetention` after it ended.

A sitting of a thread (`threadRef`) is named `<thread>-<n>` and controlled by its [OmpThread](ompthread.md),
which creates it. It leaves the continuity tarball to the thread's finalizer, because all sittings of a thread
share it.
