import { mkdtemp, rm } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import { answerSummary, answerThread } from "./answer";
import { ApiError, CloudClient } from "./api";
import { inventoryCard, type TuiModule } from "./cards";
import {
	completions,
	parseCommand,
	resolveCommand,
	resolveThread,
	shortName,
	type Command,
	type Completion,
} from "./commands";
import {
	agentDir,
	loadConfig,
	resolveConnection,
	selectAgent,
	selectNamespace,
	type CloudConfig,
	type Env,
} from "./config";
import { Deliverer, launchCard } from "./deliver";
import { plural } from "./format";
import { HubComponent, type HubResult } from "./hub";
import { launchHandoff } from "./launch";
import { extractConversation, installConversation, isClean, pullBundle, threadBranch } from "./pull";
import { git, gitRaw } from "./snapshot";
import {
	UserError,
	type AnswerBody,
	type CommandHost,
	type Host,
	type PiHost,
	type ThreadDetail,
	type ThreadOrigin,
	type ThreadSummary,
	type Whoami,
	type WhoamiAgent,
} from "./types";
import { helpLines, listLines, showLines, WIDGET_KEY, Watcher } from "./watch";

interface Session {
	cfg: CloudConfig;
	client: CloudClient;
	ns: string;
	root: string | undefined;
}

function errorText(e: unknown): string {
	return e instanceof Error ? e.message : String(e);
}

/** What a handoff started. */
export interface Delegated {
	name: string;
	namespace: string;
	agent: string;
	warnings: string[];
}

export interface LaunchOptions {
	origin: ThreadOrigin;
	/** The `cloud` tool call that delegated; its pending assistant message is cut from the upload. */
	toolCallId?: string;
	/** `name` or `namespace/name`; defaults to the configured agent. */
	agent?: string;
}

export interface PullResult {
	branch: string;
	head: string;
	filesChanged: number;
	insertions: number;
	deletions: number;
}

/** Runs `/cloud` commands and the `cloud` tool's operations. One instance per loaded extension. */
export class Cloud {
	readonly #pi: PiHost;
	readonly #ownRoot: string;
	readonly #env: Env;
	readonly #deliver: Deliverer;
	#tui: TuiModule | undefined;
	#names: string[] = [];
	#watcher: Watcher | undefined;
	#watcherKey = "";
	readonly #chains = new Map<string, Promise<unknown>>();

	constructor(pi: PiHost, ownRoot: string, env: Env = process.env) {
		this.#pi = pi;
		this.#ownRoot = ownRoot;
		this.#env = env;
		this.#deliver = new Deliverer(pi);
	}

	/** The host's pi-tui, once imported; the hub overlay needs it. */
	setTui(tui: TuiModule | undefined): void {
		this.#tui = tui;
	}

	/** Synchronous completion from the in-memory thread cache; never touches the network. */
	complete(prefix: string): Completion[] | null {
		return completions(prefix, this.#names);
	}

	/** Re-points the live watcher at the host of a new session. */
	rebind(host: Host): void {
		this.#watcher?.rebind(host);
	}

	/** Shows cards held back while the agent was busy, once it is idle. */
	flushCards(host: Host): void {
		this.#deliver.flush(host);
	}

	async handle(args: string, ctx: CommandHost): Promise<void> {
		try {
			await this.#dispatch(args, ctx);
		} catch (e) {
			ctx.ui.notify(e instanceof UserError ? e.message : `☁ ${errorText(e)}`, "error");
		}
	}

	async #repoRoot(cwd: string): Promise<string | undefined> {
		const r = await gitRaw(cwd, ["rev-parse", "--show-toplevel"]);
		return r.code === 0 ? r.stdout.trim() : undefined;
	}

	async #session(ctx: Host): Promise<Session> {
		const root = await this.#repoRoot(ctx.cwd);
		const cfg = await loadConfig({ env: this.#env, repoRoot: root });
		const conn = await resolveConnection(cfg, this.#env);
		const client = new CloudClient(conn.url, conn.token);
		const ns = cfg.namespace ?? selectNamespace(await client.whoami(), cfg);
		return { cfg, client, ns, root };
	}

	#getWatcher(ctx: Host, s: Session): Watcher {
		const key = `${s.cfg.url}|${s.ns}`;
		if (!this.#watcher || this.#watcherKey !== key) {
			this.#watcher?.unwatch();
			this.#watcher = new Watcher(ctx, s.client, s.ns, this.#deliver);
			this.#watcherKey = key;
		} else {
			this.#watcher.rebind(ctx);
		}
		return this.#watcher;
	}

	async #dispatch(args: string, ctx: CommandHost): Promise<void> {
		const parsed = parseCommand(args);
		if (parsed.kind === "error") {
			ctx.ui.notify(parsed.message, "error");
			return;
		}
		if (parsed.kind === "help") {
			this.#show(ctx, helpLines());
			return;
		}
		if (parsed.kind === "unwatch") {
			if (this.#watcher) this.#watcher.unwatch();
			else {
				ctx.ui.setWidget(WIDGET_KEY, undefined);
				ctx.ui.setStatus(WIDGET_KEY, undefined);
			}
			return;
		}
		if (parsed.kind === "launch") {
			// Handlers have a short budget and uploads exceed it; slash commands run
			// immediately even while the agent streams, so detach the launch.
			ctx.ui.notify("☁ Handing off to the cloud…", "info");
			const task = parsed.task;
			ctx.setTimeout(() => void this.#launchFromCommand(ctx, task), 0);
			return;
		}
		if (parsed.kind === "status") return this.#status(ctx);

		const s = await this.#session(ctx);
		if (parsed.kind === "hub") return this.#hub(ctx, s);
		let cmd: Command;
		if (parsed.kind === "list") {
			cmd = { kind: "list" };
		} else {
			cmd = resolveCommand(parsed, await this.#threadNames(s));
		}
		await this.#run(cmd, ctx, s);
	}

	async #threadNames(s: Session): Promise<string[]> {
		const threads = await s.client.listThreads(s.ns);
		this.#names = threads.map(t => t.name);
		return this.#names;
	}

	#show(ctx: Host, lines: string[]): void {
		if (this.#watcher) this.#watcher.showLines(lines);
		else ctx.ui.setWidget(WIDGET_KEY, lines.slice(0, 20), { placement: "aboveEditor" });
	}

	async #run(cmd: Command, ctx: CommandHost, s: Session): Promise<void> {
		switch (cmd.kind) {
			case "error":
				ctx.ui.notify(cmd.message, "error");
				return;
			case "list": {
				const threads = await s.client.listThreads(s.ns);
				this.#names = threads.map(t => t.name);
				this.#getWatcher(ctx, s).showLines(listLines(threads));
				return;
			}
			case "show": {
				const [detail, ev] = await Promise.all([s.client.thread(s.ns, cmd.thread), s.client.events(s.ns, cmd.thread)]);
				this.#getWatcher(ctx, s).showLines(showLines(detail, ev.events));
				return;
			}
			case "watch":
				await this.#getWatcher(ctx, s).watch(cmd.thread);
				return;
			case "send": {
				const turn = await s.client.message(s.ns, cmd.thread, cmd.text);
				ctx.ui.notify(`☁ queued turn ${turn.id} on ${cmd.thread}`, "info");
				await this.#getWatcher(ctx, s).watch(cmd.thread);
				return;
			}
			case "answer": {
				await this.#answer(ctx, s, cmd.thread, cmd.text);
				return;
			}
			case "stop": {
				const r = await s.client.stop(s.ns, cmd.thread);
				ctx.ui.notify(`☁ stopped ${cmd.thread}: running ${r.stoppedRunning ? "yes" : "no"}, cancelled ${r.cancelled}`, "info");
				return;
			}
			case "pull":
				await this.#pull(ctx, s, cmd.thread);
				return;
			case "resume":
				await this.#resume(ctx, s, cmd.thread);
				return;
			case "delete": {
				if (!(await ctx.ui.confirm("Delete cloud thread", `Delete ${cmd.thread} and its result?`))) return;
				await s.client.deleteThread(s.ns, cmd.thread);
				this.#names = this.#names.filter(n => n !== cmd.thread);
				ctx.ui.notify(`☁ deleted ${cmd.thread}`, "info");
				return;
			}
			default:
				return;
		}
	}

	async #status(ctx: Host): Promise<void> {
		const root = await this.#repoRoot(ctx.cwd);
		const cfg = await loadConfig({ env: this.#env, repoRoot: root });
		const conn = await resolveConnection(cfg, this.#env);
		const who = await new CloudClient(conn.url, conn.token).whoami();
		const agents =
			who.agents
				.map(a => `${a.namespace}/${a.name}${a.ready ? "" : " (not ready)"}${a.capacity ? ` running ${a.capacity.running}/${a.capacity.maxConcurrentSessions}` : ""}`)
				.join(", ") || "none";
		ctx.ui.notify(
			[
				`☁ omp-cloud ${cfg.sources.join(", ") || "no config file"}`,
				`  url: ${conn.url}`,
				`  user: ${who.user}`,
				`  agents: ${agents}`,
			].join("\n"),
			"info",
		);
	}

	// -----------------------------------------------------------------------
	// Hub
	// -----------------------------------------------------------------------

	/** The overlay hub in a TUI; elsewhere (RPC, a UI without overlays) an inventory card. */
	async #hub(ctx: CommandHost, s: Session): Promise<void> {
		const tui = this.#tui;
		const custom = ctx.ui.custom?.bind(ctx.ui);
		if (ctx.mode !== "tui" || !custom || !tui) return this.#inventory(ctx, s);
		for (;;) {
			let built = false;
			let result: HubResult | undefined;
			try {
				result = await custom<HubResult | undefined>(
					(tuiHost, theme, _keybindings, done) => {
						built = true;
						const hub = new HubComponent({
							api: s.client,
							ns: s.ns,
							theme,
							tui,
							requestRender: () => tuiHost.requestRender(),
							done,
						});
						hub.start();
						return hub;
					},
					{ overlay: true, overlayOptions: { anchor: "center", width: "92%", maxHeight: "92%", minWidth: 60 } },
				);
			} catch (e) {
				if (built) throw e;
			}
			if (!built) return this.#inventory(ctx, s);
			if (!result) return;
			if (!(await this.#hubAction(ctx, s, result))) return;
		}
	}

	/** Runs one hub key; false when the hub should not reopen. */
	async #hubAction(ctx: CommandHost, s: Session, r: HubResult): Promise<boolean> {
		try {
			switch (r.action) {
				case "send": {
					const text = (await ctx.ui.input?.(`Message to ${r.thread}`, "follow-up for the cloud agent"))?.trim();
					if (!text) return true;
					const turn = await s.client.message(s.ns, r.thread, text);
					ctx.ui.notify(`☁ queued turn ${turn.id} on ${r.thread}`, "info");
					await this.#getWatcher(ctx, s).watch(r.thread);
					return true;
				}
				case "answer":
					await this.#answer(ctx, s, r.thread);
					return true;
				case "stop": {
					if (!(await ctx.ui.confirm("Stop cloud thread", `Abort the running turn of ${r.thread} and cancel queued ones?`))) return true;
					const st = await s.client.stop(s.ns, r.thread);
					ctx.ui.notify(`☁ stopped ${r.thread}: running ${st.stoppedRunning ? "yes" : "no"}, cancelled ${st.cancelled}`, "info");
					return true;
				}
				case "pull":
					await this.#pull(ctx, s, r.thread);
					return true;
				case "resume":
					await this.#resume(ctx, s, r.thread);
					return false;
				case "delete": {
					if (!(await ctx.ui.confirm("Delete cloud thread", `Delete ${r.thread} and its result?`))) return true;
					await s.client.deleteThread(s.ns, r.thread);
					this.#names = this.#names.filter(n => n !== r.thread);
					ctx.ui.notify(`☁ deleted ${r.thread}`, "info");
					return true;
				}
			}
		} catch (e) {
			ctx.ui.notify(e instanceof UserError ? e.message : `☁ ${errorText(e)}`, "error");
			return true;
		}
	}

	async #inventory(ctx: Host, s: Session): Promise<void> {
		const [who, threads] = await Promise.all([s.client.whoami(), s.client.listThreads(s.ns)]);
		this.#names = threads.map(t => t.name);
		this.#deliver.post(
			ctx,
			inventoryCard(who.user, s.ns, who.agents.filter(a => a.namespace === s.ns), threads, Date.now()),
		);
	}

	// -----------------------------------------------------------------------
	// Questions
	// -----------------------------------------------------------------------

	async #answer(ctx: Host, s: Session, thread: string, text?: string): Promise<void> {
		const out = await answerThread(s.client, s.ns, thread, ctx.ui, text);
		for (const q of out.answered) ctx.ui.notify(`☁ answered ${answerSummary(q)}`, "info");
		if (out.answered.length === 0) {
			ctx.ui.notify("☁ answer cancelled", "info");
			return;
		}
		if (out.left > 0) ctx.ui.notify(`☁ ${plural(out.left, "question")} still wait on ${thread}: /cloud answer ${shortName(thread)}`, "warning");
		await this.#getWatcher(ctx, s).watch(thread);
	}

	// -----------------------------------------------------------------------
	// Pull and resume
	// -----------------------------------------------------------------------

	async #pullBundle(s: Session, name: string): Promise<PullResult> {
		if (!s.root) throw new UserError("☁ not a git repository");
		const tmp = await mkdtemp(join(tmpdir(), "omp-cloud-pull-"));
		try {
			const bundle = join(tmp, "result.bundle");
			let headers;
			try {
				headers = await s.client.downloadResult(s.ns, name, bundle);
			} catch (e) {
				if (e instanceof ApiError && e.status === 404) throw new UserError(`☁ ${name} has no result yet`);
				throw e;
			}
			const detail = await s.client.thread(s.ns, name);
			const out = await pullBundle({
				root: s.root,
				thread: name,
				bundlePath: bundle,
				baseCommit: detail.repo.baseCommit || headers.base,
				remote: "origin",
			});
			const r = detail.result;
			return { branch: out.branch, head: out.head, filesChanged: r?.filesChanged ?? 0, insertions: r?.insertions ?? 0, deletions: r?.deletions ?? 0 };
		} finally {
			await rm(tmp, { recursive: true, force: true });
		}
	}

	async #pull(ctx: Host, s: Session, name: string): Promise<void> {
		const p = await this.#pullBundle(s, name);
		ctx.ui.notify(`☁ ${p.branch} → ${p.head.slice(0, 8)} (+${p.insertions} −${p.deletions}, ${p.filesChanged} files)`, "info");
	}

	async #resume(ctx: CommandHost, s: Session, name: string): Promise<void> {
		if (!s.root) throw new UserError("☁ not a git repository");
		if (!(await isClean(s.root))) throw new UserError("☁ commit or stash your changes first (or use /cloud pull)");
		await this.#pull(ctx, s, name);
		await git(s.root, ["switch", threadBranch(name)]);

		const tmp = await mkdtemp(join(tmpdir(), "omp-cloud-resume-"));
		try {
			const tarball = join(tmp, "conversation.tar");
			try {
				await s.client.downloadConversation(s.ns, name, tarball);
			} catch (e) {
				if (e instanceof ApiError && e.status === 404) throw new UserError(`☁ ${name} has no conversation yet`);
				throw e;
			}
			const jsonl = await extractConversation(tarball, join(tmp, "x"));
			const dest = await installConversation(jsonl, ctx.sessionManager.getSessionDir());
			const sw = await ctx.switchSession(dest);
			if (sw.cancelled) throw new UserError("☁ session switch was cancelled; the conversation was copied to the session directory");
		} finally {
			await rm(tmp, { recursive: true, force: true });
		}
		ctx.ui.notify(`☁ resumed ${name} on branch ${threadBranch(name)}`, "info");
		const detail = await s.client.thread(s.ns, name).catch(() => undefined);
		if (detail && (detail.phase === "Working" || detail.phase === "Waiting" || detail.turns.some(t => t.state === "queued" || t.state === "running"))) {
			ctx.ui.notify("☁ the cloud agent is still working; this is a snapshot", "warning");
		}
	}

	// -----------------------------------------------------------------------
	// Launch
	// -----------------------------------------------------------------------

	/** Launches are serialized per repo root. */
	async #serialized<T>(root: string, work: () => Promise<T>): Promise<T> {
		const prev = this.#chains.get(root) ?? Promise.resolve();
		const next = prev.then(work, work);
		this.#chains.set(root, next);
		try {
			return await next;
		} finally {
			if (this.#chains.get(root) === next) this.#chains.delete(root);
		}
	}

	async #launchFromCommand(ctx: Host, task: string): Promise<void> {
		try {
			const root = (await this.#repoRoot(ctx.cwd)) ?? ctx.cwd;
			const d = await this.#serialized(root, () => this.delegate(ctx, task, { origin: "user" }));
			ctx.ui.notify(`☁ ${d.name} started on ${d.namespace}/${d.agent} — /cloud watch ${shortName(d.name)}`, "info");
			for (const w of d.warnings) ctx.ui.notify(`☁ ${w}`, "warning");
		} catch (e) {
			ctx.ui.notify(e instanceof UserError ? e.message : `☁ handoff failed: ${errorText(e)}`, "error");
		}
	}

	/**
	 * Hands the current conversation and repository to a cloud agent: uploads the snapshot,
	 * posts the launch card and starts watching the thread. Used by `/cloud <task>` and by the
	 * `cloud` tool; callers serialize per repository.
	 */
	async delegate(ctx: Host, task: string, opts: LaunchOptions): Promise<Delegated> {
		const root = (await this.#repoRoot(ctx.cwd)) ?? ctx.cwd;
		const cfg = await loadConfig({ env: this.#env, repoRoot: root });
		const conn = await resolveConnection(cfg, this.#env);
		const client = new CloudClient(conn.url, conn.token);
		const who = await client.whoami();
		const want = opts.agent?.includes("/") ? opts.agent.split("/") : [undefined, opts.agent];
		const agent = selectAgent(who, {
			namespace: want[0] ?? cfg.namespace,
			agent: want[1] ?? cfg.agent,
		});
		const { response, warnings, summary } = await launchHandoff({
			host: ctx,
			pi: this.#pi,
			cfg,
			client,
			agent,
			task,
			env: this.#env,
			home: homedir(),
			agentDir: agentDir(this.#env),
			ownRoot: this.#ownRoot,
			toolCallId: opts.toolCallId,
		});
		const name = response.name;
		const all = [...warnings, ...(response.warnings ?? [])];
		this.#names = [name, ...this.#names.filter(n => n !== name)];
		this.#deliver.setOrigin(name, opts.origin);
		this.#deliver.post(
			ctx,
			launchCard({ origin: opts.origin, thread: name, namespace: agent.namespace, agent: agent.name, prompt: task, summary, warnings: all }),
		);
		try {
			await this.#getWatcher(ctx, { cfg, client, ns: agent.namespace, root }).watch(name);
		} catch (e) {
			ctx.ui.notify(`☁ cannot watch ${name}: ${errorText(e)}`, "warning");
		}
		return { name, namespace: agent.namespace, agent: agent.name, warnings: all };
	}

	// -----------------------------------------------------------------------
	// Operations for the `cloud` tool
	// -----------------------------------------------------------------------

	/** Delegation by the local model; serialized with user launches of the same repository. */
	async delegateFromTool(ctx: Host, task: string, agent: string | undefined, toolCallId: string): Promise<Delegated> {
		const root = (await this.#repoRoot(ctx.cwd)) ?? ctx.cwd;
		return this.#serialized(root, () => this.delegate(ctx, task, { origin: "tool", toolCallId, agent }));
	}

	async overview(ctx: Host): Promise<{ who: Whoami; ns: string; agents: WhoamiAgent[]; threads: ThreadSummary[] }> {
		const s = await this.#session(ctx);
		const [who, threads] = await Promise.all([s.client.whoami(), s.client.listThreads(s.ns)]);
		this.#names = threads.map(t => t.name);
		return { who, ns: s.ns, agents: who.agents.filter(a => a.namespace === s.ns), threads };
	}

	async #resolve(s: Session, token: string): Promise<string> {
		const m = resolveThread(token, await this.#threadNames(s));
		if ("error" in m) {
			throw new UserError(
				m.error === "ambiguous"
					? `"${token}" matches several threads: ${m.candidates.join(", ")}`
					: `no thread matches "${token}"`,
			);
		}
		return m.name;
	}

	async threadDetail(ctx: Host, token: string): Promise<ThreadDetail> {
		const s = await this.#session(ctx);
		return s.client.thread(s.ns, await this.#resolve(s, token));
	}

	async sendMessage(ctx: Host, token: string, text: string): Promise<{ thread: string; turn: number }> {
		const s = await this.#session(ctx);
		const thread = await this.#resolve(s, token);
		const turn = await s.client.message(s.ns, thread, text);
		await this.#getWatcher(ctx, s).watch(thread);
		return { thread, turn: turn.id };
	}

	async answerQuestion(ctx: Host, token: string, body: AnswerBody): Promise<{ thread: string; summary: string; left: number }> {
		const s = await this.#session(ctx);
		const thread = await this.#resolve(s, token);
		const detail = await s.client.thread(s.ns, thread);
		const pending = detail.turns.flatMap(t => (t.questions ?? []).filter(q => !q.answeredAt).map(q => ({ turn: t.id, q })));
		const first = pending[0];
		if (!first) throw new UserError(`${thread} has no question waiting`);
		const answered = await s.client.answer(s.ns, thread, first.turn, first.q.id, body);
		await this.#getWatcher(ctx, s).watch(thread);
		return { thread, summary: answerSummary(answered), left: pending.length - 1 };
	}

	async stopThread(ctx: Host, token: string): Promise<{ thread: string; stoppedRunning: boolean; cancelled: number }> {
		const s = await this.#session(ctx);
		const thread = await this.#resolve(s, token);
		return { thread, ...(await s.client.stop(s.ns, thread)) };
	}

	async pullThread(ctx: Host, token: string): Promise<PullResult & { thread: string }> {
		const s = await this.#session(ctx);
		const thread = await this.#resolve(s, token);
		return { thread, ...(await this.#pullBundle(s, thread)) };
	}
}
