import { describe, expect, test } from "bun:test";
import type { CardDetails } from "../src/cards";
import { Deliverer, launchCard, originsFromEntries } from "../src/deliver";
import type { PendingQuestion, ThreadDetail, ThreadTurn } from "../src/types";
import { fakeHost, fakePi } from "./fakes";

const thread = (): ThreadDetail => ({
	namespace: "omp-dev",
	name: "coder-k3x9fa",
	agent: "coder",
	title: "Add Greet",
	owner: "token:dev",
	phase: "Idle",
	queued: 0,
	model: "anthropic/claude-sonnet-5-5",
	createdAt: "2026-10-19T10:00:00Z",
	url: "",
	repo: { name: "app" },
	turns: [],
});

const done = (over: Partial<ThreadTurn> = {}): ThreadTurn => ({
	id: 1,
	state: "done",
	text: "t",
	by: "token:dev",
	queuedAt: "2026-10-19T10:00:00Z",
	startedAt: "2026-10-19T10:00:10Z",
	completedAt: "2026-10-19T10:04:22Z",
	reply: "All done.",
	title: "Added Greet",
	head: "0123456789abcdef",
	filesChanged: 2,
	insertions: 31,
	deletions: 4,
	pushed: true,
	usage: { cost: "0.42" },
	...over,
});

const pending = (): PendingQuestion => ({ turn: 1, id: 1, ask: 1, text: "Which one?", askedAt: "", remaining: 1, options: [{ label: "a" }] });

function setup(origin: "user" | "tool", idle: boolean) {
	const pi = fakePi();
	const host = fakeHost();
	host.idle = idle;
	const d = new Deliverer(pi.pi);
	d.setOrigin("coder-k3x9fa", origin);
	return { pi, host, d };
}

describe("delivery by origin", () => {
	test("a thread the user started displays its cards without waking the model", () => {
		const { pi, host, d } = setup("user", true);
		d.turn(host.host, thread(), done(), []);
		d.question(host.host, thread(), pending());
		expect(pi.sent.map(s => s.options)).toEqual([{ triggerTurn: false }, { triggerTurn: false }]);
		expect(pi.sent.map(s => (s.message.details as CardDetails).kind)).toEqual(["turn", "question"]);
		expect(pi.sent.every(s => s.message.customType === "omp-cloud" && s.message.display)).toBe(true);
	});

	test("while the agent is busy the user's cards wait and appear once it is idle", () => {
		const { pi, host, d } = setup("user", false);
		d.turn(host.host, thread(), done(), []);
		expect(pi.sent).toHaveLength(0);
		expect(d.held).toBe(1);
		d.flush(host.host);
		expect(pi.sent).toHaveLength(0);
		host.idle = true;
		d.flush(host.host);
		expect(pi.sent.map(s => s.options)).toEqual([{ triggerTurn: false }]);
		expect(d.held).toBe(0);
		d.flush(host.host);
		expect(pi.sent).toHaveLength(1);
	});

	test("a thread the model started wakes it with each turn and question: a turn when idle", () => {
		const { pi, host, d } = setup("tool", true);
		d.turn(host.host, thread(), done(), []);
		d.question(host.host, thread(), pending());
		expect(pi.sent.map(s => s.options)).toEqual([{ triggerTurn: true }, { triggerTurn: true }]);
	});

	test("... and as an aside at the next step boundary when the agent is busy, never held", () => {
		const { pi, host, d } = setup("tool", false);
		d.turn(host.host, thread(), done(), []);
		d.question(host.host, thread(), pending());
		expect(pi.sent.map(s => s.options)).toEqual([{ deliverAs: "aside" }, { deliverAs: "aside" }]);
		expect(d.held).toBe(0);
	});

	test("the launch card of a model-started thread only displays: the tool result already answered the model", () => {
		for (const idle of [true, false]) {
			const { pi, host, d } = setup("tool", idle);
			d.post(host.host, launchCard({ origin: "tool", thread: "coder-k3x9fa", namespace: "omp-dev", agent: "coder", prompt: "p", summary: summary(), warnings: [] }));
			if (idle) expect(pi.sent.map(s => s.options)).toEqual([{ triggerTurn: false }]);
			else expect(pi.sent).toHaveLength(0);
		}
	});

	test("the card's content is the compact [cloud] text and its details are the card", () => {
		const { pi, host, d } = setup("user", true);
		d.turn(host.host, thread(), done({ reply: "x".repeat(4000) }), [{ at: "t", kind: "tool", text: "read a.go" }]);
		const sent = pi.sent[0]?.message;
		expect(sent?.content.startsWith("[cloud] coder-k3x9fa turn 1 done: Added Greet")).toBe(true);
		expect(sent?.content.length).toBeLessThan(1800);
		const card = sent?.details as CardDetails;
		expect(card.kind === "turn" && card.duration).toBe("4m12s");
		expect(card.kind === "turn" && card.cost).toBe("$0.42");
		expect(card.kind === "turn" && card.events).toEqual([{ kind: "tool", text: "read a.go" }]);
		expect(card.kind === "turn" && card.reply.length).toBe(4000);
	});

	test("turns that are not finished produce no card", () => {
		const { pi, host, d } = setup("user", true);
		d.turn(host.host, thread(), done({ state: "running" }), []);
		expect(pi.sent).toHaveLength(0);
	});
});

function summary() {
	return {
		repo: { name: "app", mode: "delta" as const, baseRef: "main", baseCommit: "abc", localBranch: "feat" },
		commits: 1,
		wip: { files: 1, insertions: 1, deletions: 0 },
		untracked: [],
		untrackedTotal: 0,
		withheld: [],
		secrets: { files: 0, env: 0 },
		conversation: "included" as const,
		setupBytes: 10,
		model: "m",
		thinking: "",
	};
}

describe("thread origin", () => {
	test("is recovered from the launch cards persisted in the session", () => {
		const entries = [
			{ type: "message", id: "e1" },
			{ type: "custom_message", customType: "omp-cloud", details: launchCard({ origin: "tool", thread: "coder-k3x9fa", namespace: "n", agent: "a", prompt: "p", summary: summary(), warnings: [] }) },
			{ type: "custom_message", customType: "omp-cloud", details: launchCard({ origin: "user", thread: "coder-zzzzzz", namespace: "n", agent: "a", prompt: "p", summary: summary(), warnings: [] }) },
			{ type: "custom_message", customType: "other", details: { kind: "launch", origin: "tool", thread: "coder-yyyyyy" } },
		];
		expect([...originsFromEntries(entries)]).toEqual([
			["coder-k3x9fa", "tool"],
			["coder-zzzzzz", "user"],
		]);

		const pi = fakePi();
		const host = fakeHost({ entries });
		const d = new Deliverer(pi.pi);
		d.turn(host.host, thread(), done(), []);
		expect(pi.sent[0]?.options).toEqual({ triggerTurn: true });
		expect(d.originOf(host.host, "unknown-thread")).toBe("user");
	});
});
