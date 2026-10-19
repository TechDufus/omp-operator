/** Answering the questions a cloud agent asks: finding the pending ones, parsing typed answers, prompting. */

import type { CloudClient } from "./api";
import { clip } from "./format";
import { UserError, type AnswerBody, type HostUi, type Question, type QuestionOption, type ThreadDetail } from "./types";

/** The choice that lets the user type a free-text answer. */
export const OTHER = "Other (type your own)";
const DONE = "Done";

export interface Pending {
	turn: number;
	/** Unanswered questions of the oldest pending ask, in order. */
	questions: Question[];
}

function options(q: Pick<Question, "options">): QuestionOption[] {
	return q.options ?? [];
}

/**
 * The questions the thread is waiting on: the unanswered ones of the oldest pending ask of
 * the turn that asked first.
 */
export function pendingQuestions(detail: Pick<ThreadDetail, "turns">): Pending | undefined {
	for (const turn of detail.turns) {
		const open = (turn.questions ?? []).filter(q => !q.answeredAt);
		if (open.length === 0) continue;
		const oldest = Math.min(...open.map(q => q.ask));
		return { turn: turn.id, questions: open.filter(q => q.ask === oldest).sort((a, b) => a.id - b.id) };
	}
	return undefined;
}

/**
 * A typed answer: text equal to an option label (ignoring case) selects it; for a multi-select
 * question a comma-separated list of labels selects them all; anything else is free text.
 */
export function parseAnswerText(q: Pick<Question, "options" | "multi">, text: string): AnswerBody {
	const t = text.trim();
	const labels = options(q).map(o => o.label);
	const find = (s: string): string | undefined => labels.find(l => l.toLowerCase() === s.trim().toLowerCase());
	const whole = find(t);
	if (whole) return { selected: [whole] };
	if (q.multi) {
		const parts = t.split(",").map(p => p.trim()).filter(p => p !== "");
		const picked = parts.map(find);
		if (parts.length > 1 && picked.every((p): p is string => p !== undefined)) return { selected: [...new Set(picked)] };
	}
	return { answer: t };
}

function optionLabel(o: QuestionOption, i: number, recommended: number | null | undefined): string {
	return `${o.label}${recommended === i ? " (recommended)" : ""}${o.description ? ` — ${clip(o.description, 100)}` : ""}`;
}

/** Prompts for one question. Undefined when the user cancels. */
export async function askUser(ui: HostUi, thread: string, q: Question): Promise<AnswerBody | undefined> {
	const title = `${thread}${q.header ? ` · ${q.header}` : ""}: ${clip(q.text, 300)}`;
	const opts = options(q);
	if (opts.length === 0 ? !ui.input : !ui.select) {
		throw new UserError("☁ this host cannot prompt; answer with /cloud answer <id> <text>");
	}
	const typed = async (): Promise<string | undefined> => (await ui.input?.(title, "your answer"))?.trim() || undefined;
	if (opts.length === 0) {
		const t = await typed();
		return t ? { answer: t } : undefined;
	}
	const chosen: string[] = [];
	for (;;) {
		const left = opts.map((o, i) => ({ o, i })).filter(({ o }) => !chosen.includes(o.label));
		const done = `${DONE} (${chosen.length} selected)`;
		const items = [...left.map(({ o, i }) => optionLabel(o, i, q.recommended)), ...(q.multi && chosen.length > 0 ? [done] : []), OTHER];
		const picked = await ui.select?.(q.multi ? `${title} (select several)` : title, items);
		if (picked === undefined) return undefined;
		if (q.multi && picked === done) return { selected: chosen };
		if (picked === OTHER) {
			const t = await typed();
			if (!t) return undefined;
			return chosen.length > 0 ? { selected: chosen, answer: t } : { answer: t };
		}
		const hit = left[items.indexOf(picked)];
		if (!hit) return undefined;
		if (!q.multi) return { selected: [hit.o.label] };
		chosen.push(hit.o.label);
	}
}

export interface AnswerOutcome {
	answered: Question[];
	/** Questions of the ask that still wait (the user cancelled before them). */
	left: number;
}

/**
 * Answers the thread's pending question(s). With `text`, answers the first one (a label selects
 * it, anything else is free text); without, prompts for each question of the oldest pending ask
 * in order and stops at the first cancel.
 */
export async function answerThread(
	client: Pick<CloudClient, "thread" | "answer">,
	ns: string,
	thread: string,
	ui: HostUi,
	text?: string,
): Promise<AnswerOutcome> {
	const detail = await client.thread(ns, thread);
	const pending = pendingQuestions(detail);
	if (!pending) throw new UserError(`☁ ${thread} has no question waiting`);
	const answered: Question[] = [];
	for (const q of pending.questions) {
		const body = text !== undefined && text.trim() !== "" ? parseAnswerText(q, text) : await askUser(ui, thread, q);
		if (!body) break;
		answered.push(await client.answer(ns, thread, pending.turn, q.id, body));
		if (text !== undefined && text.trim() !== "") break;
	}
	return { answered, left: pending.questions.length - answered.length };
}

/** One line describing an answer, for notifications and tool results. */
export function answerSummary(q: Question): string {
	const parts = [...(q.selected ?? []), ...(q.answer ? [q.answer] : [])];
	return `"${clip(q.text, 60)}" → ${clip(parts.join(", "), 80)}`;
}
