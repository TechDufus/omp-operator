/** Small pure helpers shared by the cards, the hub, the widgets and the tool. */

import type { ThreadTurn, TurnState } from "./types";

/** One line, whitespace collapsed, cut to `n` characters with an ellipsis. */
export function clip(s: string, n: number): string {
	const one = s.replace(/\s+/g, " ").trim();
	return one.length > n ? `${one.slice(0, n - 1)}…` : one;
}

/** At most `n` lines of `text`, trailing whitespace dropped. */
export function firstLines(text: string | undefined, n: number): string[] {
	return (text ?? "").trimEnd().split("\n").slice(0, n);
}

/** `text` cut to `max` characters; a cut is marked and says how much was left out. */
export function truncateText(text: string, max: number): string {
	if (text.length <= max) return text;
	return `${text.slice(0, max).trimEnd()}… [${text.length - max} more characters]`;
}

/** `4m12s`, `38s`, `2h05m`; "" for a missing or negative span. */
export function formatDuration(ms: number | undefined): string {
	if (ms === undefined || !Number.isFinite(ms) || ms < 0) return "";
	const s = Math.round(ms / 1000);
	if (s < 60) return `${s}s`;
	const m = Math.floor(s / 60);
	if (m < 60) return `${m}m${String(s % 60).padStart(2, "0")}s`;
	return `${Math.floor(m / 60)}h${String(m % 60).padStart(2, "0")}m`;
}

/** The span between two RFC 3339 timestamps, formatted; "" when either is missing. */
export function spanBetween(from: string | undefined, to: string | undefined): string {
	if (!from || !to) return "";
	return formatDuration(Date.parse(to) - Date.parse(from));
}

/** `3m`, `2h`, `5d` since `at`; "" when unknown. */
export function formatAge(at: string | undefined, now: number): string {
	if (!at) return "";
	const ms = now - Date.parse(at);
	if (!Number.isFinite(ms) || ms < 0) return "";
	const m = Math.floor(ms / 60_000);
	if (m < 1) return "now";
	if (m < 60) return `${m}m`;
	const h = Math.floor(m / 60);
	return h < 48 ? `${h}h` : `${Math.floor(h / 24)}d`;
}

/** A cost string or number as `$0.42`; "" when absent or zero. */
export function formatCost(cost: string | number | undefined): string {
	const n = typeof cost === "string" ? Number(cost) : cost;
	if (n === undefined || !Number.isFinite(n) || n <= 0) return "";
	return `$${n < 0.01 ? n.toFixed(4) : n.toFixed(2)}`;
}

/** The icon for a thread phase. */
export function phaseIcon(phase: string): string {
	switch (phase) {
		case "Working":
			return "●";
		case "Waiting":
			return "?";
		case "Idle":
			return "○";
		default:
			return "·";
	}
}

export function stateIcon(state: TurnState): string {
	switch (state) {
		case "done":
			return "✓";
		case "failed":
			return "✗";
		case "running":
			return "●";
		case "queued":
			return "…";
		default:
			return "–";
	}
}

/** `+ins −del · N files`, with the zero case spelled out. */
export function diffstat(t: Pick<ThreadTurn, "filesChanged" | "insertions" | "deletions">): string {
	const files = t.filesChanged ?? 0;
	if (files === 0 && !t.insertions && !t.deletions) return "no changes";
	return `+${t.insertions ?? 0} −${t.deletions ?? 0} · ${files} file${files === 1 ? "" : "s"}`;
}

/** The cost of a turn, from its usage block. */
export function turnCost(t: Pick<ThreadTurn, "usage">): string {
	return formatCost(t.usage?.cost);
}

export function plural(n: number, one: string, many: string = `${one}s`): string {
	return `${n} ${n === 1 ? one : many}`;
}
