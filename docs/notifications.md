# Notifications

`OmpAgent.spec.notify` lists sinks, up to 16. When a session ends, the operator sends the result to every
sink whose `on` list contains it. Every sink reads its destination from a Secret in the agent's namespace, so
URLs and topics (which are often secrets themselves) never appear in the object or in error messages.

```yaml
spec:
  notify:
    - type: ntfy
      on: [fixed, needs_owner, failed]
      ntfy:
        url: https://ntfy.sh                   # default
        topicSecretRef: {name: triage-notify, key: ntfy-topic}
    - type: slack
      slack: {webhookURLSecretRef: {name: triage-notify, key: slack-url}}
    - type: discord
      discord: {webhookURLSecretRef: {name: triage-notify, key: discord-url}}
    - type: webhook
      webhook:
        urlSecretRef: {name: triage-notify, key: hook-url}
        hmacSecretRef: {name: triage-notify, key: hook-hmac}   # optional
```

`type` must match the member that is set. `on` is a list of `ok`, `fixed`, `needs_owner`, `inconclusive` and
`failed`; the default, and an empty list, mean all five. A session's result is its verdict, or `failed` when the
session is `Failed`.

## Delivery

- Each sink is tried up to 3 times, 10 seconds apart, with a 15 second timeout per request. A non-2xx
  response or a transport error is retried.
- Sinks are independent: one failing sink does not stop the others. The final outcome per sink is counted in
  `omp_operator_notifications_total{type,result=ok|error}`.
- A session's delivery is not retried later. The `Notified` condition records `NotifyFailed` with the error.
- Links point to `<dashboard-url>/sessions/<namespace>/<name>` when the operator has `--dashboard-url`
  (chart `dashboard.url`), and are omitted otherwise.

## Payloads

| Sink | Payload |
|---|---|
| `ntfy` | JSON to the server URL: `{topic, title, message, priority, tags: ["eye"], click}`. `message` is the summary cut to 3,800 bytes. `priority` by verdict: `ok` 2, `fixed` 3, `inconclusive` 3, `needs_owner` 4, `failed` 3. `click` is the session link |
| `slack` | `{"text": "*<title>*\n<summary>\n<link>"}`, cut to 3,000 characters |
| `discord` | `{"content": "**<title>**\n<summary>\n<link>"}`, cut to 2,000 characters |
| `webhook` | `{session, agent, trigger, verdict, title, summary, proposedCommands, actions, url}`, with `session` as `<namespace>/<name>`. With `hmacSecretRef`, the header `X-Omp-Signature-256: sha256=<hex HMAC-SHA256 of the body>` is added |

The title is the agent's own `finish` title. If there is none (a failed session), it is
`<agent>: session <name> failed`, and the summary is the session's error.

A cloud agent's sinks are told about each finished thread turn instead of about its sittings. The result
is `failed` for a failed turn, `fixed` when the turn changed files and `ok` otherwise; the webhook payload's
`session` is `<namespace>/<thread>`, the title `<thread title>: <turn title>`, the summary the first 800 characters
of the reply (or the error), and the link `<dashboard-url>/threads/<namespace>/<thread>`.

Verify a webhook sink's signature by computing the HMAC-SHA256 of the raw request body with the secret and
comparing it, in constant time, to the hex string after `sha256=`.

## Notices

Besides results, the operator sends rate-limited **notices** to *every* sink of the agent, regardless of
`on`. They link to the agent in the dashboard. A notice is sent at most once per period for each key, tracked
in `OmpAgent.status.notices`; an agent without sinks sends nothing.

| Notice | Sent when | Period |
|---|---|---|
| `auth` | A session attempt ended with outcome `auth`: `<agent>: model login missing or expired`. When the agent uses a gateway, the text includes the gateway login commands | 6 hours |
| `cap:<trigger>:<date>` | A webhook event was skipped because a session budget is used up: `<agent>: session limit reached` | Once per day per trigger |
| `toolchain` | A toolchain sync failed: `<agent>: toolchain sync failed` | 6 hours |
| `workspace` | A Flux workspace has no artifact yet: `<agent>: workspace source not ready` | 6 hours |
