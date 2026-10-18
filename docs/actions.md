# Actions

An agent can change things only through actions. An `OmpAction` declares one bounded capability; an
agent lists the actions it may use in `spec.actions`, and each one appears to the model as a tool:

```yaml
# OmpAgent
spec:
  actions:
    - name: restart-workload      # the tool is called restart_workload
```

The operator, not the pod, executes the call. The pod has no Kubernetes credentials for it, and every call
is checked against the action's policy first. Refusals come back to the agent as error results starting with
`refused:` so it can adapt.

Every action has:

| Field | Default | Meaning |
|---|---|---|
| `description` | required | The tool description the model sees. Say when to use it and when not to |
| `maxPerSession` | `3` | Calls of this action one session may make. Every executed call counts, including refused ones |
| exactly one of `kubernetesRolloutRestart`, `fluxReconcile`, `alertmanagerSilence`, `httpRequest` | | The type; reported in `status.type` |

Every call is recorded in the session's `status.actions` (action, target, ok, time, result), in its event
log, and counted in `omp_operator_action_calls_total{result=ok|refused|error}`.

An `OmpAction` is Ready when its name matches `^[a-z][a-z0-9-]{0,62}$`, is not reserved, exactly one type is set,
templates parse, the JSON Schema compiles, URLs are valid, and referenced Secrets exist (reasons
`InvalidName`, `ReservedName`, `InvalidSpec`, `MissingSecret`). The name must not collide with `finish` or
with a tool name omp registers itself (`read`, `bash`, `edit`, `task`, `todo` and so on; the full list is in
the [OmpAction reference](reference/ompaction.md#reserved-names)). An agent that lists an action that is
missing or not Ready is itself not Ready (`ActionNotReady`).

## Target namespace rule

`kubernetesRolloutRestart` and `fluxReconcile` act on objects in a namespace chosen by the agent. A target
namespace is allowed only when **all** of these hold:

1. It is in the action's `namespaces.allow` (default: the action's own namespace; `*` matches any).
2. It is not in `namespaces.deny`.
3. It is not the operator's own namespace.
4. It is the action's own namespace, **or** it is listed in the operator flag `--action-target-namespaces`
   (chart value `actions.targetNamespaces`), **or** that flag is `*`.

So users of a namespace can never reach outside it unless the cluster admin opens that up.

| Action in `team-a`, `allow` | Flag | Target | Allowed |
|---|---|---|---|
| unset | unset | `team-a` | yes |
| unset | unset | `team-b` | no |
| `[team-b]` | unset | `team-b` | no (not own, flag does not list it) |
| `[team-b]` | `team-b` | `team-b` | yes |
| `["*"]` | unset | `team-b` | no |
| `["*"]` | `*` | `team-b` | yes |
| `["*"]` | `*` | operator namespace | no, always |

The operator's cluster-wide RBAC for these actions (patch on Deployments, StatefulSets, DaemonSets, Flux
sources, Kustomizations and HelmReleases) is bounded only by this rule.

## `kubernetesRolloutRestart`

Rolling-restarts a Deployment, StatefulSet or DaemonSet.

```yaml
apiVersion: omp.techdufus.com/v1alpha1
kind: OmpAction
metadata: {name: restart-workload, namespace: platform}
spec:
  description: >-
    Rolling-restart a workload. Use it only when logs show it is wedged and a restart is the proven fix.
  maxPerSession: 2
  kubernetesRolloutRestart:
    kinds: [Deployment, StatefulSet]      # default [Deployment]
    namespaces: {allow: [platform], deny: []}
    cooldown: 6h                          # default 6h
    waitTimeout: 5m                       # default 5m
```

Agent arguments: `kind` (an allowed kind), `namespace`, `name`, `reason` (1-300 characters).

- The kind and namespace are checked as above.
- A target with a successful restart in the agent's sessions within `cooldown` is refused.
- The call patches the `kubectl.kubernetes.io/restartedAt` annotation on the pod template, then polls every 5
  seconds until the rollout completes, `waitTimeout` passes, or the session's deadline passes. The result is
  `restarted <kind> ns/name; ready N/N after Xs`, or `restart issued; not ready after <wait>: <counts>`.

## `fluxReconcile`

Asks Flux to reconcile an object now.

```yaml
spec:
  description: Ask Flux to reconcile a Kustomization or HelmRelease that looks stale.
  fluxReconcile:
    kinds: [Kustomization, HelmRelease]   # default Kustomization, HelmRelease, GitRepository, OCIRepository
    namespaces: {allow: [flux-system, platform]}
    waitTimeout: 5m
```

Kinds may also include `HelmRepository`. Agent arguments: `kind`, `namespace`, `name`, `with_source` (bool),
`reason`.

- Refused when the object is suspended: `suspended; resuming is not yours`.
- `with_source` also reconciles the object's referenced source when that source's kind is allowed and its
  namespace passes the target rule.
- The call sets the `reconcile.fluxcd.io/requestedAt` annotation and polls until
  `status.lastHandledReconcileAt` matches or the wait ends. The result reports the Ready status and message.

## `alertmanagerSilence`

Creates a silence in an Alertmanager.

```yaml
spec:
  description: Silence one alert for at most a few hours while a fix rolls out.
  alertmanagerSilence:
    url: http://alertmanager.monitoring.svc:9093
    authSecretRef: {name: am-token, key: token}   # optional bearer token
    maxDuration: 4h                               # default 24h
    protectedAlertnames: [Watchdog, InfoInhibitor]   # default; these can never be silenced
```

Agent arguments: `matchers` (1-8 of `{name, value}`), `minutes` (15 up to `maxDuration`) and `reason`.
Exactly one matcher must be on `alertname`, and its value must not be a protected alert. Matcher names match
`^[a-zA-Z_][a-zA-Z0-9_]*$`, values are 1-200 characters, and matching is by equality, never regex.

The operator POSTs `/api/v2/silences` with `createdBy` `omp-operator/<namespace>/<agent>` and comment
`<reason> (omp-operator session <namespace>/<session>)`, and returns `silence <id> until <endsAt>`.

## `httpRequest`

Sends an HTTP request to a fixed prefix, with arguments the agent supplies under a JSON Schema you
define.

```yaml
spec:
  description: Open an incident in the runbook tool. Arguments are validated.
  httpRequest:
    method: POST
    allowedURLPrefix: https://runbooks.example.com/api
    url: https://runbooks.example.com/api/services/{{ .Args.service | urlquery }}/incidents
    headers:
      - {name: Content-Type, value: application/json}
      - name: Authorization
        secretRef: {name: runbooks, key: bearer}
    body: '{"title": {{ json .Args.title }}}'
    parameters:
      type: object
      required: [service, title]
      additionalProperties: false
      properties:
        service: {type: string, maxLength: 63}
        title: {type: string, maxLength: 200}
    responseMaxBytes: 65536
```

- The agent's arguments are validated against `parameters` (a JSON Schema whose root is `type: object`, with
  no external `$ref`s) and refused if invalid. This is what the model sees as the tool's parameters.
- `url` and `body` are Go templates over `.Args` (missing keys are errors). `json` and `urlquery` are
  available as functions. Use `json` for anything inserted into a JSON body.
- The rendered URL must stay under `allowedURLPrefix` (an absolute http(s) URL without userinfo, query or
  fragment): the same scheme, host and port; no userinfo; no encoded `/` or `\` in the path; and the cleaned path
  equal to or below the prefix path. `https://api.example.com.evil.net/x`, `https://api.example.com@evil.net/`,
  `/../` and `%2f` escapes are refused.
- `headers` entries are literal `value`s or `secretRef`s. Secret header values are sent but never shown to the agent.
- The timeout is 30 seconds, redirects are **not** followed (a 302 is returned as is), and the result is `HTTP
  <code>\n<body>`, cut to `responseMaxBytes` (default 65536).

## Trust model

`httpRequest` and `alertmanagerSilence` send requests **from the operator's network position**, with the
operator's DNS and routing, and can use the secrets they reference. Whoever may create or edit an `OmpAction`
in a namespace therefore gets that reach, up to anywhere the operator can connect. Grant `OmpAction` write
access only to namespace administrators, and give agents access to actions through `OmpAgent.spec.actions` only.
Likewise, the target namespace rule is what limits Kubernetes and Flux actions: `--action-target-namespaces=*`
lets any action whose own `allow` says `*` act in every namespace except the operator's.

Keep `allowedURLPrefix` as narrow as possible, and keep Secret values to what the action needs. See also
[security](security.md).
