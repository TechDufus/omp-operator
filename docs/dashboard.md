# Dashboard

A read-only web UI on port 8080 (Service port `web`): agents, triggers, sessions, their reports and
transcripts. It has no JavaScript, no write operations and answers only GET and HEAD (405 otherwise).

Without a configured sign-in method every page except `/healthz` answers `503 dashboard access not
configured`. With a sign-in method but no matching access rule, a signed-in user sees nothing.

Reach it with a port-forward or put an Ingress or Gateway route in front of it (the chart creates none):

```sh
kubectl -n omp-system port-forward svc/omp-operator 8080:8080
```

## Access file

The chart renders `dashboard.*` into the ConfigMap `omp-operator-access`, mounted at the path of the
operator flag `--access-config` (default `/etc/omp-operator/access.yaml`, read at start; changing the values
restarts the operator). Its format:

```yaml
oidc:                       # optional
  issuerURL: https://id.example.com
  clientID: omp-operator
  clientSecretFile: /etc/omp-operator/oidc/client-secret
  redirectURL: https://omp.example.com/auth/callback
  scopes: [openid, email, profile, groups]
  usernameClaim: email
  groupsClaim: groups
basic:                      # optional
  htpasswdFile: /etc/omp-operator/basic/htpasswd    # bcrypt entries only
rules:
  - users: ["alice@example.com", "basic:ops"]
    groups: ["platform"]
    namespaces: ["*"]
    role: viewer
```

You do not write this file by hand with the chart; the values map as follows.

| Chart value | Access file |
|---|---|
| `dashboard.url` | `oidc.redirectURL` = `<url>/auth/callback` (required with OIDC) |
| `dashboard.oidc.issuerURL`, `clientID`, `scopes`, `usernameClaim`, `groupsClaim` | `oidc.*`; `issuerURL`, `clientID` and `clientSecret.existingSecret` are required to enable OIDC |
| `dashboard.oidc.clientSecret.existingSecret`, `.key` | Mounted at `/etc/omp-operator/oidc/client-secret`; `oidc.clientSecretFile` |
| `dashboard.basic.existingSecret` | A Secret with key `htpasswd`, mounted at `/etc/omp-operator/basic/htpasswd`; enables `basic` |
| `dashboard.access` | `rules`, as is |

The `scopes` always include `openid`. `usernameClaim` defaults to `email`, `groupsClaim` to `groups`. The
htpasswd file has `name:hash` lines, only bcrypt hashes are accepted (`htpasswd -nBC 12 <name>`), and a user
named `ops` is matched in rules as `basic:ops`.

### Rules

A rule grants a role on namespaces to users and groups. A user matches a rule when their username is in
`users` (an OIDC username, or `basic:<name>`) or any of their groups is in `groups`. `namespaces` lists the
granted namespaces; `*` means every namespace the operator serves. A user's access is the union of
their matching rules. `role` defaults to `viewer`, the only role in v0.1; any other value is rejected at
start.

A user only ever sees objects in granted namespaces. Everything else, including a namespace that does
not exist, is a plain `404`.

## Sign-in

**OIDC.** Unauthenticated requests are redirected to `/auth/login`. The operator starts the authorization-code
flow with a state and nonce kept in a signed cookie for 10 minutes. `/auth/callback` verifies the ID token
(issuer, audience, nonce), reads the username and groups from the claims, and sets the `omp_session`
cookie: signed with HMAC-SHA256 with the key in the Secret `omp-operator-dashboard-session` (created
automatically, in the operator's namespace), `HttpOnly`, `Secure`, `SameSite=Lax`, valid for 12 hours.
`/auth/logout` clears it. Because the cookie is `Secure`, serve the dashboard over HTTPS.

Register `<dashboard.url>/auth/callback` as the redirect URI with your identity provider and make it include
the `groups` claim if rules use groups. Only groups that some rule names are kept in the cookie.

**Basic auth.** `Authorization: Basic ...` is checked against the htpasswd file with bcrypt. Without OIDC,
unauthenticated requests get `401` with `WWW-Authenticate: Basic realm="omp-operator"`; with both
configured, browsers are sent to OIDC and scripts can still use basic credentials.

## Pages

| Page | Shows |
|---|---|
| `/healthz` | `ok`, no authentication |
| `/` | Visible agents (Ready, model, sessions in the last 24 hours by verdict, budgets used against limits), triggers, and the newest 50 sessions |
| `/agents/{namespace}/{name}` | Model, tools, skill sources, workspace, credentials (name, description and where, **never values**), MCP server names, actions, triggers, budgets, newest 100 sessions |
| `/triggers/{namespace}/{name}` | Type, hook URL (with `--hooks-public-url`), adapter, filters, mapping, grouping, groups, schedule, newest sessions |
| `/sessions/{namespace}/{name}` | Header, summary, proposed commands, actions, the last 200 events, report, error, usage, continuity and a transcript link. Refreshes every 10 seconds while Pending or Running |
| `/sessions/{namespace}/{name}/transcript` | The exported omp transcript |

Pages escape all content. Every response sends `X-Content-Type-Options: nosniff`, `Referrer-Policy:
no-referrer` and `Cache-Control: no-store`, and a Content-Security-Policy of `default-src 'none'; style-src
'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`. The transcript
page is omp's own exported HTML, which runs scripts, so it is served with `Content-Security-Policy: sandbox
allow-scripts allow-popups`, which gives it an opaque origin with no access to the dashboard's cookies. The
driver has already redacted credential values from it.
