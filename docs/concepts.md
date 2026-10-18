# Concepts

All kinds are namespaced, have a status subresource and live in group `omp.techdufus.com`, version
`v1alpha1`. Everything an object refers to by name (Secrets, ConfigMaps, agents, actions) is in the same
namespace, except Flux sources, which name their namespace.

| Kind | Short name | What it is |
|---|---|---|
| `OmpAgent` | `ompagent` | An agent definition: model, instructions, skills, workspace, credentials, MCP servers, actions, limits and notification sinks |
| `OmpTrigger` | `omptrigger` | What starts sessions for one agent: a webhook or a cron schedule, plus the prompt template |
| `OmpSession` | `ompsession` | One run of an agent: a prompt in, a verdict, report and transcript out. Created by triggers, or by you |
| `OmpAction` | `ompaction` | A bounded thing the agent may do, shown to it as a tool |

Field-by-field references: [OmpAgent](reference/ompagent.md), [OmpTrigger](reference/omptrigger.md),
[OmpSession](reference/ompsession.md), [OmpAction](reference/ompaction.md).

## The life of an event

1. A system posts to `/hooks/<namespace>/<trigger>`, or a schedule comes due.
2. For a webhook, the trigger's adapter turns the payload into events; the trigger's filter drops the
   uninteresting ones. Events with the same key form a **group**; a group's firing period is an
   **episode**. The operator decides per event whether to start a session, skip it as a duplicate,
   or stop because a budget is used up. See [webhooks](webhooks.md).
3. A session is created from the trigger's prompt template. It is `Pending` until the agent has no other
   session running and it heads the agent's queue. See [sessions](sessions.md).
4. The operator starts a pod. An init container prepares the workspace, skills, toolchain and continuity;
   the main container runs `omp --mode rpc`. The agent's tools are omp's built-ins you allowed, MCP
   servers, one tool per action, and the built-in `finish`.
5. The agent investigates and calls `finish` with a verdict, a title, a summary, a report and optionally
   proposed commands for an owner to run.
6. The session becomes `Succeeded` (a verdict was recorded) or `Failed`. The agent's sinks whose `on`
   list contains the result are notified, and the session stays visible in the dashboard and in the
   cluster until `sessionRetention` has passed.

## Phases, outcomes and verdicts

| Phase | Meaning |
|---|---|
| `Pending` | Waiting: for its agent to be Ready, for the gateway to pick up a new token, for other sessions to finish, for a toolchain sync, or for a retry delay |
| `Running` | A pod exists for the current attempt |
| `Succeeded` | The agent called `finish`; `status.verdict` is set |
| `Failed` | Anything else terminal; `status.error` says why |

`status.outcome` is how the latest attempt ended, reported by the in-pod driver: `completed`,
`retryable`, `auth` or `error`. It drives retries, see [sessions](sessions.md#retries).

| Verdict | Meaning, as the agent is asked to use it |
|---|---|
| `ok` | Nothing to do |
| `fixed` | The agent remediated the problem with an action |
| `needs_owner` | A human has to act |
| `inconclusive` | The agent could not tell |

`failed` is not a verdict. It is the name notification sinks use for a `Failed` session.

## Object names

- Webhook sessions: `<trigger>-<YYYYMMDD-HHMMSS>-<4 hex>` (UTC).
- Schedule sessions: `<trigger>-<YYYYMMDD-HHMM>`, the UTC slot. The same slot always yields the same
  name, so a slot is never run twice.
- Manual sessions: whatever you name them.
- Session pods: `<session>-<attempt>`. Token Secret: `<session>-token`.
- Managed gateway token Secret: `<agent>-omp-gateway`. Toolsync pods: `<agent>-toolsync-<unix seconds>`,
  with Secret `<agent>-toolsync-token`.
- `OmpAction` names match `^[a-z][a-z0-9-]{0,62}$`. The agent sees the action as a tool named after it
  with `-` replaced by `_`, so `restart-workload` becomes `restart_workload`.

## Labels

Labels use the prefix `omp.techdufus.com/`.

| Label | On | Value |
|---|---|---|
| `agent` | Sessions, pods | OmpAgent name |
| `trigger` | Webhook and schedule sessions | OmpTrigger name |
| `kind` | Sessions | `webhook`, `schedule` or `manual` |
| `group` | Webhook sessions | First 16 hex characters of the sha256 of the event key |
| `severity` | Webhook sessions | The event severity, sanitized to a valid label value |
| `continuity` | Sessions with a continuity key | First 16 hex characters of the sha256 of the key |
| `session` | Session pods | OmpSession name |
| `role` | Pods | `session` or `toolsync` |

Pods and operator-created Secrets also carry `app.kubernetes.io/managed-by: omp-operator`. These are the
labels to select on in NetworkPolicies and monitoring; see [security](security.md#network-policy).

Owner references: a session is owned by its OmpAgent and, when it has one, its OmpTrigger, so deleting an
agent deletes its sessions.

## Ready conditions

`OmpAgent`, `OmpTrigger` and `OmpAction` report a `Ready` condition whose `reason` says what is wrong.
They are re-checked every 5 minutes, because Secrets, ConfigMaps, Flux sources and actions are not
watched. Sessions of a not-Ready agent wait; webhooks of a not-Ready trigger answer 409.

| Kind | Reasons when not Ready |
|---|---|
| OmpAgent | `NamespaceNotAllowed`, `NoImage`, `InvalidSpec`, `MissingSecret`, `MissingConfigMap`, `MissingSource`, `ActionNotReady`, `NoGatewayToken`, `DuplicateName`, `InvalidCredential` |
| OmpTrigger | `AgentNotFound`, `InvalidSpec`, `InvalidExpression`, `InvalidPrompt`, `MissingSecret`, `InvalidSchedule` |
| OmpAction | `InvalidName`, `ReservedName`, `InvalidSpec`, `MissingSecret` |

`kubectl get ompagent,omptrigger,ompaction` shows Ready; `kubectl describe` shows the message.
Objects in a namespace outside `--agent-namespaces` are ignored and get no status.

## Defaults

Static defaults are in the CRD schema. A few defaults are computed by the operator when the field is
left empty, so the field stays empty on the object: the agent `image`, `model.gateway.allowedModels`,
MCP server `type`, trigger `priority`, `alertFilter`, `filter` and the `mapping` expressions, and the
action namespace `allow` list. Each is listed in the reference tables.
