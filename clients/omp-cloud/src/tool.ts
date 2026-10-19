/**
 * The `cloud` tool: lets the local model delegate to cloud agents and manage the threads it
 * started, with the capacity information it needs to decide. Results and questions come back
 * later as `[cloud]` messages; the model is told not to poll.
 */

import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { threadBranch } from "./cards";
import { clip, diffstat, plural, truncateText } from "./format";
import { UserError, type AnswerBody, type Host, type PendingQuestion, type ThreadDetail, type ThreadSummary, type WhoamiAgent } from "./types";
import type { Cloud } from "./verbs";

export const TOOL_NAME = "cloud";
/** The name used when `cloud` is already taken by another tool. */
export const TOOL_NAME_FALLBACK = "cloud_agent";

/** Reply length the model sees per turn. */
export const STATUS_REPLY_MAX = 1500;
const STATUS_TURNS = 3;
const LIST_THREADS = 20;

export const TOOL_OPS = ["delegate", "list", "status", "send", "answer", "stop", "pull"] as const;
export type ToolOp = (typeof TOOL_OPS)[number];

export interface ToolParams {
	op: ToolOp;
	task?: string;
	agent?: string;
	thread?: string;
	text?: string;
	selected?: string[];
	answer?: string;
}

export interface ToolOutput {
	text: string;
	details: Record<string, unknown>;
}

export const TOOL_DESCRIPTION = [
	"Delegate work to a cloud agent running on omp-operator, or manage the cloud threads you started.",
	"",
	"op=delegate {task, agent?}: hand off work that is independent of this machine and long-running or parallel: broad refactors, full test-suite runs, research, or anything the user can leave running while you continue. The cloud agent gets this conversation, the repository including uncommitted changes, and the user's omp setup, and commits to branch omp-cloud/<thread>. Write `task` as a self-contained instruction. Do not delegate quick edits, work that needs this machine's services or display, or anything the user wants to watch live.",
	"op=list: cloud agents with capacity (sessions running of the maximum, threads working, waiting, idle) and recent threads. Check it before delegating; a full agent queues new work behind running sessions.",
	"op=status {thread}: phase, the last turns' replies, a pending question and the result. Use it only when asked: results arrive later as [cloud] messages in this conversation, so do not poll.",
	"op=send {thread, text}: queue a follow-up turn. op=stop {thread}: abort the running turn and cancel queued ones.",
	"op=answer {thread, selected?, answer?}: answer the question a thread is waiting on, `selected` being option labels and `answer` free text. Answer only when this conversation settles it; otherwise ask the user.",
	"op=pull {thread}: fetch the result branch omp-cloud/<thread> into the local repository (the working tree is not touched).",
	"`thread` is the full name or its last 6 characters.",
].join("\n");

/** Validates the arguments the model sent against {@link ToolParams}. */
export function parseToolParams(v: unknown): ToolParams {
	if (v === null || typeof v !== "object") throw new UserError("cloud: expected an object of arguments");
	const str = (key: string): string | undefined => {
		const x: unknown = Reflect.get(v, key);
		return typeof x === "string" ? x : undefined;
	};
	const op = TOOL_OPS.find(o => o === str("op"));
	if (!op) throw new UserError(`cloud: op must be one of ${TOOL_OPS.join(", ")}`);
	const selected: unknown = Reflect.get(v, "selected");
	return {
		op,
		task: str("task"),
		agent: str("agent"),
		thread: str("thread"),
		text: str("text"),
		selected: Array.isArray(selected) ? selected.filter((s): s is string => typeof s === "string") : undefined,
		answer: str("answer"),
	};
}

/** The approval tier of a call: reads are free, thread changes write, delegation ships the workspace. */
export function toolApproval(args: unknown): "read" | "write" | { tier: "exec"; reason: string } {
	const op = args !== null && typeof args === "object" && "op" in args ? args.op : undefined;
	switch (op) {
		case "list":
		case "status":
			return "read";
		case "send":
		case "answer":
		case "stop":
		case "pull":
			return "write";
		default:
			return { tier: "exec", reason: "ships the repository, this conversation and your omp setup to a cloud agent" };
	}
}

/** `cloud`, or `cloud_agent` when another tool already has that name. */
export function chooseToolName(existing: readonly string[]): string {
	return existing.includes(TOOL_NAME) ? TOOL_NAME_FALLBACK : TOOL_NAME;
}

function need(value: string | undefined, what: string, op: string): string {
	const v = value?.trim();
	if (!v) throw new UserError(`${op} needs ${what}`);
	return v;
}

function questionText(q: PendingQuestion): string {
	const opts = (q.options ?? []).map((o, i) => `${i + 1}. ${o.label}${o.description ? ` — ${clip(o.description, 120)}` : ""}${q.recommended === i ? " (recommended)" : ""}`);
	return [
		`Waiting for an answer (turn ${q.turn}${q.remaining > 1 ? `, ${q.remaining} questions` : ""}): ${q.header ? `${q.header}: ` : ""}${clip(q.text, 600)}`,
		...(opts.length > 0 ? [`Options${q.multi ? " (several allowed)" : ""}: ${opts.join("; ")}; free text is always allowed`] : []),
	].join("\n");
}

function agentLine(a: WhoamiAgent): string {
	const parts = [a.ready ? "ready" : "not ready"];
	if (a.capacity) parts.push(`running ${a.capacity.running} of ${a.capacity.maxConcurrentSessions} sessions`);
	if (a.threads) parts.push(`${a.threads.working} working, ${a.threads.waiting} waiting, ${a.threads.idle} idle threads`);
	parts.push(`default model ${a.defaultModel}`);
	return `- ${a.namespace}/${a.name}: ${parts.join("; ")}`;
}

/** The `list` result text. */
export function overviewText(user: string, ns: string, agents: WhoamiAgent[], threads: ThreadSummary[]): string {
	const lines = [`Cloud agents in ${ns} (you are ${user}):`, ...(agents.length > 0 ? agents.map(agentLine) : ["- none"])];
	lines.push(`Threads (newest first, up to ${LIST_THREADS}):`);
	for (const t of threads.slice(0, LIST_THREADS)) {
		lines.push(`- ${t.name} [${t.phase || "-"}] ${t.turns} turn${t.turns === 1 ? "" : "s"}: ${clip(t.title, 80)}${t.question ? ` — asks: ${clip(t.question.text, 100)}` : ""}`);
	}
	if (threads.length === 0) lines.push("- none");
	return lines.join("\n");
}

/** The `status` result text: phase, the last three turns, the pending question and the result. */
export function statusDetailText(d: ThreadDetail): string {
	const lines = [`${d.name} [${d.phase || "-"}] on ${d.agent}, model ${d.model || "agent default"}, ${plural(d.turns.length, "turn")}: ${clip(d.title, 100)}`];
	if (d.question) lines.push(questionText(d.question));
	for (const t of d.turns.slice(-STATUS_TURNS)) {
		lines.push(`Turn ${t.id} ${t.state}${t.title ? `: ${clip(t.title, 100)}` : ""}`);
		const body = t.state === "failed" ? t.error : t.reply;
		if (body) lines.push(`  ${truncateText(body, STATUS_REPLY_MAX).replace(/\n/g, "\n  ")}`);
	}
	const r = d.result;
	if (r) {
		lines.push(`Result: ${r.branch} @ ${r.head.slice(0, 8)} (${diffstat(r)}; ${r.pushed ? "pushed" : r.pushError ? `push failed: ${clip(r.pushError, 80)}` : "not pushed"})`);
	} else {
		lines.push("Result: none yet");
	}
	return lines.join("\n");
}

/** Runs one tool call. Failures throw; the host shows the message as a tool error. */
export async function runTool(cloud: Cloud, p: ToolParams, ctx: Host, toolCallId: string): Promise<ToolOutput> {
	switch (p.op) {
		case "delegate": {
			const task = need(p.task, "a task", "delegate");
			const d = await cloud.delegateFromTool(ctx, task, p.agent?.trim() || undefined, toolCallId);
			const warn = d.warnings.length > 0 ? ` Warnings: ${d.warnings.join("; ")}.` : "";
			return {
				text: `Started cloud thread ${d.name} on ${d.namespace}/${d.agent}; the result will be branch ${threadBranch(d.name)}.${warn} Results and questions arrive later as [cloud] messages; do not poll.`,
				details: { op: p.op, thread: d.name, namespace: d.namespace, agent: d.agent, warnings: d.warnings },
			};
		}
		case "list": {
			const o = await cloud.overview(ctx);
			return {
				text: overviewText(o.who.user, o.ns, o.agents, o.threads),
				details: { op: p.op, agents: o.agents.length, threads: o.threads.length },
			};
		}
		case "status": {
			const d = await cloud.threadDetail(ctx, need(p.thread, "a thread", "status"));
			return { text: statusDetailText(d), details: { op: p.op, thread: d.name, phase: d.phase } };
		}
		case "send": {
			const r = await cloud.sendMessage(ctx, need(p.thread, "a thread", "send"), need(p.text, "text", "send"));
			return { text: `Queued turn ${r.turn} on ${r.thread}. The reply arrives later as a [cloud] message.`, details: { op: p.op, ...r } };
		}
		case "answer": {
			const body: AnswerBody = {};
			if (p.selected && p.selected.length > 0) body.selected = p.selected;
			if (p.answer?.trim()) body.answer = p.answer.trim();
			if (!body.selected && !body.answer) throw new UserError("answer needs selected option labels or an answer text");
			const r = await cloud.answerQuestion(ctx, need(p.thread, "a thread", "answer"), body);
			return {
				text: `Answered ${r.summary} on ${r.thread}.${r.left > 0 ? ` ${plural(r.left, "more question")} wait.` : ""}`,
				details: { op: p.op, thread: r.thread, left: r.left },
			};
		}
		case "stop": {
			const r = await cloud.stopThread(ctx, need(p.thread, "a thread", "stop"));
			return {
				text: `Stopped ${r.thread}: running turn ${r.stoppedRunning ? "aborted" : "none"}, ${plural(r.cancelled, "queued turn")} cancelled.`,
				details: { op: p.op, ...r },
			};
		}
		case "pull": {
			const r = await cloud.pullThread(ctx, need(p.thread, "a thread", "pull"));
			return {
				text: `Fetched ${r.branch} at ${r.head.slice(0, 8)} (+${r.insertions} −${r.deletions}, ${r.filesChanged} files). Inspect it with git log ${r.branch}; the working tree was not touched.`,
				details: { op: p.op, ...r },
			};
		}
	}
}

/** Registers the tool. */
export function registerCloudTool(pi: ExtensionAPI, cloud: Cloud): void {
	const z = pi.zod;
	let existing: string[] = [];
	try {
		existing = pi.getAllTools().map(t => t.name);
	} catch {
		// the tool registry is not populated while extensions load; the reserved names do not include `cloud`
	}
	pi.registerTool({
		name: chooseToolName(existing),
		label: "Cloud",
		description: TOOL_DESCRIPTION,
		loadMode: "essential",
		approval: toolApproval,
		parameters: z.object({
			op: z.enum(TOOL_OPS).describe("What to do"),
			task: z.string().optional().describe("delegate: the self-contained task for the cloud agent"),
			agent: z.string().optional().describe("delegate: agent name or namespace/name; defaults to the configured one"),
			thread: z.string().optional().describe("status, send, answer, stop, pull: thread name or its last 6 characters"),
			text: z.string().optional().describe("send: the follow-up message"),
			selected: z.array(z.string()).optional().describe("answer: option labels to select"),
			answer: z.string().optional().describe("answer: free-text answer"),
		}),
		async execute(toolCallId, params, _signal, _onUpdate, ctx) {
			const out = await runTool(cloud, parseToolParams(params), ctx, toolCallId);
			return { content: [{ type: "text", text: out.text }], details: out.details };
		},
	});
}
