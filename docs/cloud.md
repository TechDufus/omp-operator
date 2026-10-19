# Cloud agents

A cloud agent continues your local omp conversation on the operator. In any local omp session inside a git
repository, `/cloud <task>` ships the repository, your uncommitted changes, an allowlist of secrets, the
conversation and your omp setup to an `OmpAgent` that has [`spec.cloud`](reference/ompagent.md#cloud). The agent
works in a pod while you do something else, commits to a branch `omp-cloud/<thread>`, and you pull that branch
back (or fetch it from your Git host, when the agent pushes) and, if you want, `/cloud resume` the conversation
locally.

Cloud work stays visible and steerable without attaching. Each handoff, finished turn and question becomes a
[card](#cards) in your conversation; `/cloud` opens a [hub](#the-hub) over all your threads; the agent's questions
reach you and your model ([questions](#questions)); and your model can delegate on its own with the
[`cloud` tool](#the-cloud-tool).

Three parts work together:

| Part | What it is |
|---|---|
| The extension `omp-cloud` | Runs inside your local omp, in any terminal. Registers `/cloud`, takes the snapshot, uploads it, follows the thread |
| The [cloud API](reference/cloud-api.md) | Served by the operator under `/api/` on the dashboard port; bearer-token authenticated |
| An [`OmpThread`](reference/ompthread.md) | One handed-off conversation. It runs as a series of *sittings* (session pods) on the cloud agent |

## What the cloud agent has

The pod mirrors your machine as far as the cluster allows:

| Your omp | The cloud agent |
|---|---|
| The omp version | The omp of the agent's image. The runtime image pins one release (`OMP_VERSION` in `images/runtime/Dockerfile`, `v18.12.2` in this release); run `omp update` locally to match, and compare with `omp --version` |
| The repository at `/home/ada/src/app` | The same repository at the same absolute path, on branch `omp-cloud/<thread>`, starting from your state at handoff including uncommitted changes. Paths that cannot be mirrored fall back to `/workspace` ([why](#troubleshooting)) |
| `$HOME` | The same path. `~/.omp/agent` is restored from the shipped setup |
| Built-in tools | Every one when the agent sets `tools.allow: ["*"]`; the agent's MCP servers and actions as well. There is no `finish` tool |
| Model and thinking level | The model and level of your session, when the agent allows the model; otherwise the agent's default (a warning says so) |
| `config.yml` | Shipped (`ship.settings`), with the overrides below |
| Extensions | Shipped (`ship.extensions`) and loaded by the cloud omp |
| `AGENTS.md`, `RULES.md`, skills | Shipped; skills the agent itself defines replace same-named ones |
| Mnemopi memory | A snapshot of `memories/mnemopi`, shipped (`ship.memory`) |
| The conversation | Opened as the cloud omp's session, so it continues where you left off |
| Gitignored secrets you list | Files copied into the workspace and variables set in the environment |

Overrides applied to your `config.yml` in the pod: every model role whose provider the agent's gateway does not
serve, and the `default` role, point at the thread's model; the thread's thinking level wins; approvals are off
(`yolo`); computer use, collab auto-start and marketplace auto-update are off. Without a shipped `config.yml` the pod
uses the roles a plain session of the agent would have.

The system prompt gets a *Running in the cloud* section: the repository, the checkout path, the result branch and the
branch it was based on; use the `ask` tool for decisions that block the work (you may answer late; after
`cloud.questionTimeout` it is told to proceed on its best judgment); commit as you go, anything uncommitted is
committed at the end of the turn; do not push or change remotes; the shipped secret files must never be printed or
committed.

## Quick start

Cluster side:

```sh
# 1. A token for your laptop: keep the file, store the same value in a Secret.
umask 077
openssl rand -hex 32 > ~/.config/omp-cloud-token
kubectl -n omp-system create secret generic omp-cloud-laptop --from-file=token="$HOME/.config/omp-cloud-token"

# 2. Let the operator accept it (see "API tokens and access" below). The token Secret lives in the operator's
#    namespace. Helm replaces lists, so dashboard.access must hold your existing rules too.
helm upgrade omp oci://ghcr.io/techdufus/charts/omp-operator --version 0.2.1 -n omp-system --reuse-values \
  -f - <<'EOF'
dashboard:
  apiTokens:
    - {name: laptop, existingSecret: omp-cloud-laptop}
  access:
    - {users: ["token:laptop"], namespaces: ["agents"], role: operator}
EOF
```

An agent in namespace `agents`. Its git token is a Secret in that namespace (here the token `gh` uses; any token that
may push to the repository works; see [enabling cloud](#enabling-cloud-on-an-agent)):

```sh
gh auth token | kubectl -n agents create secret generic github-token --from-file=token=/dev/stdin
```

```yaml
apiVersion: omp.techdufus.com/v1alpha1
kind: OmpAgent
metadata: {name: coder, namespace: agents}
spec:
  model:
    default: anthropic/claude-sonnet-5-5
    gateway: {url: "http://omp-auth.omp-system.svc:4000", allowedModels: ["anthropic/*"]}
  tools: {allow: ["*"]}
  toolchain: {}
  cloud:
    git:
      - host: github.com
        auth: {tokenSecretRef: {name: github-token, key: token}}
        push: true
```

Local side:

```sh
omp plugin marketplace add TechDufus/omp-operator
omp plugin install omp-cloud@omp-operator
cat > ~/.omp/agent/cloud.json <<'EOF'
{"url": "https://omp.example.com", "tokenCommand": ["cat", "/home/ada/.config/omp-cloud-token"],
 "namespace": "agents", "agent": "coder"}
EOF
```

Then, in omp inside a repository: `/cloud add retries to the HTTP client and run the tests`. The extension prints
`☁ <thread> started on agents/coder — /cloud watch <id>` and shows a live widget. For a throw-away setup on kind,
see the [dev cluster](development.md#dev-cluster).

## Enabling cloud on an agent

Setting `spec.cloud` (even `cloud: {}`) makes an agent a cloud agent: it then appears in the API's `whoami` for
tokens that are operator in its namespace, and threads can be created on it. The agent name is limited to 50
characters. Full field reference: [`cloud`](reference/ompagent.md#cloud).

| Field | Default | Meaning |
|---|---|---|
| `cloud.idleTimeout` | `10m` | A sitting waits this long for the next message, then saves its state and exits. 1m to 24h |
| `cloud.maxSitting` | `4h` | A sitting takes no turn that could end past this; the next message starts a new pod. 10m to 24h |
| `cloud.threadRetention` | `168h` | An Idle thread is deleted this long after its last activity. At least 1h |
| `cloud.git[]` | none | Git hosts the pod may fetch the base commit from, and push to |

What else shapes a cloud agent:

- **`tools.allow: ["*"]`** gives the agent every built-in tool of omp, which is what parity with your local omp
  needs. Left unset, the default set applies (`read, grep, glob, bash, todo`); `*` cannot be combined with other
  entries.
- **`toolchain: {}`** caches the tools in the repository's `mise.toml`: the first sitting of an agent for a given
  `mise.toml` runs `mise install`, later ones restore it from the operator (the newest 8 per agent are kept). A partial
  install is cached too, and after every restore `mise install` runs again so missing tools are retried and the
  cache heals. Without `toolchain` nothing is installed. The agent's own stored toolchain sync is not used by cloud
  sittings.
- **`limits.sessionTimeout`** (default `30m`) is the time limit of one *turn*. **`limits.maxConcurrentSessions`**
  (default 1, at most 20) is how many sittings of the agent run at once; sittings of one thread never overlap. Give
  a cloud agent more than 1 if you hand off several tasks at a time.
- **`pod.resources`**, credentials, MCP servers, skills, actions and `notify` sinks apply to sittings as to any
  session. The agent's `workspace` does not: the repository comes from the handoff.
- Cloud pods have bigger volumes than plain session pods: workspace 20Gi, home 10Gi, mise 10Gi, tmp 5Gi.
- Model access works as for any agent: [model access](model-access.md). The model your omp uses must match
  `model.gateway.allowedModels` (computed default: every model of the default's provider), or, without a gateway,
  be `model.default` or `model.small`; otherwise the thread runs on `model.default`.

### Git hosts and push

`cloud.git[]` entries are matched by host name against your repository's remote URL (`git@github.com:acme/app.git`,
`ssh://git@host:2222/acme/app.git`, `https://github.com/acme/app.git`).

| Field | Meaning |
|---|---|
| `host` | Lowercase host name, for example `github.com`. Unique in the list |
| `auth` | [`GitAuth`](reference/ompagent.md#gitauth): `tokenSecretRef` (with `username`, default `x-access-token`), or `sshKeySecretRef` and `knownHostsSecretRef` |
| `push` | Push `omp-cloud/<thread>` to the remote after every turn. Needs `auth` with write access |

- **Fetch.** At the start of each sitting, the thread's base commit is fetched from the remote with the host's `auth`
  (token auth: `https://<host>/<path>`, or the `http(s)` scheme and port as written; SSH auth:
  `ssh://git@<host>/<path>`). A remote on a host that is not listed is fetched anonymously when it is an
  `http(s)` URL, and cannot be fetched when it is SSH, so a handoff of an SSH remote on an unlisted host carries the
  whole history instead.
- **Push.** With `push: true`, the branch `omp-cloud/<thread>` is force-pushed to the same remote after every turn;
  the outcome is recorded per turn (`pushed`, `pushError`). The branch name and URL are fixed by the operator; the agent
  cannot choose them. See [security](security.md#cloud-agents).

## API tokens and access

The extension authenticates to the cloud API with a bearer token. Tokens are defined in the operator's access file as
`apiTokens` and granted a role per namespace by access rules, using the principal `token:<name>`:

| Role | Can |
|---|---|
| `viewer` | List and read threads and their events, download results and conversations |
| `operator` | Everything a viewer can, plus create threads, send messages, stop and delete. `whoami` lists only the cloud agents of namespaces where the token is operator |

Chart values (details in [dashboard](dashboard.md#api-tokens) and the [operator reference](reference/operator.md#chart-values)):

```yaml
dashboard:
  apiTokens:
    - {name: laptop, existingSecret: omp-cloud-laptop}        # key "token" of the Secret; or {name, sha256}
  access:
    - {users: ["token:laptop"], namespaces: ["agents"], role: operator}
cloud:
  maxUploadBytes: 1073741824                                   # largest repository bundle, default 1 GiB
```

The API is on the dashboard port, so it needs the same Ingress, Gateway route or port-forward. Use HTTPS if it
leaves your network: the token is a bearer token. A token cannot sign in to the dashboard; dashboard users need their
own sign-in method and rules.

## Install the extension

```sh
omp plugin marketplace add TechDufus/omp-operator
omp plugin install omp-cloud@omp-operator
```

The plugin's version is the operator's: `omp-cloud@omp-operator` 0.2.0 is the code of tag `v0.2.0`. The catalog
(`.omp-plugin/marketplace.json`) pins each release to its tag, so work on `main` never reaches installed copies.

Updates: with `marketplace.autoUpdate: auto` in `~/.omp/agent/config.yml`, omp upgrades the plugin when an
interactive session starts and the catalog lists a newer version. omp refreshes the catalog once it is 24 hours
old, so a release arrives within a day; to take it at once, run
`omp plugin marketplace update omp-operator && omp plugin upgrade omp-cloud@omp-operator`. omp's default,
`notify`, only writes the available update to its debug log.

The extension (`clients/omp-cloud`) has no runtime npm dependencies. To work on it, link a checkout instead:
`ln -s <repo>/clients/omp-cloud ~/.omp/agent/extensions/omp-cloud` (`mise run dev:client` does that for the dev
cluster). Install one or the other, never both: each copy registers `/cloud`, omp runs only one of them without a
warning, and nothing shows which. Switch with `omp plugin uninstall omp-cloud@omp-operator` or by removing the link. The extension
never ships itself to the cloud.

## Configure the extension

`~/.omp/agent/cloud.json` (the directory is `PI_CODING_AGENT_DIR` when set):

| Field | Default | Meaning |
|---|---|---|
| `url` | | Operator URL (the dashboard URL; the API is under `/api/`) |
| `token` | | The bearer token itself. Prefer `tokenCommand` |
| `tokenCommand` | | Argument vector whose stdout, trimmed, is the token (10 s timeout), for example `["cat", "/home/ada/.config/omp-cloud-token"]` |
| `namespace`, `agent` | | Where to hand off. When unset, the extension asks `whoami`: exactly one cloud agent is used; none or several is an error that tells you what to set |
| `maxUploadMB` | `512` | Largest repository bundle to upload, further capped by the agent's limit (`cloud.maxUploadBytes`) |
| `forceBundle` | `false` | Always upload the whole history, never a delta against the remote |
| `ship.settings` | `true` | Ship `config.yml` |
| `ship.extensions` | `true` | Ship `extensions/` |
| `ship.memory` | `true` | Ship `memories/mnemopi/` |
| `excludeExtensions` | `[]` | Names inside `extensions/` not to ship (for example editors or terminal-multiplexer extensions that make no sense in a pod) |

Environment variables override the file: `OMP_CLOUD_URL`, `OMP_CLOUD_TOKEN`, `OMP_CLOUD_NAMESPACE`, `OMP_CLOUD_AGENT`.
The token comes from `OMP_CLOUD_TOKEN`, else `token`, else `tokenCommand`.

A repository file `<repo root>/.omp/cloud.json` overrides `agent` and `namespace` for that repository and is where the
repository's secrets are listed:

```json
{"agent": "coder", "namespace": "agents", "secretFiles": [".env"], "secretEnv": ["NPM_TOKEN"]}
```

Precedence: environment, then the repository file, then `~/.omp/agent/cloud.json`. `/cloud status` shows which files
were read, the URL, your user and the agents you may hand off to.

### Secrets

Nothing secret leaves your machine unless you list it:

- `secretFiles`: workspace-relative paths (at most 32), each an existing regular file of at most 256 KiB that git
  **ignores** (`git check-ignore`). The handoff refuses otherwise: `<path> is not gitignored; refusing to ship it`.
  They are copied into the same path of the pod's workspace, mode `0600`, and kept out of the agent's commits.
- `secretEnv`: names (at most 64) of variables set in your omp's environment. They are set in the cloud agent's
  environment. `HOME`, `PATH`, `KUBECONFIG`, `OMP_*` and `MISE_*` are refused. A name that is not set locally
  fails the handoff: `<NAME> is not set in this omp's environment`.
- Files and values together are at most 1 MiB.

Values are redacted in everything the pod reports. See [security](security.md#cloud-agents).

## What a handoff ships

| Item | Detail |
|---|---|
| Repository | Your state as a git bundle, uploaded as a *delta* or in *full* mode, below |
| Uncommitted work | Recorded as one extra commit, `omp-cloud: local changes at handoff`, holding every tracked change and every untracked file that git does not ignore, except [credential-named files](#what-stays-on-your-machine). Made with a private index, so your index, branch and working tree are untouched |
| Model, thinking level | `provider/id` of the current model and the thinking level of your session |
| Paths, git identity | Your `$HOME` and repository root, and `user.name`/`user.email` (default `omp-cloud`/`omp-cloud@localhost`) |
| Secrets | `secretFiles` and `secretEnv`, [above](#secrets) |
| Conversation | The current session file (at most 64 MiB; larger is skipped with a warning and the agent starts without it). The header line gets a new session id and `parentSession` pointing at your file, so the cloud and local sessions never share an id; [cards](#cards) are removed (children re-linked) and, for a delegation by the `cloud` tool, the assistant message carrying the pending call and everything after it. Other lines stay byte-identical. A session not yet written to disk has no conversation |
| Setup | `AGENTS.md`, `RULES.md`, `skills/`, and per `ship.*`: `config.yml`, `extensions/` and `memories/mnemopi/` (SQLite databases are written as consistent snapshots), plus the blobs the conversation references. Symlinks are followed. At most 64 MiB. Nothing else from `~/.omp/agent` is shipped |

**Delta mode** is used when all hold: `forceBundle` is off; the current branch has an upstream (else the remote
`origin` and its default branch is used); your local `refs/remotes/<remote>/<branch>` has a merge-base with `HEAD`;
and the remote is an `http(s)` URL or its host is in the agent's `cloud.git`. The bundle then holds only the commits the
remote does not have, and the pod fetches the base from the remote. With nothing beyond the base, no bundle is
uploaded. Run `git fetch` before a handoff so your remote-tracking branch is current, and push what the pod needs
as its base.

**Full mode** uploads the whole history as a bundle, for repositories with no remote, no upstream, an SSH remote
on a host the agent does not list, or `forceBundle`. The upload limit is the smaller of `maxUploadMB` and the agent's
`cloud.maxUploadBytes`; over it: `repository bundle is <X> MB, over the <Y> MB upload limit`.

In the pod, a sitting rebuilds the workspace from these (a `git init`, the base fetched from the remote, the bundle
applied, `omp-cloud/<thread>` checked out), adds `origin` without credentials, sets the git identity, turns commit
signing off, copies secret files, extracts the setup into `~/.omp/agent`, installs the agent's skills and the
cached toolchain, and restores the conversation.

## Commands

`/cloud` is the only entry point; there is no input-prefix trigger. It runs immediately, even while the local agent
is streaming. The first word, case-insensitive, selects a verb; anything else is a task to hand off.

| Command | Behaviour |
|---|---|
| `/cloud` | Opens the [hub](#the-hub), an overlay with the agents, the threads and live detail. Without an overlay-capable UI (RPC, a terminal host without overlays) it posts an [inventory card](#cards) |
| `/cloud <task>`, `/cloud new <task>` | Hand off. `new` launches the rest as the task even when it starts with a verb word (`/cloud new stop the flaky test`). Notifies `☁ Handing off to the cloud…`, uploads in the background (launches are serialized per repository), then `☁ <thread> started on <ns>/<agent> — /cloud watch <id>`, one warning per warning from the API, and a [launch card](#cards) in the conversation (no model turn). Starts watching |
| `/cloud list` | Widget listing up to 20 threads of the namespace: `<id> <phase> t<turns> <latest state> <title>` |
| `/cloud show <id>` | Widget with the header, the first 12 lines of the latest reply (or error), the pending question, the result line and the last 8 events |
| `/cloud watch <id>` | Live widget `omp-cloud` above the editor: header, a pending question and the last 10 events, refreshed every 3 s while the thread works or waits, plus a footer status `☁ <k> working` (`☁ <k> working · <m> waiting` when a thread waits). Each finished turn is notified (`☁ <thread> turn <n> done: <title>`, an error when failed) and becomes a [turn card](#cards); each new question is notified and becomes a [question card](#cards). The widget then shows the first 8 lines of the reply and `/cloud pull <id>`, and clears after 10 minutes. An idle thread is shown once. Polling stops after 5 consecutive failures |
| `/cloud unwatch` | Stop the live widget |
| `/cloud send <id> <message>` | Queue a follow-up turn (at most 32 KiB; a thread holds 200 turns), then watch |
| `/cloud answer <id> [text]` | Answer the question the thread waits on, [below](#questions) |
| `/cloud stop <id>` | Abort the running turn and cancel queued ones: `☁ stopped <thread>: running yes, cancelled <n>` |
| `/cloud pull <id>` | Bring the result branch into your repository, [below](#getting-results-back) |
| `/cloud resume <id>` | Continue the thread locally, [below](#getting-results-back) |
| `/cloud delete <id>` | After a confirmation, delete the thread and everything stored for it |
| `/cloud status` | Config files read, URL, user and the agents you may hand off to, with their running sessions |
| `/cloud help` | One usage line per verb |

`<id>` is the full thread name, its last 6 characters (as printed after `/cloud watch`), or a unique name prefix. A
verb that takes an id and finds none says `☁ no thread matches "<token>"; to start a cloud task beginning with
"<verb>", use /cloud new <task>`. Tab completion offers verbs and, after an id verb, thread names from a cache that
`list`, handoffs and id verbs refresh; it never calls the network. Attached images are not sent: slash commands
receive text only.

## Cards

Everything you would want to know about a thread shows up in your local conversation as a *card*: a transcript
entry of type `omp-cloud`. omp draws the card from structured data with its terminal components (sections,
key/value rows, metric rows, Markdown, a collapsible event list); the model sees a compact text version that starts
with `[cloud]`.

| Card | Posted when | What it shows |
|---|---|---|
| Launch | A handoff succeeded | Thread, agent, model and thinking level; repository, base and result branch; commits and changed files shipped; the first 10 untracked files (`and N more`); the files [left on your machine](#what-stays-on-your-machine); secret files and variables, conversation and setup sizes; warnings |
| Turn | A turn finished | `✓`/`✗`, thread, turn, duration, model, cost; `+ins −del · files · pushed or push error · branch`; the reply as Markdown; the events in a collapsed list (expand the message to read them); hints for `pull`, `send` and `resume` |
| Question | The cloud agent asked | Thread, turn, the question, its options with descriptions, the recommended one, and how to answer |
| Inventory | `/cloud` without an overlay UI | Agents with capacity and the namespace's threads |

- **What the model sees.** `[cloud] <thread> turn 2 done: <title>`, the diffstat, the branch and head, and the reply
  cut to 1500 characters (the card shows up to 8000). A launch says what was shipped and what stayed, and that results
  arrive later.
- **Plain fallback.** If omp's terminal components cannot be loaded, or a card fails to draw, it is drawn as plain
  wrapped text with the same content.
- **Delivery.** Threads you start with `/cloud` only *display* their cards: an idle conversation shows them at once
  without a model turn; a busy one shows them when the agent ends. Threads the model starts with the
  [`cloud` tool](#the-cloud-tool) wake it: a turn and question card starts a model turn when the conversation is idle
  and joins the running turn at its next step boundary otherwise. That origin is stored in the launch card, so it
  survives restarting omp or resuming the session.
- **Not uploaded.** Cards are removed from the conversation a later handoff ships, and their children re-linked, so a
  cloud agent never reads its own earlier reports as part of your conversation.

## The hub

`/cloud` with no arguments opens an overlay in the terminal UI. It polls every 3 seconds.

- **Agents** of the namespace with their readiness, running sessions of the maximum, and how many threads work, wait
  and idle.
- **Threads**, newest first: phase icon (`●` working, `?` waiting on you, `○` idle), id, phase, turns, latest turn
  state, `+/−` of the result, age and title.
- **The selected thread**: its pending question with the options, the first lines of the latest reply, and the live
  events.

| Key | Action |
|---|---|
| `↑` `↓` (`k` `j`) | Select a thread |
| `s` | Send a message (prompts for the text) |
| `a` | Answer the pending question (prompts) |
| `x` | Stop, after a confirmation |
| `p` | Pull the result branch |
| `r` | Resume the thread locally; closes the hub |
| `d` | Delete, after a confirmation |
| `q`, `Esc` | Close |

An action closes the overlay, runs, reports with a notification, and reopens the hub.

## Questions

The cloud agent asks you with omp's `ask` tool when a decision blocks its work. The turn keeps running, the thread
phase becomes `Waiting`, and you see the question:

- as a notification `☁ <thread> asks: <question> — /cloud answer <id>` and a [question card](#cards), once per
  question;
- in the footer: `☁ 1 working · 1 waiting`;
- in the hub, on the thread and in its detail;
- in the model's context, as the card's `[cloud]` text, so a model that started the thread can answer through the
  [`cloud` tool](#the-cloud-tool).

Answer with `/cloud answer <id> [text]`:

- **With text**: a text equal to an option label (ignoring case) selects that option; for a multiple-choice question a
  comma-separated list of labels selects them all; anything else is a free-text answer. Free text is always allowed.
- **Without text**: a picker lists the options (the recommended one marked) and `Other (type your own)`; multiple
  choice repeats until `Done`. Cancel to leave the question open.

Several questions from one `ask` call are answered in order, oldest first. Answering resumes the turn.

**No answer in time.** After `cloud.questionTimeout` (default `30m`, set on the agent) the agent is told "No answer
within <timeout>. Continue with your best judgment" and carries on, saying in its reply what it assumed. The question
is marked as expired on the turn. Time spent waiting does not count against `limits.sessionTimeout`. Stopping the
thread expires its open questions too.

## The `cloud` tool

The extension also gives your local model a tool named `cloud` (`cloud_agent` if another tool has the name), so it can
delegate work to a cloud agent and manage the threads it started, without you typing `/cloud`. The tool's description
tells the model when to delegate (work that is independent of your machine and long-running or parallel), to check
capacity first, that results come back as `[cloud]` messages, and not to poll.

| `op` | Arguments | Does | Approval tier |
|---|---|---|---|
| `delegate` | `task`, `agent?` | Ships the repository, the conversation and your setup like `/cloud <task>`; returns the thread, agent and warnings | `exec` |
| `list` | | Agents with capacity (running sessions of the maximum; threads working, waiting, idle) and recent threads | `read` |
| `status` | `thread` | Phase, the last 3 turns with replies up to 1500 characters, the pending question and the result | `read` |
| `send` | `thread`, `text` | Queues a follow-up turn | `write` |
| `answer` | `thread`, `selected?`, `answer?` | Answers the pending question: option labels and/or free text | `write` |
| `stop` | `thread` | Aborts the running turn and cancels queued ones | `write` |
| `pull` | `thread` | Fetches the result branch into your repository (the working tree is not touched) | `write` |

Approval follows omp's tool approval mode (`tools.approvalMode`, `tools.approval.cloud`): `read` calls always run;
`always-ask` prompts for `write` and `exec` calls, `write` mode only for `exec` (`delegate`), and `yolo` (the default)
for nothing. A model-started delegation cuts its own pending `cloud` call out of the uploaded conversation, so the
cloud agent never sees a tool call without a result.

## What stays on your machine

- **Credential-named files.** The work-in-progress snapshot leaves out files whose name looks like a credential:
  `.env`, `.env.*` (not `*.example`, `*.sample`, `*.template`), `*.tfvars`, `*.tfvars.json`, `*.tfstate`,
  `*.tfstate.*`, `*.pem`, `*.key`, `*.p12`, `*.pfx`, `*.jks`, `*.keystore`, `id_rsa`, `id_dsa`, `id_ecdsa`,
  `id_ed25519`, `.netrc`, `.npmrc`, `.pypirc`, `kubeconfig`, `*.kubeconfig` and `talosconfig`. An untracked one is
  skipped; a tracked one keeps the content of your last commit, so uncommitted edits to it are not shipped. Committed
  history is shipped as it is. The launch card and the model-visible text list what was left out. Files you list in
  `secretFiles` follow the [secrets rules](#secrets) instead.
- **Everything git ignores**, unless listed in `secretFiles`.
- **Environment variables** you did not list in `secretEnv`.
- **The extension itself**, and any extension you put in `excludeExtensions`.
- **Untracked files that are not credential-named do ship.** The launch card names the first 10; check them before
  you hand off if the directory holds scratch files.

## Lifecycle

- **Thread.** The handoff creates an `OmpThread` named `<agent>-<6 random characters>` and queues your task as
  turn 1. Each `/cloud send` queues another turn. A turn is `queued`, `running`, `done`, `failed` or `cancelled`.
  `kubectl get ompthread` shows phase (`Waiting` while a running turn has an unanswered question, `Working` while a
  turn is queued or running, else `Idle`), turn count and the latest turn; the dashboard has a page per thread
  (`/threads/<ns>/<name>`).
- **Sitting.** When a turn is queued and no sitting is active, the operator starts a sitting, an `OmpSession`
  `<thread>-<n>` with `kind=cloud` that runs one pod. It waits in the agent's queue if the agent is at
  `maxConcurrentSessions`. The pod's omp serves the thread's turns one after another.
- **Warm pod.** After a turn the pod commits what the agent left uncommitted, stores the branch on the operator,
  pushes it when push is on, reports the reply, and then waits `cloud.idleTimeout` for the next message. A follow-up
  within that time is served at once, in the same omp, in the same workspace.
- **Max sitting.** A sitting takes no new turn that could end after `cloud.maxSitting` (the first turn is always
  taken); a message that arrives then starts a new pod. The pod's hard deadline is `maxSitting` plus 10 minutes.
- **Cold restore.** After the idle timeout, or at `maxSitting`, the sitting saves the conversation and ends
  `Succeeded` (no verdict). The next message starts a new sitting that restores the branch from the stored result, the
  conversation, the setup and the toolchain cache. **Everything committed survives.** Things that are not in the branch
  or the conversation do not: ignored files (build output, installed dependencies), files in the home directory and
  memories the agent wrote. They are rebuilt or re-extracted from the upload.
- **Failures.** A model outage (HTTP 429 or 5xx), an omp crash or a terminated pod makes the sitting retry; the
  running turn stays running and is redone by the next sitting. Retries follow `limits.maxAttempts` and
  `limits.retryDelay` like any session. When a sitting fails for good, every queued and running turn fails with
  `cloud session failed: <reason>`; a turn queued afterwards starts a new sitting.
- **Stop.** Aborts the running turn (it fails with `stopped by token:<name>`) and cancels queued ones. The thread stays
  and accepts new messages. A turn that outlives `limits.sessionTimeout` fails.
- **Retention.** An Idle thread is deleted `cloud.threadRetention` after its last activity. Its stored files, the
  conversation and its Secret go with it. Pull what you need before.
- **Notifications.** Each finished turn goes to the agent's `notify` sinks. See [notifications](notifications.md).

Details of sittings and status: [OmpThread](reference/ompthread.md#sittings).

## Getting results back

The agent works on the branch `omp-cloud/<thread>`. Its first commits are your own history and the
`omp-cloud: local changes at handoff` commit; the agent's commits follow.

- **From your Git host.** When the thread's remote host has `push: true`, the branch is force-pushed to your remote
  after every turn: `git fetch origin omp-cloud/<thread>`, open a pull request, or check it out. A failed push does
  not fail the turn; the turn's `pushError` (and the dashboard) say why.
- **`/cloud pull <id>`.** Downloads the result from the operator and fetches it into your repository. When
  `omp-cloud/<thread>` is checked out it fast-forwards; otherwise it creates or **resets** the local branch of that name
  to the result. Reports `☁ omp-cloud/<thread> → <sha> (+<ins> −<del>, <files> files)`. The result holds only the
  commits beyond the thread's base, so your repository needs the base commit: `git fetch` first when you get
  `your repository lacks the thread's base commit <sha>; git fetch <remote>, then retry`. A thread whose branch has
  nothing beyond its base has `☁ <thread> has no result yet`.
- **`/cloud resume <id>`.** Requires a clean working tree (untracked files count): `☁ commit or stash your changes
  first (or use /cloud pull)`. Pulls the branch, switches to `omp-cloud/<thread>`, downloads the conversation with the
  cloud agent's turns, copies it into your session directory and switches your omp to it. You are then in the cloud
  conversation, locally, on the cloud branch. A thread that is still working adds a warning that this is a snapshot.

Local and cloud copies diverge from the moment of handoff: your local session continues independently of the thread.

## Limits

- **Questions need you or your model.** A cloud agent runs without anyone attached. When it needs a decision it asks
  with omp's `ask` tool and carries on after `cloud.questionTimeout` with its best judgment; see
  [Questions](#questions). Attaching to a running agent and chatting with it directly are planned for v0.3.
- **Text only.** Images attached to the command are not sent.
- **Sizes.** Repository bundle: the smaller of `maxUploadMB` (512) and `cloud.maxUploadBytes` (1 GiB); conversation
  64 MiB; setup 64 MiB (a shipped setup expands to at most 512 MiB in the pod); result bundle 1 GiB; messages 32 KiB;
  200 turns per thread.
- **Pod volumes** (workspace 20Gi, home 10Gi, mise 10Gi, tmp 5Gi) and the agent's `pod.resources` bound what the agent
  can build.
- **No git credentials in the agent's container.** It cannot fetch private remotes or push; the operator does both.
  Credentials the agent owner defines under `spec.credentials` are available to it as for any agent.
- **Shared infrastructure.** Everything the agent does runs in a pod on the agent's service account, network and
  credentials. Cloud agents inherit your shipped extensions: exclude what does not belong in a pod.

## Troubleshooting

| You see | Cause and fix |
|---|---|
| `☁ omp-cloud is not configured: set OMP_CLOUD_URL and OMP_CLOUD_TOKEN or create ~/.omp/agent/cloud.json` | No URL or no token. `/cloud status` after fixing it shows what was read |
| `☁ no cloud agent is available to token:<name>` | No agent with `spec.cloud` in a namespace where the token is `operator`. Check the access rule and `kubectl get ompagent` |
| `☁ several cloud agents are available (...)` | Set `agent` and `namespace` in `cloud.json` or `.omp/cloud.json` |
| `agent <x> is not ready: <reason>` (HTTP 409) | The agent's `Ready` condition; `kubectl describe ompagent <x>` |
| `503 cloud API not configured` | No `dashboard.apiTokens`; the API is off |
| `401`, `403` | Wrong or unknown token; a viewer token cannot write. `404` also means the namespace is not granted or not served |
| `repository bundle is <X> MB, over the <Y> MB upload limit` | Push the branch so a delta against the remote is used, drop large files from history, or raise `maxUploadMB` and `cloud.maxUploadBytes` |
| `<path> is not gitignored; refusing to ship it` | Add the file to `.gitignore`, or remove it from `secretFiles` |
| `<NAME> is not set in this omp's environment` | Export the variable before starting omp, or remove it from `secretEnv` |
| The thread's turns fail with `cloud session failed: ...`; the sitting's init container `prepare` exits non-zero | `kubectl -n <ns> logs <thread>-<n>-<attempt> -c prepare`. If it says `base commit <sha> is not on <url> <ref>; fetch and push, then hand off again`, the commit the handoff is based on is not on the remote: push it, then hand off again (or set `forceBundle`). `no cloud.git host of the agent matches <url>` means the remote is not fetchable: add its host to `cloud.git`. A private `http(s)` remote on an unlisted host fails the same way |
| `☁ model <m> is not available on <agent>; using <default>` (warning) | Your model is outside `model.gateway.allowedModels`, or is not the agent's `default`/`small` model without a gateway. Widen the agent's list, or switch the local model. The thread runs on the agent's default |
| `☁ paths home "<h>" and workspace "<w>" cannot be mirrored; the cloud agent uses /home/agent and /workspace` (warning) | A path is not absolute or has characters other than letters, digits and `._@+-`; is longer than 512 bytes; is `/` or inside a system directory (`/bin /boot /dev /etc /lib* /opt /proc /run /sbin /sys /tmp /usr /var`); or the repository is the home directory or contains it. The agent works, but absolute paths in the conversation refer to your machine |
| `☁ conversation is over 64 MiB; the cloud agent starts without it` (warning) | The session is too large to ship; the task prompt still goes |
| `☁ memory copied without a snapshot; it may miss the last few writes` (warning) | The extension could not use Bun's SQLite, so the databases were copied as files |
| `507 insufficient storage on the operator data volume` | The data volume has less than twice the upload free; see [install](install.md#storage) |
| `your repository lacks the thread's base commit` on pull | `git fetch <remote>`, then retry |
| `409` on a message: `thread has 200 turns; start a new one` | Hand off again, or `/cloud resume` and continue locally |
| `☁ <thread> has no question waiting` | The question was already answered, expired, or the turn ended. `/cloud show <id>` shows the thread |
| `409 question is not waiting for an answer` on an answer | Someone else answered first, or the question timed out; the answer is recorded on the turn |
| Cards show as plain text, or `/cloud` posts an inventory card instead of the hub | omp's terminal UI library could not be loaded by the extension, or the UI cannot show overlays (RPC, a host without them). The plain card and the inventory card carry the same information |

The dashboard page `/threads/<namespace>/<name>` shows the turns, the result and the last 100 events of the newest
sitting. Metrics: `omp_operator_thread_turns_total`, `omp_operator_threads` ([metrics](metrics.md)).
