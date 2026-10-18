# OmpTrigger reference

`omp.techdufus.com/v1alpha1`, kind `OmpTrigger`, short name `omptrigger`, namespaced. `kubectl get omptrigger`
shows `Agent`, `Type` (`status.type`), `Ready` and `Age`.

How webhooks and schedules behave is described in [webhooks and schedules](../webhooks.md).

## spec

Exactly one of `webhook` and `schedule` is set.

| Field | Type | Default | Description |
|---|---|---|---|
| `agentRef.name` | string | required | The `OmpAgent` (same namespace) that runs the sessions |
| `enabled` | bool | `true` | `false` stops firing; webhooks answer 409 |
| `prompt` | string | required | Go `text/template`, `missingkey=error`. Data differs by trigger type, see below |
| `priority` | int 0-3 | computed: webhook 2 (1 when the event severity is `critical`), schedule 3 | Lower runs first among an agent's Pending sessions |
| `session.resume` | enum | `none` | `none`, `episode` (webhook triggers only) or `key` |
| `webhook` | [WebhookSpec](#webhook) | | |
| `schedule` | [ScheduleSpec](#schedule) | | |

### webhook

| Field | Type | Default | Description |
|---|---|---|---|
| `adapter` | enum | required | `alertmanager`, `grafana` or `generic` |
| `auth` | [auth](#auth) | required | |
| `alertFilter` | CEL string | computed: `!(alert.labels.?alertname.orValue('') in ['Watchdog','InfoInhibitor'])` | Over `alert`, returns bool. Alert adapters only |
| `filter` | CEL string | computed: `true` | Over `event`, returns bool |
| `mapping` | [mapping](#mapping) | | Required for `generic`; ignored by the other adapters |
| `grouping.episodeGap` | duration | `30m` | |
| `grouping.retriageAfter` | duration | `1h` | Minimum gap between sessions for new fingerprints of a group |
| `grouping.perGroupPerDay` | int | `2` | Minimum 1 |

CEL variables: `alert` (map: `labels`, `annotations`, `status`, `fingerprint`, `startsAt`), `event` (map: `source`,
`key`, `name`, `status`, `severity`, `labels`, `alerts`, `fingerprints`, `payload`), `payload` (any; in `mapping.events`
the whole body, in the other mapping expressions the single element). Optional types and the strings extension are
enabled; the cost limit is 100,000. A runtime error in a filter is false; in a mapping it drops the event.

#### auth

Exactly one of:

| Member | Fields |
|---|---|
| `bearer` | `secretRef: {name, key}` |
| `basic` | `secretRef: {name}`, a Secret with keys `username` and `password` |
| `hmac` | `secretRef: {name, key}`; `header` (default `X-Hub-Signature-256`); `prefix` (unset: `sha256=` for `X-Hub-Signature-256` and none for other headers; an explicit `""` means none); `timestampHeader` (when set the header carries Unix seconds and the signed text is `<timestamp>:<body>`); `maxSkew` (default `5m`) |

#### mapping

CEL over `payload`. All expressions are evaluated per event; see the [generic adapter](../webhooks.md#generic).

| Field | Result | Default |
|---|---|---|
| `events` | list | `[payload]` |
| `key` | string | required |
| `title` | string | `''` |
| `severity` | string | `'info'` |
| `status` | `"firing"` or `"resolved"` | `'firing'` |
| `fingerprint` | string | sha256 of the event's JSON |

### schedule

| Field | Type | Default | Description |
|---|---|---|---|
| `cron` | string | required | Exactly 5 fields. No descriptors, no `TZ=` or `CRON_TZ=` |
| `timeZone` | string | `UTC` | IANA name |
| `input` | string | | Available to the prompt as `.Input` |

### prompt data

| Trigger | Fields |
|---|---|
| webhook | `.Agent`, `.Trigger`, `.Session`, `.Event.Name`, `.Event.Key`, `.Event.Severity`, `.Event.Status`, `.Event.Labels`, `.Reason`, `.AlertCount`, `.Previous`, `.Alerts` (fenced JSON, up to 20 alerts), `.Payload` (fenced JSON, up to 64 KiB) |
| schedule | `.Agent`, `.Trigger`, `.Session`, `.Input`, `.Now` (RFC 3339 in the trigger's time zone) |

## status

| Field | Description |
|---|---|
| `type` | `webhook` or `schedule` |
| `conditions` | `Ready` |
| `hookPath` | `/hooks/<namespace>/<name>`, webhook triggers |
| `groups` | At most 100: `id`, `name`, `severity`, `episodeStart`, `lastNotified`, `resolvedAt`, `triagedFingerprints` (at most 50), `lastSession`, `lastSessionAt` |
| `lastFireAt`, `nextFireAt` | Schedule slots |

`Ready` is true when the agent exists, the unions hold, CEL programs compile and have the right result type, `mapping`
is present for `generic`, the prompt parses, the auth Secret and keys exist, the cron expression and time zone parse, and
`resume: episode` is not used on a schedule. Reasons: `AgentNotFound`, `InvalidSpec`, `InvalidExpression`,
`InvalidPrompt`, `MissingSecret`, `InvalidSchedule`. Re-checked every 5 minutes.
