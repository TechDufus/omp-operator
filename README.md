# omp-operator

A Kubernetes operator that runs [omp](https://github.com/can1357/oh-my-pi) (oh-my-pi) agents on
webhooks and schedules, and as cloud agents that you hand a local omp conversation to.

You describe an agent as a custom resource: its model, instructions, skills, workspace, credentials, MCP
servers and the actions it may take. You describe what wakes it up as a trigger: an Alertmanager,
Grafana or generic JSON webhook, or a cron schedule. When a trigger fires, the operator starts one pod
per session that drives `omp --mode rpc`, lets the agent investigate, and records a verdict, a report
and a transcript. The agent can act only through actions you define, and the operator reports the
result to ntfy, Slack, Discord or a webhook and shows it in a read-only dashboard.

A cloud agent works the other way round. In your local omp, `/cloud <task>` ships the repository (with your
uncommitted changes), the conversation, an allowlist of secrets and your omp setup (settings, extensions,
skills, `AGENTS.md`, memory) to an agent on the operator. It works asynchronously with the same omp, the same
paths and every built-in tool, and returns its work as a branch `omp-cloud/<thread>`. See
[cloud agents](docs/cloud.md).

v0.1 was the core platform and v0.2 adds cloud agents. GitHub mode (a robomp-style PR agent), attaching to a
running cloud agent, and launching or chatting with agents from the dashboard are planned for later
releases.

## What you get

- **Five CRDs** in group `omp.techdufus.com/v1alpha1`: `OmpAgent`, `OmpTrigger`, `OmpSession`,
  `OmpAction` and `OmpThread`.
- **Webhook triggers** with adapters for Alertmanager, Grafana and arbitrary JSON, bearer, basic or HMAC
  authentication, CEL filters, grouping into episodes, dedupe, and per-group, per-agent and
  non-critical daily budgets.
- **Schedules** with 5-field cron expressions and IANA time zones.
- **Sessions** that run up to `limits.maxConcurrentSessions` at a time per agent (default 1), with
  priorities, retries, an optional continuity of omp history across sessions (per episode or per key), and a
  toolchain of workspace tools cached on the operator.
- **Actions**: rollout restart, Flux reconcile, Alertmanager silence and a schema-checked HTTP request.
  Each is bounded by the policy in its `OmpAction`.
- **Cloud agents**: `/cloud <task>` from a local omp (the `omp-cloud` extension) hands the conversation,
  repository state, secrets you list and your omp setup to an agent that runs in a warm pod with an idle
  timeout and restores its state when it has to start again. A bearer-token [API](docs/reference/cloud-api.md)
  with viewer and operator roles, results as a pushed or pullable branch, and `/cloud resume` to bring the
  conversation back.
- **Notifications** to ntfy, Slack, Discord and signed webhooks, plus rate-limited notices when
  something needs the owner.
- **A dashboard** with OIDC sign-in or a basic-auth fallback, per-namespace access rules and no write
  operations (cloud threads have a page too).
- **Model access** through an omp auth-gateway shipped in the chart (it can hold a Claude or ChatGPT
  subscription login, so pods never see it), or through provider API keys passed as credentials.
- **Prometheus metrics** for sessions, webhook events, actions, notifications and storage.

## How it fits together

```text
 Alertmanager / Grafana / any JSON                         ┌──────────────────────────────┐
 webhook  ──► POST /hooks/<ns>/<trigger> ──┐               │ omp-operator (one Deployment)│
 cron schedule ────────────────────────────┤  OmpTrigger   │  controllers  hooks :8082    │
                                           ▼               │  dashboard :8080  API :8083  │
                              OmpSession (Pending) ──────► │  metrics :9090  data PVC     │
                                                           └──────┬───────────────────────┘
                                                                  │ creates, per session
                                                                  ▼
          ┌──────────── session pod ─────────────┐        OmpAction policy
          │ init  prepare: workspace, skills,    │◄────── (restart, Flux, silence, HTTP)
          │       toolchain, continuity          │
          │ main  run: omp --mode rpc + finish   │──► operator API: actions, events, transcript
          └──────────────┬───────────────────────┘
                         │ model calls
                         ▼
           omp auth-gateway (chart)  or  provider API keys
```

## Quick start

You need a cluster, `helm` and `kubectl`. This path uses a provider API key; see
[model access](docs/model-access.md) for the auth-gateway, which keeps the key out of the pods.

```sh
helm install omp oci://ghcr.io/techdufus/charts/omp-operator --version 0.2.1 \
  -n omp-system --create-namespace

kubectl create namespace demo
kubectl -n demo create secret generic anthropic --from-literal=api-key="$ANTHROPIC_API_KEY"

kubectl apply -f - <<'EOF'
apiVersion: omp.techdufus.com/v1alpha1
kind: OmpAgent
metadata:
  name: hello
  namespace: demo
spec:
  model:
    default: anthropic/claude-sonnet-5-5
  credentials:
    - name: anthropic
      description: Anthropic API key used for model calls
      secretRef: {name: anthropic, key: api-key}
      env: ANTHROPIC_API_KEY
---
apiVersion: omp.techdufus.com/v1alpha1
kind: OmpSession
metadata:
  name: first
  namespace: demo
spec:
  agentRef: {name: hello}
  prompt: Say hello, then call finish with verdict ok and a one-line summary.
EOF

kubectl -n demo get ompagent,ompsession -w
kubectl -n demo get ompsession first -o jsonpath='{.status.verdict}: {.status.summary}{"\n"}'
```

The chart installs in any namespace by default (`rbac.agentNamespaces` is empty). From here, add a
[webhook trigger](docs/webhooks.md), [actions](docs/actions.md), [notifications](docs/notifications.md)
and the [dashboard](docs/dashboard.md), or hand local work to the agent with [cloud agents](docs/cloud.md).
Working samples for every kind but `OmpThread` are in [`config/samples`](config/samples).

## Documentation

| Document | Contents |
|---|---|
| [Install](docs/install.md) | Helm install, chart values, namespaces and RBAC, exposing hooks, upgrades, signature verification |
| [Concepts](docs/concepts.md) | The five kinds, how a webhook becomes a session, how a cloud thread becomes sittings, labels, phases and verdicts |
| [Model access](docs/model-access.md) | Auth-gateway login flow, managed tokens, provider API keys |
| [Webhooks and schedules](docs/webhooks.md) | Adapters, authentication, CEL, grouping and budgets, response codes, cron |
| [Sessions](docs/sessions.md) | Lifecycle, queue and priorities, retries, continuity, workspace, toolchain, pod layout |
| [Actions](docs/actions.md) | The four action types, the target namespace rule, the trust model |
| [Notifications](docs/notifications.md) | Sinks, payloads, notices |
| [Cloud agents](docs/cloud.md) | `/cloud` from a local omp: enabling `spec.cloud`, API tokens, the extension, what a handoff ships, commands, lifecycle, getting results back, troubleshooting |
| [Dashboard](docs/dashboard.md) | OIDC, basic auth, access rules, API tokens, pages |
| [Metrics](docs/metrics.md) | Every metric and its labels |
| [Security](docs/security.md) | Trust boundaries, pod hardening, token scoping, redaction, cloud agents, network policy guidance |
| [Development](docs/development.md) | mise tasks, tests, generated files, releases |
| Reference | [OmpAgent](docs/reference/ompagent.md), [OmpTrigger](docs/reference/omptrigger.md), [OmpSession](docs/reference/ompsession.md), [OmpAction](docs/reference/ompaction.md), [OmpThread](docs/reference/ompthread.md), [cloud API](docs/reference/cloud-api.md), [operator flags and chart values](docs/reference/operator.md) |

## Development

Tools are pinned in `mise.toml`; run `mise tasks` for the verbs (`mise run ci`, `mise run test-e2e`).
See [docs/development.md](docs/development.md).

## License

Apache-2.0
