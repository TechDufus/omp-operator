# Model access

An agent reaches its model one of two ways. `spec.model.default` is always `provider/id`, for example
`anthropic/claude-sonnet-5-5`; `spec.model.small` is an optional second model for small tasks and
defaults to `default`.

|  | Auth-gateway | Provider API keys |
|---|---|---|
| Set with | `spec.model.gateway` | `spec.credentials` with `env` |
| Subscription logins (Claude, ChatGPT) | Yes | No, keys only |
| Secret visible to the agent's pod | A per-agent token that works only at the gateway, restricted to the models you allow | The real provider key, in an environment variable the agent's tools can read |
| Rotating or revoking | Re-login once for all agents; revoke one agent by deleting it | Change the Secret |

Prefer the gateway. The pod never holds the provider credential, and a leaked token is useful only
against your gateway and only for the allowed models.

## Auth-gateway

The chart ships the gateway as an opt-in component:

```sh
helm upgrade --install omp oci://ghcr.io/techdufus/charts/omp-operator --version 0.2.1 \
  -n omp-system --create-namespace --reuse-values --set authGateway.enabled=true
```

It adds Deployment `omp-auth`, Service `omp-auth` (port 4000) and PVC `omp-auth-data`, runs the pinned
runtime image as UID 10001 and passes `--gateway-clients-secret=<authGateway.clientsSecret>` (default
`omp-auth-clients`) to the operator. The Pod contains two containers:

- `broker` (a native sidecar): `omp auth-broker serve --bind=127.0.0.1:8765`. It holds the provider
  logins on the `omp-auth-data` volume.
- `gateway`: `omp auth-gateway serve --bind=0.0.0.0:4000 --clients=/etc/omp-auth/clients.json`. It
  starts once the broker's token file exists, and authenticates each client token against the clients
  Secret.

### Log in once

The gateway has no login until you give it one. The broker's OAuth flow redirects to a local port, so
forward it first. The commands are also printed by `helm install`:

```sh
kubectl -n omp-system port-forward deploy/omp-auth 54545:54545 &
kubectl -n omp-system exec -it deploy/omp-auth -c gateway -- omp auth-broker login anthropic
kubectl -n omp-system rollout restart deploy/omp-auth
```

`anthropic` is the provider. The login's OAuth callback is received on the forwarded port, so complete
the browser step on the machine that runs the port-forward. For a ChatGPT subscription use provider
`openai-codex` and forward port 1455 instead of 54545. The restart makes the gateway pick up the
new login. The login lives on the `omp-auth-data` volume and survives restarts.

When a login expires or is revoked, sessions end with outcome `auth` and the owner is told (see
[below](#when-the-login-fails)); repeat the same commands.

### Point an agent at it

```yaml
spec:
  model:
    default: anthropic/claude-sonnet-5-5
    thinking: medium
    gateway:
      url: http://omp-auth.omp-system.svc:4000   # http://omp-auth.<release-namespace>.svc:4000
```

With no `tokenSecretRef`, the token is **managed**:

1. The operator creates Secret `<agent>-omp-gateway` (key `token`: 32 random bytes, hex-encoded), owned
   by the agent. The token is never rotated automatically; delete the Secret to get a new one.
2. It rewrites the clients Secret (`<authGateway.clientsSecret>`, key `clients.json`, in the operator's
   namespace) from every managed agent:
   `{"<namespace>.<agent>": {"tokenSha256": "<sha256 of the token string>", "models": [...]}}`. The operator
   only writes when the content changes.
3. The gateway reloads that file within about 10 seconds, and kubelet may take a while to propagate
   the Secret into the gateway pod. After the running operator writes the clients Secret, sessions of
   managed agents wait (`Pending`) until 2 minutes have passed, so a brand-new agent does not start before the gateway knows its token.
4. The session pod receives the token as `OMP_GATEWAY_TOKEN`, in the main container only. The driver
   writes omp's `models.yml` so the providers of the default and small models use `baseUrl` = the
   gateway URL, `apiKey` = `OMP_GATEWAY_TOKEN`, `transport: pi-native`.

`model.gateway.allowedModels` lists the model patterns the gateway permits this agent. The computed
default is `["<provider of model.default>/*"]`. If `model.small` uses a different provider, list both:

```yaml
      allowedModels: ["anthropic/*", "openai/gpt-*"]
```

The operator **owns** `clients.json` of the clients Secret: it contains exactly the managed agents and is
`{}` when there are none. Do not put your own clients into it.

#### Bring your own token or gateway

`model.gateway.tokenSecretRef: {name, key}` points at a Secret in the agent's namespace and turns off
token management for that agent: the operator neither creates a token nor adds a clients entry. Use it
with a gateway you run and configure yourself. An agent with a gateway but no `tokenSecretRef` while
`--gateway-clients-secret` is unset (chart `authGateway.enabled=false`) is not Ready, reason
`NoGatewayToken`.

### When the login fails

If the gateway answers HTTP 401 or 403 to a model call, the driver reports outcome `auth`. The session
goes back to `Pending` and is retried after `limits.retryDelay` while attempts remain (see
[sessions](sessions.md#retries)); `omp_operator_model_auth_failures_total` counts the failed attempts. At
most every 6 hours per agent the agent's [notification sinks](notifications.md#notices) get
`<agent>: model login missing or expired` with the login commands above, and a session that exhausts its
attempts ends `Failed`.

## Provider API keys

Without `model.gateway`, expose the key to omp as the provider's usual environment variable with a
credential:

```yaml
spec:
  model:
    default: anthropic/claude-sonnet-5-5
  credentials:
    - name: anthropic
      description: Anthropic API key used for model calls
      secretRef: {name: anthropic, key: api-key}
      env: ANTHROPIC_API_KEY
```

The variable name is whatever omp expects for that provider (`ANTHROPIC_API_KEY` for `anthropic`);
see omp's documentation for others. Credentials are described to the agent by name and description, never
by value, and the driver scrubs their values from everything it reports, but the agent's `bash` tool
can read the environment of its own pod. Use a key scoped as narrowly as the provider allows, and see
[security](security.md#credentials-and-redaction).

`env` names beginning with `OMP_` or `MISE_`, and `HOME`, `PATH` and `KUBECONFIG`, are reserved.
