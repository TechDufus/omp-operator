import { shortName, VERBS, verbHelp } from "./commands";
import type { Host, ThreadDetail, ThreadEvent, ThreadEvents, ThreadResult, ThreadSummary, ThreadTurn } from "./types";

export const WIDGET_KEY = "omp-cloud";
export const POLL_MS = 3000;
export const DONE_WIDGET_MS = 10 * 60 * 1000;
const MAX_FAILURES = 5;

function clip(s: string, n: number): string {
	const one = s.replace(/\s+/g, " ").trim();
	return one.length > n ? `${one.slice(0, n - 1)}…` : one;
}

function firstLines(text: string | undefined, n: number): string[] {
	return (text ?? "").trimEnd().split("\n").slice(0, n);
}

export function resultLine(r: ThreadResult | null | undefined): string {
	if (!r) return "result: none yet";
	const pushed = r.pushed ? "pushed" : r.pushError ? `push failed: ${clip(r.pushError, 80)}` : "not pushed";
	return `result: ${r.branch} @ ${r.head.slice(0, 8)} (+${r.insertions} −${r.deletions}, ${r.filesChanged} files, ${pushed})`;
}

export function listLines(threads: ThreadSummary[], max = 20): string[] {
	if (threads.length === 0) return ["☁ no cloud threads"];
	const lines = threads
		.slice(0, max)
		.map(t => `${shortName(t.name)} ${t.phase || "-"} t${t.turns} ${t.latest?.state ?? "-"} ${t.title}`);
	return ["☁ cloud threads", ...lines];
}

export function showLines(t: ThreadDetail, events: ThreadEvent[]): string[] {
	const latest = [...t.turns].reverse().find(x => x.state === "done" || x.state === "failed");
	const lines = [`☁ ${t.name} · ${t.phase || "-"} · ${t.turns.length} turns · ${t.agent} · ${t.model || "agent default"}`, `  ${t.title}`];
	if (latest) {
		lines.push(`  turn ${latest.id} ${latest.state}${latest.title ? `: ${latest.title}` : ""}`);
		for (const l of firstLines(latest.state === "failed" ? latest.error : latest.reply, 12)) lines.push(`    ${l}`);
	}
	lines.push(`  ${resultLine(t.result)}`);
	for (const e of events.slice(-8)) lines.push(`  ${e.kind}: ${clip(e.text, 120)}`);
	return lines;
}

export function liveLines(t: Pick<ThreadDetail, "name" | "phase" | "turns">, activeTurn: number, events: ThreadEvent[]): string[] {
	const lines = [`☁ ${t.name} · ${t.phase || "-"} · turn ${activeTurn || t.turns.length}`];
	for (const e of events.slice(-10)) lines.push(`${e.kind}: ${clip(e.text, 120)}`);
	return lines;
}

export function doneLines(t: Pick<ThreadSummary, "name">, turn: ThreadTurn): string[] {
	const head = `☁ ${t.name} · turn ${turn.id} ${turn.state}`;
	const body = turn.state === "failed" ? firstLines(turn.error || turn.reply, 8) : firstLines(turn.reply, 8);
	return [head, ...body, `/cloud pull ${shortName(t.name)}`];
}

export function helpLines(): string[] {
	const usage: Record<string, string> = {
		new: "/cloud [new] <task>",
		list: "/cloud [list]",
		show: "/cloud show <id>",
		watch: "/cloud watch <id>",
		unwatch: "/cloud unwatch",
		send: "/cloud send <id> <message>",
		stop: "/cloud stop <id>",
		pull: "/cloud pull <id>",
		resume: "/cloud resume <id>",
		delete: "/cloud delete <id>",
		status: "/cloud status",
		help: "/cloud help",
	};
	return ["☁ omp-cloud", ...VERBS.map(v => `${(usage[v] ?? v).padEnd(28)} ${verbHelp(v)}`)];
}

function isActive(t: ThreadDetail): boolean {
	return t.phase === "Working" || t.turns.some(x => x.state === "queued" || x.state === "running");
}

export interface WatchApi {
	thread(ns: string, name: string): Promise<ThreadDetail>;
	events(ns: string, name: string, since?: string): Promise<ThreadEvents>;
}

interface Watched {
	name: string;
	since?: string;
	events: ThreadEvent[];
	/** Turn ids whose completion was already reported. */
	reported: Set<number>;
	detail?: ThreadDetail;
}

/**
 * Polls watched threads every few seconds while any is working, keeps the
 * live `omp-cloud` widget and the footer status current, and reports each
 * finished turn once.
 */
export class Watcher {
	#host: Host;
	readonly #api: WatchApi;
	readonly #ns: string;
	readonly #watched = new Map<string, Watched>();
	#timer: Timer | undefined;
	#doneTimer: Timer | undefined;
	#busy = false;
	#failures = 0;
	#lastLines = "";

	constructor(host: Host, api: WatchApi, ns: string) {
		this.#host = host;
		this.#api = api;
		this.#ns = ns;
	}

	get watching(): string[] {
		return [...this.#watched.keys()];
	}

	/** Point the watcher at a new session's host after `switchSession`. */
	rebind(host: Host): void {
		this.#host = host;
		if (this.#timer) {
			this.#timer = undefined; // the old session's timers were cleared by the host
			this.#armTimer();
		}
	}

	/** Shows static lines in the widget slot (list, show, help). */
	showLines(lines: string[]): void {
		this.#render(lines, 24);
	}

	async watch(name: string): Promise<void> {
		const detail = await this.#api.thread(this.#ns, name);
		const w: Watched = {
			name,
			events: [],
			reported: new Set(detail.turns.filter(t => t.state !== "queued" && t.state !== "running").map(t => t.id)),
			detail,
		};
		const ev = await this.#api.events(this.#ns, name).catch((): ThreadEvents => ({ session: "", events: [] }));
		w.events = ev.events.slice(-10);
		w.since = ev.events.at(-1)?.at;
		this.#watched.set(name, w);
		this.#clearDoneTimer();
		if (isActive(detail)) {
			this.#renderThread(w);
			this.#updateStatus();
			this.#armTimer();
		} else {
			this.#render(showLines(detail, w.events), 24);
			this.#watched.delete(name);
			this.#updateStatus();
		}
	}

	/** Stops polling and clears the widget and the footer status. */
	unwatch(): void {
		this.#watched.clear();
		this.#stopTimer();
		this.#clearDoneTimer();
		this.#lastLines = "";
		this.#host.ui.setWidget(WIDGET_KEY, undefined);
		this.#host.ui.setStatus(WIDGET_KEY, undefined);
	}

	#armTimer(): void {
		if (this.#timer) return;
		this.#timer = this.#host.setInterval(() => void this.#tick(), POLL_MS);
	}

	#stopTimer(): void {
		if (this.#timer) {
			this.#host.clearTimer(this.#timer);
			this.#timer = undefined;
		}
	}

	#clearDoneTimer(): void {
		if (this.#doneTimer) {
			this.#host.clearTimer(this.#doneTimer);
			this.#doneTimer = undefined;
		}
	}

	#render(lines: string[], max = 12): void {
		const key = lines.join("\n");
		if (key === this.#lastLines) return;
		this.#lastLines = key;
		this.#host.ui.setWidget(WIDGET_KEY, lines.slice(0, max), { placement: "aboveEditor" });
	}

	#renderThread(w: Watched): void {
		const d = w.detail;
		if (!d) return;
		const active = d.turns.find(t => t.state === "running") ?? d.turns.find(t => t.state === "queued");
		this.#render(liveLines(d, active?.id ?? 0, w.events));
	}

	#updateStatus(): void {
		const k = [...this.#watched.values()].filter(w => w.detail && isActive(w.detail)).length;
		this.#host.ui.setStatus(WIDGET_KEY, k > 0 ? `☁ ${k} working` : undefined);
	}

	async #tick(): Promise<void> {
		if (this.#busy) return;
		this.#busy = true;
		try {
			for (const w of [...this.#watched.values()]) await this.#poll(w);
			this.#failures = 0;
		} catch (e) {
			if (++this.#failures >= MAX_FAILURES) {
				this.#host.ui.notify(`☁ lost contact with the cloud API: ${(e as Error).message}`, "error");
				this.unwatch();
			}
		} finally {
			this.#busy = false;
		}
		if (this.#watched.size === 0) this.#stopTimer();
	}

	async #poll(w: Watched): Promise<void> {
		const [detail, ev] = await Promise.all([this.#api.thread(this.#ns, w.name), this.#api.events(this.#ns, w.name, w.since)]);
		w.detail = detail;
		if (ev.events.length > 0) {
			w.events = [...w.events, ...ev.events].slice(-10);
			w.since = ev.events.at(-1)?.at ?? w.since;
		}
		for (const turn of detail.turns) {
			if ((turn.state !== "done" && turn.state !== "failed") || w.reported.has(turn.id)) continue;
			w.reported.add(turn.id);
			const what = turn.state === "failed" ? turn.error : turn.title;
			this.#host.ui.notify(
				`☁ ${w.name} turn ${turn.id} ${turn.state}${what ? `: ${clip(what, 200)}` : ""}`,
				turn.state === "failed" ? "error" : "info",
			);
			this.#render(doneLines(detail, turn));
			this.#clearDoneTimer();
			this.#doneTimer = this.#host.setTimeout(() => {
				this.#lastLines = "";
				this.#host.ui.setWidget(WIDGET_KEY, undefined);
			}, DONE_WIDGET_MS);
		}
		if (isActive(detail)) {
			this.#clearDoneTimer();
			this.#renderThread(w);
		} else {
			this.#watched.delete(w.name);
		}
		this.#updateStatus();
	}
}
