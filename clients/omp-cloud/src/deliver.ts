/**
 * Delivery of cards into the local conversation.
 *
 * - Threads the user started with `/cloud` only display their cards: an idle session shows them
 *   at once without a model turn; a busy one holds them until the agent ends.
 * - Threads the local model started with the `cloud` tool (`origin: "tool"`) wake the model with
 *   their turn and question cards: `triggerTurn` when idle, an aside (next step boundary, no
 *   interruption of the running tool batch) when busy.
 */

import { CARD_TYPE, asCard, cardContent, cardEvents, DETAILS_REPLY_MAX, questionCard, threadBranch, type CardDetails, type LaunchCard, type TurnCard } from "./cards";
import type { HandoffSummary } from "./launch";
import { spanBetween, turnCost } from "./format";
import type { WatchSink } from "./watch";
import type { Host, PendingQuestion, PiHost, ThreadDetail, ThreadEvent, ThreadOrigin, ThreadTurn } from "./types";

interface Message {
	customType: string;
	content: string;
	display: boolean;
	details: CardDetails;
}

/** Whether a card wakes the model (a tool-started thread's results and questions). */
function wakesModel(d: CardDetails): boolean {
	return d.origin === "tool" && (d.kind === "turn" || d.kind === "question");
}

/** Thread origins recorded in the launch cards of a session's entries. */
export function originsFromEntries(entries: readonly unknown[]): Map<string, ThreadOrigin> {
	const out = new Map<string, ThreadOrigin>();
	for (const e of entries) {
		if (e === null || typeof e !== "object") continue;
		const entry = e as { type?: unknown; customType?: unknown; details?: unknown };
		if (entry.type !== "custom_message" || entry.customType !== CARD_TYPE) continue;
		const card = asCard(entry.details);
		if (card?.kind === "launch") out.set(card.thread, card.origin);
	}
	return out;
}

export function launchCard(args: {
	origin: ThreadOrigin;
	thread: string;
	namespace: string;
	agent: string;
	prompt: string;
	summary: HandoffSummary;
	warnings: string[];
}): LaunchCard {
	const s = args.summary;
	return {
		kind: "launch",
		origin: args.origin,
		thread: args.thread,
		namespace: args.namespace,
		agent: args.agent,
		model: s.model,
		thinking: s.thinking,
		prompt: args.prompt,
		repo: { ...s.repo, branch: threadBranch(args.thread) },
		commits: s.commits,
		wip: s.wip,
		untracked: s.untracked,
		untrackedTotal: s.untrackedTotal,
		withheld: s.withheld,
		secrets: s.secrets,
		conversation: s.conversation,
		setupBytes: s.setupBytes,
		warnings: args.warnings,
	};
}

/** Posts cards to the session and feeds the watcher's turn and question events into them. */
export class Deliverer implements WatchSink {
	readonly #pi: PiHost;
	readonly #origins = new Map<string, ThreadOrigin>();
	#pending: Message[] = [];

	constructor(pi: PiHost) {
		this.#pi = pi;
	}

	setOrigin(thread: string, origin: ThreadOrigin): void {
		this.#origins.set(thread, origin);
	}

	/** The origin of a thread: remembered, else recovered from the session's launch cards, else the user. */
	originOf(host: Host, thread: string): ThreadOrigin {
		const known = this.#origins.get(thread);
		if (known) return known;
		const entries = host.sessionManager.getEntries?.() ?? [];
		for (const [name, origin] of originsFromEntries(entries)) this.#origins.set(name, origin);
		return this.#origins.get(thread) ?? "user";
	}

	/** Sends the card; see the module comment for who is woken. */
	post(host: Host, d: CardDetails): void {
		const msg: Message = { customType: CARD_TYPE, content: cardContent(d), display: true, details: d };
		if (host.isIdle()) {
			this.#pi.sendMessage(msg, { triggerTurn: wakesModel(d) });
		} else if (wakesModel(d)) {
			this.#pi.sendMessage(msg, { deliverAs: "aside" });
		} else {
			this.#pending.push(msg);
		}
	}

	/** Shows cards held back while the agent was busy; a no-op until it is idle. */
	flush(host: Host): void {
		if (this.#pending.length === 0 || !host.isIdle()) return;
		const held = this.#pending;
		this.#pending = [];
		for (const msg of held) this.#pi.sendMessage(msg, { triggerTurn: false });
	}

	/** Number of cards waiting for the agent to be idle. */
	get held(): number {
		return this.#pending.length;
	}

	turn(host: Host, thread: ThreadDetail, turn: ThreadTurn, events: ThreadEvent[]): void {
		if (turn.state !== "done" && turn.state !== "failed") return;
		const card: TurnCard = {
			kind: "turn",
			origin: this.originOf(host, thread.name),
			thread: thread.name,
			namespace: thread.namespace,
			turn: turn.id,
			state: turn.state,
			title: turn.title ?? "",
			duration: spanBetween(turn.startedAt ?? turn.queuedAt, turn.completedAt),
			model: thread.model,
			cost: turnCost(turn),
			head: turn.head ?? "",
			filesChanged: turn.filesChanged ?? 0,
			insertions: turn.insertions ?? 0,
			deletions: turn.deletions ?? 0,
			pushed: turn.pushed ?? false,
			pushError: turn.pushError ?? "",
			branch: threadBranch(thread.name),
			reply: (turn.reply ?? "").slice(0, DETAILS_REPLY_MAX),
			error: turn.error ?? "",
			events: cardEvents(events),
		};
		this.post(host, card);
	}

	question(host: Host, thread: ThreadDetail, q: PendingQuestion): void {
		this.post(host, questionCard(this.originOf(host, thread.name), thread.name, thread.namespace, q));
	}
}
