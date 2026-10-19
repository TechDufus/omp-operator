import { mkdtemp, rm } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import { ApiError, CloudClient } from "./api";
import {
	agentDir,
	loadConfig,
	resolveConnection,
	selectAgent,
	selectNamespace,
	type CloudConfig,
	type Env,
} from "./config";
import {
	completions,
	parseCommand,
	resolveCommand,
	shortName,
	type Command,
	type Completion,
} from "./commands";
import { launchHandoff } from "./launch";
import { extractConversation, installConversation, isClean, pullBundle, threadBranch } from "./pull";
import { git, gitRaw } from "./snapshot";
import { UserError, type CommandHost, type Host, type PiHost } from "./types";
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

/** Runs `/cloud` commands. One instance per loaded extension. */
export class Cloud {
	readonly #pi: PiHost;
	readonly #ownRoot: string;
	readonly #env: Env;
	#names: string[] = [];
	#watcher: Watcher | undefined;
	#watcherKey = "";
	readonly #chains = new Map<string, Promise<void>>();

	constructor(pi: PiHost, ownRoot: string, env: Env = process.env) {
		this.#pi = pi;
		this.#ownRoot = ownRoot;
		this.#env = env;
	}

	/** Synchronous completion from the in-memory thread cache; never touches the network. */
	complete(prefix: string): Completion[] | null {
		return completions(prefix, this.#names);
	}

	/** Re-points the live watcher at the host of a new session. */
	rebind(host: Host): void {
		this.#watcher?.rebind(host);
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
			this.#watcher = new Watcher(ctx, s.client, s.ns);
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
			ctx.setTimeout(() => void this.#enqueueLaunch(ctx, task), 0);
			return;
		}
		if (parsed.kind === "status") return this.#status(ctx);

		const s = await this.#session(ctx);
		let cmd: Command;
		if (parsed.kind === "list") {
			cmd = { kind: "list" };
		} else {
			const threads = await s.client.listThreads(s.ns);
			this.#names = threads.map(t => t.name);
			cmd = resolveCommand(parsed, this.#names);
		}
		await this.#run(cmd, ctx, s);
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
		const agents = who.agents.map(a => `${a.namespace}/${a.name}${a.ready ? "" : " (not ready)"}`).join(", ") || "none";
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

	async #pull(ctx: Host, s: Session, name: string): Promise<void> {
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
			ctx.ui.notify(
				`☁ ${out.branch} → ${out.head.slice(0, 8)} (+${r?.insertions ?? 0} −${r?.deletions ?? 0}, ${r?.filesChanged ?? 0} files)`,
				"info",
			);
		} finally {
			await rm(tmp, { recursive: true, force: true });
		}
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
		if (detail && (detail.phase === "Working" || detail.turns.some(t => t.state === "queued" || t.state === "running"))) {
			ctx.ui.notify("☁ the cloud agent is still working; this is a snapshot", "warning");
		}
	}

	/** Launches are serialized per repo root. */
	async #enqueueLaunch(ctx: Host, task: string): Promise<void> {
		const root = (await this.#repoRoot(ctx.cwd)) ?? ctx.cwd;
		const prev = this.#chains.get(root) ?? Promise.resolve();
		const next = prev.then(() => this.#launch(ctx, task, root));
		this.#chains.set(root, next);
		await next;
		if (this.#chains.get(root) === next) this.#chains.delete(root);
	}

	async #launch(ctx: Host, task: string, root: string): Promise<void> {
		try {
			const cfg = await loadConfig({ env: this.#env, repoRoot: root });
			const conn = await resolveConnection(cfg, this.#env);
			const client = new CloudClient(conn.url, conn.token);
			const who = await client.whoami();
			const agent = selectAgent(who, cfg);
			const { response, warnings } = await launchHandoff({
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
			});
			const name = response.name;
			ctx.ui.notify(`☁ ${name} started on ${agent.namespace}/${agent.name} — /cloud watch ${shortName(name)}`, "info");
			for (const w of [...warnings, ...(response.warnings ?? [])]) ctx.ui.notify(`☁ ${w}`, "warning");
			this.#names = [name, ...this.#names.filter(n => n !== name)];
			if (ctx.isIdle()) {
				this.#pi.sendMessage(
					{ customType: "omp-cloud", content: `Handed off to cloud thread ${name}: ${task}`, display: true },
					{ triggerTurn: false },
				);
			}
			try {
				await this.#getWatcher(ctx, { cfg, client, ns: agent.namespace, root }).watch(name);
			} catch (e) {
				ctx.ui.notify(`☁ cannot watch ${name}: ${errorText(e)}`, "warning");
			}
		} catch (e) {
			ctx.ui.notify(e instanceof UserError ? e.message : `☁ handoff failed: ${errorText(e)}`, "error");
		}
	}
}
