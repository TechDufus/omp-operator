# OmpThread reference

`omp.techdufus.com/v1alpha1`, kind `OmpThread`, short name `ompthread`, namespaced. `kubectl get ompthread`
shows `Agent`, `Phase` (`Working`, `Waiting` or `Idle`), `Turns`, `Latest` (the latest finished turn's state), `Title` and `Age`.

A thread is a conversation handed off from a local omp to a cloud agent. The cloud API creates it (with the
uploaded repository, setup and conversation) and appends turns to it; you rarely write one by hand. The agent
must set [`cloud`](ompagent.md#cloud).

## spec

| Field | Type | Default | Description |
|---|---|---|---|
| `agentRef.name` | string | required | The cloud-enabled `OmpAgent` that runs the thread |
| `owner` | string | required | The API principal that created it, for example `token:dev` |
| `title` | string | | First line of the first message, at most 80 characters |
| `model` | string | computed: the agent's `model.default` | `provider/id` used for every turn |
| `thinking` | enum | computed: the agent's `model.thinking` | `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max` |
| `repo.name` | string | required | Checkout directory name, for display |
| `repo.remoteURL` | string | | The raw remote URL configured on the user's machine. Empty: the whole history was uploaded |
| `repo.baseRef` | string | | Remote branch the base commit came from |
| `repo.baseCommit` | string | | Commit on the remote the uploaded delta builds on. Empty: a full-history upload |
| `repo.handoffCommit` | string | required | The user's state at handoff, including uncommitted changes |
| `repo.localBranch` | string | | Branch checked out on the user's machine |
| `paths.home` | string | computed: `/home/agent` | The user's `$HOME`, mirrored in the pod |
| `paths.workspace` | string | computed: `/workspace` | The repository root on the user's machine, mirrored in the pod |
| `author` | `{name, email}` | | Git identity of the cloud agent's commits |
| `secretFiles` | []string | | At most 32 workspace-relative paths. Secret `<thread>-secrets` holds them as `file-<i>` |
| `secretEnv` | []string | | At most 64 variable names. Secret `<thread>-secrets` holds them as `env.<NAME>` |

Paths that cannot be mirrored (not absolute, a system directory, equal paths, or a workspace above the home) are
stored empty, and the pod uses the defaults for both.

## status

| Field | Description |
|---|---|
| `phase` | `Waiting` while the running turn has an unanswered question, else `Working` while a turn is queued or running, else `Idle` |
| `turns`, `queued` | Turns in any state; turns waiting for a sitting |
| `activeSession` | The sitting serving the thread |
| `question` | The oldest unanswered question of the running turn: `{turn, id, text (at most 1000 characters), askedAt}`. Unset unless the phase is `Waiting` |
| `latest` | The newest done or failed turn: `{id, state, title, reply (at most 3000 characters), error, completedAt}` |
| `result` | The newest result: `{branch, head, base, filesChanged, insertions, deletions, pushed, pushError, updatedAt}`. `branch` is `omp-cloud/<thread>` |
| `lastActivityAt` | The latest of the thread's creation and turn timestamps |
| `conditions` | `Ready` (`False` with `AgentNotFound` when the agent is gone) |

A turn is `queued`, `running`, `done`, `failed` or `cancelled`. The turns themselves live in the operator's data
volume, not in the object; read them through the cloud API or the dashboard.

## Questions

A cloud agent can ask the user a question with omp's `ask` tool. Each question is stored in the turn log, on the turn
that asked it, and shown by the cloud API (`turns[].questions`, and `question` on the thread summary) and the
dashboard. While one is unanswered the thread is `Waiting` and `status.question` names the oldest.

A question has an `id` (1-based within its turn), an `ask` number (the `ask` call within the turn; the questions of
one call are answered together), the omp question `key`, `text` (at most 4000 bytes), an optional `header`, at most 20
`options` (`{label, description}`, labels unique within the question), `multi`, and `recommended` (an index into the
options). Free text is always allowed besides the options. A turn holds at most 20 questions.

An answer is `selected` (option labels), free text (at most 4000 bytes), or both, from the cloud API
(`POST .../turns/{turn}/questions/{id}/answer`). A question without `multi` takes at most one label. It records
`answeredBy` (the principal) and `answeredAt`. The sitting continues once every question of the ask is answered.

A question nobody answered within `cloud.questionTimeout` expires: the agent is told to continue with its best
judgment, say what it assumed, and ask again at the end of its turn if it still matters. A stop expires every open
question of the running turn at once. Expired questions have `expired: true` and `answeredBy` `timeout` or `stop`. A
turn that is redone in a new sitting after its sitting died loses its questions.

## Sittings

A thread runs in sittings: `OmpSession`s named `<thread>-1`, `<thread>-2`, ... with `kind=cloud`. The thread
reconciler creates the next one whenever a turn is queued and no sitting is active. A sitting waits for messages up
to the agent's `cloud.idleTimeout`, takes no turn that could end past `cloud.maxSitting`, then saves the workspace
and the conversation and ends `Succeeded` without a verdict. A new sitting restores both.

- A sitting that ends while a turn is running puts that turn back in the queue, and the next sitting redoes it.
- A sitting that fails for good (attempts exhausted, deadline exceeded) fails every queued and running turn with
  `cloud session failed: <reason>`, and the thread starts a new sitting only for turns queued afterwards.
- Sittings of a thread never run at the same time, and count against the agent's `limits.maxConcurrentSessions`.

## Notifications

Each finished turn notifies the agent's sinks once. The `on` filter sees `fixed` when the turn changed files, `ok`
when it did not, and `failed` for a failed turn. The title is `<thread title>: <turn title>` (`turn <n>` without
one), the summary the first 800 characters of the reply or the error, and the link
`<dashboard-url>/threads/<namespace>/<thread>`. Cancelled turns are not notified. See
[notifications](../notifications.md).

## Retention and deletion

An Idle thread is deleted `cloud.threadRetention` after its last activity. The thread has the finalizer
`omp.techdufus.com/cleanup`, which deletes its stored files (source and result bundles, setup archive, turn log) and
the conversation tarball. Its sittings and its `<thread>-secrets` Secret are owned by it and go with it.
