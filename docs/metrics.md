# Metrics

The operator serves Prometheus metrics at `:9090/metrics` over plain HTTP (`--metrics-bind-address`,
`--metrics-secure=false`; Service port `metrics`). The collectors are registered next to
controller-runtime's own metrics (reconcile counts and durations, work queues, client requests) on the same endpoint.
The chart ships no ServiceMonitor or scrape annotation; add your own, and restrict access to the port with a
[NetworkPolicy](security.md#network-policy).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `omp_operator_sessions_total` | counter | `namespace`, `agent`, `trigger`, `kind`, `verdict` | Finished sessions. `kind` is `webhook`, `schedule`, `manual` or `cloud`; `verdict` is the agent's verdict, `failed` for a Failed session, or `ok` for a cloud sitting (which has none) |
| `omp_operator_session_duration_seconds` | histogram | `namespace`, `agent` | Time from session start to completion |
| `omp_operator_sessions_active` | gauge | `namespace`, `agent`, `phase` | Sessions currently in each phase |
| `omp_operator_trigger_groups_active` | gauge | `namespace`, `trigger` | Groups tracked in a trigger's status |
| `omp_operator_webhook_events_total` | counter | `namespace`, `trigger`, `result` | Handled webhook events. `result` is `session`, `filtered`, `deduped`, `busy`, `capped`, `resolved` or `error` |
| `omp_operator_action_calls_total` | counter | `namespace`, `agent`, `action`, `result` | Action calls, including `finish`. `result` is `ok`, `refused` (policy or validation) or `error` (failed while running) |
| `omp_operator_model_auth_failures_total` | counter | `namespace`, `agent` | Session attempts that ended because the model rejected authentication |
| `omp_operator_notifications_total` | counter | `type`, `result` | Notification deliveries by sink type (`ntfy`, `slack`, `discord`, `webhook`) and final result (`ok`, `error`) |
| `omp_operator_thread_turns_total` | counter | `namespace`, `agent`, `state` | Finished cloud thread turns, counted once each when their notification is recorded. `state` is `done`, `failed` or `cancelled` |
| `omp_operator_threads` | gauge | `namespace`, `agent`, `phase` | Cloud threads in each phase (`Working`, `Idle`) |
| `omp_operator_data_free_bytes` | gauge | | Free bytes on the data volume, sampled every 60 seconds |
| `omp_operator_data_capacity_bytes` | gauge | | Capacity of the data volume |
| `omp_operator_build_info` | gauge | `version` | Always 1; the operator version |

Useful queries:

```promql
# sessions that need a human, last 24h
sum by (namespace, agent) (increase(omp_operator_sessions_total{verdict="needs_owner"}[24h]))

# the model login needs attention
increase(omp_operator_model_auth_failures_total[1h]) > 0

# an agent is hitting its budgets
sum by (namespace, trigger) (increase(omp_operator_webhook_events_total{result="capped"}[24h]))

# the data volume is almost full
omp_operator_data_free_bytes / omp_operator_data_capacity_bytes < 0.1
```
