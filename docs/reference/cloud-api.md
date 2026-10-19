# Cloud API

The HTTP API local omp uses to hand work to [cloud agents](ompthread.md): upload a repository and conversation, follow
the thread, send follow-up messages and fetch the result branch. It is served by the operator on the dashboard
port (8080, Service port `web`) under `/api/`, so it needs the same Ingress or port-forward as the dashboard.

Every response is JSON with `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`, except the two file
downloads. Errors are `{"error": "<message>"}`.

## Authentication and access

Only `Authorization: Bearer <token>` is accepted. Tokens are defined in the access file as `apiTokens` (see
[dashboard](../dashboard.md#api-tokens)); the token named `dev` is the principal `token:dev`, which access rules grant
a role on namespaces: `viewer` reads, `operator` also writes and includes `viewer`.

| Case | Response |
|---|---|
| No `apiTokens` configured | `503` `cloud API not configured: no apiTokens in the access file` |
| Missing or unknown token | `401` with `WWW-Authenticate: Bearer realm="omp-operator"` |
| Namespace not granted to the token, or not served by the operator | `404` `not found` |
| A viewer calling a write route | `403` `forbidden: operator role required` |
| Unknown route | `404` `not found` |

## Routes

`{ns}` is the namespace of the agent and the thread.

| Route | Role | Purpose |
|---|---|---|
| `GET /api/v1/whoami` | any | `{user, agents}`: the cloud agents in namespaces where the token is operator, each `{namespace, name, ready, defaultModel, modelPatterns, gitHosts, maxUploadBytes}` |
| `POST /api/v1/namespaces/{ns}/threads` | operator | Create a thread (multipart, below); `201` with a thread summary |
| `GET /api/v1/namespaces/{ns}/threads` | viewer | `{threads: [summary]}`, the newest 50 |
| `GET /api/v1/namespaces/{ns}/threads/{name}` | viewer | Thread detail |
| `GET .../threads/{name}/events?since=<RFC3339Nano>` | viewer | `{session, events: [{at, kind, text}]}` of the active (else newest) sitting, only events after `since`, at most the newest 500. Before the first sitting: `{"session":"","events":[]}` |
| `POST .../threads/{name}/messages` | operator | Body `{"text": "..."}` (1 to 32768 bytes); `201 {"turn": {...}}`. `409` when the thread has 200 turns (`thread has 200 turns; start a new one`) or is being deleted |
| `POST .../threads/{name}/stop` | operator | Abort the running turn and cancel queued ones; `200 {"stoppedRunning", "cancelled"}` |
| `GET .../threads/{name}/result` | viewer | The result branch as a git bundle, with headers `X-Omp-Branch`, `X-Omp-Head` and `X-Omp-Base`; supports `Range`; `404 no result yet` |
| `GET .../threads/{name}/conversation` | viewer | The conversation as a gzip tarball of omp's session files; `404` until stored |
| `DELETE .../threads/{name}` | operator | Delete the thread (background propagation); `202` |

`modelPatterns` is the agent's `model.gateway.allowedModels` (default `["<provider of model.default>/*"]`), or
`[model.default, model.small]` without a gateway. A pattern ending in `*` is a prefix.

### Thread summary and detail

A summary is `{namespace, name, agent, title, owner, phase, turns, queued, model, latest, result, createdAt,
lastActivityAt, url, warnings}`; `turns` is the number of turns, `url` is `<--dashboard-url>/threads/<ns>/<name>` (empty
without the flag), `warnings` appears only in the creation response. A detail has the same fields except that
`turns` is the array of turn objects, plus `repo`, `paths` and `activeSession`.

A turn is `{id, state, text, by, queuedAt, startedAt, completedAt, reply, title, error, usage, head, filesChanged,
insertions, deletions, pushed, pushError}`. `text` is cut to 2000 bytes.

## Creating a thread

`POST /api/v1/namespaces/{ns}/threads` takes `multipart/form-data`, read in part order:

| Part | Content | Limit |
|---|---|---|
| `request` (first) | JSON, below | 2 MiB |
| `source` | git bundle: `refs/omp-cloud/handoff`, delta mode `^<baseCommit>` | `--cloud-max-upload-bytes` |
| `conversation` | omp session `.jsonl` | 64 MiB |
| `setup` | tar.gz extracted into `~/.omp/agent` | 64 MiB |

```json
{
  "agent": "coder", "prompt": "...", "model": "provider/id", "thinking": "high",
  "repo": {"name": "app", "remoteURL": "https://github.com/acme/app.git", "baseRef": "main",
           "baseCommit": "<sha>", "handoffCommit": "<sha>", "localBranch": "main"},
  "paths": {"home": "/home/ada", "workspace": "/home/ada/src/app"},
  "author": {"name": "Ada", "email": "ada@example.com"},
  "secretFiles": [{"path": ".env", "content": "<base64>"}],
  "secretEnv": {"API_KEY": "..."},
  "conversationFile": "2026-10-18_01.jsonl"
}
```

An empty `baseCommit` is a full-history upload and needs `source`. With `baseCommit` set (delta mode) the remote must be
reachable by the cloud agent, `remoteURL` and `baseRef` are required, and so is `source` unless `handoffCommit` equals
`baseCommit`. Credentials in `remoteURL` (URL userinfo, scp-style `user@`) are dropped before it is stored.

| Check | Failure |
|---|---|
| Agent exists and has `spec.cloud` | `404` |
| Agent is Ready | `409` `agent <x> is not ready: <reason>` |
| `prompt` is 1 to 32768 bytes; `repo.name` is set; `handoffCommit` (and `baseCommit` when set) is 40 or 64 hex characters | `400` |
| Delta mode: `remoteURL`, `baseRef` and, unless nothing changed, `source`; the remote's host is configured in the agent's `cloud.git` (or is http(s)) | `400` |
| `secretFiles`: at most 32, relative, clean, not under `.git`, unique; `secretEnv`: at most 64, valid names that are not reserved (`HOME`, `PATH`, `KUBECONFIG`, `OMP_*`, `MISE_*`); files and values together at most 1 MiB | `400` |
| `conversationFile` matches `^[A-Za-z0-9._-]+\.jsonl$`, and is present exactly when the `conversation` part is | `400` |
| Free space on the data volume is at least twice the request's `Content-Length` | `507` `insufficient storage on the operator data volume` |
| A part over its limit | `413` |

Two requests do not fail but fall back, adding a message to `warnings`: a `model` the agent does not allow is replaced
by the agent's default, and `paths` that the pod cannot mirror (not absolute, in a system directory, a workspace equal
to or containing the home) are dropped, so the pod uses `/home/agent` and `/workspace`.

The thread is created as `<agent>-<6 random characters>` with the finalizer `omp.techdufus.com/cleanup`, then its files,
its `<thread>-secrets` Secret (only with secrets) and its first turn. Any failure after the thread exists deletes it with
everything stored for it.
