/**
 * The `/cloud` hub: an overlay listing the cloud agents with their capacity and the namespace's
 * threads, with the selected thread's latest reply, pending question and live events. Keys act
 * on the selected thread; the overlay closes with the action and the command runs it, then
 * reopens the hub.
 */

import type { Component, KeyId, Theme } from "@oh-my-pi/pi-tui";
import type { TuiModule } from "./cards";
import { clip, firstLines, formatAge, phaseIcon, plural } from "./format";
import { shortName } from "./commands";
import type { ThreadDetail, ThreadEvent, ThreadEvents, ThreadSummary, Whoami, WhoamiAgent } from "./types";

export type HubAction = "send" | "answer" | "stop" | "pull" | "resume" | "delete";

export interface HubResult {
	action: HubAction;
	thread: string;
}

export interface HubApi {
	whoami(): Promise<Whoami>;
	listThreads(ns: string): Promise<ThreadSummary[]>;
	thread(ns: string, name: string): Promise<ThreadDetail>;
	events(ns: string, name: string, since?: string): Promise<ThreadEvents>;
}

export interface HubDeps {
	api: HubApi;
	ns: string;
	theme: Pick<Theme, "fg" | "bold">;
	tui: Pick<TuiModule, "truncateToWidth" | "matchesKey">;
	requestRender(): void;
	done(result: HubResult | undefined): void;
	now?(): number;
	pollMs?: number;
}

/** Rows of the thread list shown at once. */
const LIST_ROWS = 10;
const REFRESH_AGENTS_EVERY = 5;
const KEY_ACTIONS: [KeyId, HubAction][] = [
	["s", "send"],
	["a", "answer"],
	["x", "stop"],
	["p", "pull"],
	["r", "resume"],
	["d", "delete"],
];

/** The hub overlay. `start` begins polling; `dispose` (called by the host) stops it. */
export class HubComponent implements Component {
	readonly #d: HubDeps;
	#agents: WhoamiAgent[] = [];
	#user = "";
	#threads: ThreadSummary[] = [];
	#sel = 0;
	#detail: ThreadDetail | undefined;
	#events: ThreadEvent[] = [];
	#error = "";
	#loaded = false;
	#running: Promise<void> | undefined;
	#again = false;
	#ticks = 0;
	#timer: Timer | undefined;
	#closed = false;

	constructor(deps: HubDeps) {
		this.#d = deps;
	}

	/** Loads once, then polls every `pollMs` (3 s). */
	start(): void {
		void this.refresh();
		this.#timer = setInterval(() => void this.refresh(), this.#d.pollMs ?? 3000);
	}

	dispose(): void {
		this.#closed = true;
		clearInterval(this.#timer);
		this.#timer = undefined;
	}

	invalidate(): void {}

	get selected(): ThreadSummary | undefined {
		return this.#threads[this.#sel];
	}

	/**
	 * Refreshes agents (every few polls), threads and the selected thread's detail and events.
	 * A call while one is running makes it run once more afterwards, so a selection change is never dropped.
	 */
	refresh(): Promise<void> {
		if (this.#closed) return Promise.resolve();
		if (this.#running) {
			this.#again = true;
			return this.#running;
		}
		this.#running = (async () => {
			do {
				this.#again = false;
				await this.#load();
			} while (this.#again && !this.#closed);
		})().finally(() => {
			this.#running = undefined;
		});
		return this.#running;
	}

	async #load(): Promise<void> {
		try {
			const { api, ns } = this.#d;
			if (this.#ticks++ % REFRESH_AGENTS_EVERY === 0) {
				const who = await api.whoami();
				this.#user = who.user;
				this.#agents = who.agents.filter(a => a.namespace === ns);
			}
			const prev = this.selected?.name;
			this.#threads = await api.listThreads(ns);
			const keep = prev ? this.#threads.findIndex(t => t.name === prev) : -1;
			this.#sel = keep >= 0 ? keep : Math.min(this.#sel, Math.max(0, this.#threads.length - 1));
			const t = this.selected;
			if (t) {
				const [detail, ev] = await Promise.all([api.thread(ns, t.name), api.events(ns, t.name)]);
				if (this.selected?.name === t.name) {
					this.#detail = detail;
					this.#events = ev.events.slice(-12);
				}
			} else {
				this.#detail = undefined;
				this.#events = [];
			}
			this.#error = "";
		} catch (e) {
			this.#error = e instanceof Error ? e.message : String(e);
		} finally {
			this.#loaded = true;
			if (!this.#closed) this.#d.requestRender();
		}
	}

	handleInput(data: string): void {
		const { matchesKey } = this.#d.tui;
		if (matchesKey(data, "escape") || matchesKey(data, "q") || matchesKey(data, "ctrl+c")) {
			this.#d.done(undefined);
			return;
		}
		if (matchesKey(data, "up") || matchesKey(data, "k")) return this.#move(-1);
		if (matchesKey(data, "down") || matchesKey(data, "j")) return this.#move(1);
		const t = this.selected;
		if (!t) return;
		for (const [key, action] of KEY_ACTIONS) {
			if (matchesKey(data, key)) {
				this.#d.done({ action, thread: t.name });
				return;
			}
		}
	}

	#move(delta: number): void {
		const next = Math.max(0, Math.min(this.#threads.length - 1, this.#sel + delta));
		if (next === this.#sel) return;
		this.#sel = next;
		this.#detail = undefined;
		this.#events = [];
		this.#d.requestRender();
		void this.refresh();
	}

	render(width: number): readonly string[] {
		const { theme, tui } = this.#d;
		const now = this.#d.now?.() ?? Date.now();
		const fit = (s: string): string => tui.truncateToWidth(s, Math.max(10, width));
		const lines: string[] = [];
		const rule = theme.fg("borderMuted", "─".repeat(Math.max(10, width)));

		lines.push(theme.bold(theme.fg("accent", `☁ omp-cloud${this.#user ? ` · ${this.#user}` : ""} · ${this.#d.ns}`)));
		if (this.#error) lines.push(theme.fg("error", `! ${clip(this.#error, 200)}`));
		if (!this.#loaded) lines.push(theme.fg("muted", "loading…"));

		lines.push(theme.fg("muted", "Agents"));
		for (const a of this.#agents) {
			const cap = a.capacity ? `running ${a.capacity.running}/${a.capacity.maxConcurrentSessions}` : "";
			const th = a.threads ? `${a.threads.working} working · ${a.threads.waiting} waiting · ${a.threads.idle} idle` : "";
			const ready = a.ready ? theme.fg("success", "ready") : theme.fg("warning", "not ready");
			lines.push(`  ${a.name}  ${[ready, cap, th].filter(Boolean).join(" · ")}`);
		}
		if (this.#agents.length === 0 && this.#loaded) lines.push("  none");

		lines.push(theme.fg("muted", `Threads (${this.#threads.length})`));
		const first = Math.max(0, Math.min(this.#sel - Math.floor(LIST_ROWS / 2), this.#threads.length - LIST_ROWS));
		for (const [i, t] of this.#threads.slice(first, first + LIST_ROWS).entries()) {
			const at = first + i;
			const mark = at === this.#sel ? theme.fg("accent", "▸") : " ";
			const icon = t.question ? theme.fg("warning", "?") : phaseIcon(t.phase);
			const diff = t.result ? `+${t.result.insertions}/−${t.result.deletions}` : "";
			const row = `${mark} ${icon} ${shortName(t.name)} ${(t.phase || "-").padEnd(7)} t${String(t.turns).padEnd(2)} ${(t.latest?.state ?? "-").padEnd(7)} ${diff.padEnd(9)} ${formatAge(t.lastActivityAt ?? t.createdAt, now).padEnd(4)} ${clip(t.title, 80)}`;
			lines.push(at === this.#sel ? theme.bold(row) : row);
		}
		if (this.#threads.length === 0 && this.#loaded) lines.push("  no threads — hand off work with /cloud <task>");

		const d = this.#detail;
		if (d) {
			lines.push(rule);
			lines.push(theme.bold(`${d.name} · ${d.phase || "-"} · ${plural(d.turns.length, "turn")} · ${d.agent} · ${d.model || "agent default"}`));
			const q = d.question;
			if (q) {
				lines.push(theme.fg("warning", `? ${q.header ? `${q.header}: ` : ""}${clip(q.text, 400)}${q.remaining > 1 ? ` (+${q.remaining - 1} more)` : ""}`));
				(q.options ?? []).forEach((o, i) => {
					lines.push(`   ${q.multi ? "[ ]" : "(•)"} ${o.label}${q.recommended === i ? theme.fg("success", "  ← recommended") : ""}${o.description ? theme.fg("muted", ` — ${clip(o.description, 100)}`) : ""}`);
				});
			}
			const latest = [...d.turns].reverse().find(x => x.state === "done" || x.state === "failed");
			if (latest) {
				lines.push(theme.fg("muted", `Latest — turn ${latest.id} ${latest.state}${latest.title ? `: ${latest.title}` : ""}`));
				for (const l of firstLines(latest.state === "failed" ? latest.error : latest.reply, 6)) lines.push(`  ${l}`);
			}
			if (this.#events.length > 0) {
				lines.push(theme.fg("muted", "Live"));
				for (const e of this.#events.slice(-6)) lines.push(theme.fg("dim", `  ${e.kind}: ${clip(e.text, 140)}`));
			}
		}
		lines.push(rule);
		lines.push(theme.fg("dim", "↑↓ select · s send · a answer · x stop · p pull · r resume · d delete · q close"));
		return lines.map(fit);
	}
}
