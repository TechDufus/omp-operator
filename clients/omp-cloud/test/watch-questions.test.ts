import { describe, expect, test } from "bun:test";
import { helpLines, liveLines, showLines, statusText, Watcher, type WatchApi, type WatchSink } from "../src/watch";
import type { PendingQuestion, ThreadDetail, ThreadEvent, ThreadPhase, ThreadTurn } from "../src/types";
import { fakeHost } from "./fakes";

const pq = (id: number, over: Partial<PendingQuestion> = {}): PendingQuestion => ({
	turn: 1,
	id,
	ask: 1,
	text: `question ${id}?`,
	options: [{ label: "yes" }, { label: "no" }],
	askedAt: "2026-10-19T10:00:00Z",
	remaining: 1,
	...over,
});

const turn = (id: number, state: ThreadTurn["state"], extra: Partial<ThreadTurn> = {}): ThreadTurn => ({
	id,
	state,
	text: `t${id}`,
	by: "token:dev",
	queuedAt: "2026-10-19T10:00:00Z",
	...extra,
});

const detail = (phase: ThreadPhase, turns: ThreadTurn[], question: PendingQuestion | null = null): ThreadDetail => ({
	namespace: "omp-dev",
	name: "coder-k3x9fa",
	agent: "coder",
	title: "Add Greet",
	owner: "token:dev",
	phase,
	queued: 0,
	model: "",
	question,
	createdAt: "2026-10-19T10:00:00Z",
	url: "",
	repo: { name: "app" },
	turns,
});

class Sink implements WatchSink {
	questions: PendingQuestion[] = [];
	turns: { id: number; events: ThreadEvent[] }[] = [];
	flushed = 0;
	turn(_h: unknown, _t: ThreadDetail, t: ThreadTurn, events: ThreadEvent[]): void {
		this.turns.push({ id: t.id, events });
	}
	question(_h: unknown, _t: ThreadDetail, q: PendingQuestion): void {
		this.questions.push(q);
	}
	flush(): void {
		this.flushed++;
	}
}

describe("questions in the watcher", () => {
	test("a waiting thread is polled; each new question is one card and one notification; the status counts waiting", async () => {
		const f = fakeHost();
		const sink = new Sink();
		let current = detail("Waiting", [turn(1, "running")], pq(1));
		const api: WatchApi = { thread: async () => current, events: async () => ({ session: "s", events: [] }) };
		const w = new Watcher(f.host, api, "omp-dev", sink);
		await w.watch("coder-k3x9fa");

		expect(f.intervalCallbacks).toHaveLength(1);
		expect(sink.questions.map(q => q.id)).toEqual([1]);
		expect(f.notes).toEqual([["☁ coder-k3x9fa asks: question 1? — /cloud answer k3x9fa", "warning"]]);
		expect(f.status.at(-1)).toBe("☁ 0 working · 1 waiting");
		expect(f.widgets.at(-1)?.[1]).toBe("? question 1? — /cloud answer k3x9fa");

		// Still waiting on the same question: nothing new.
		await f.tick();
		await f.tick();
		expect(sink.questions).toHaveLength(1);
		expect(f.notes).toHaveLength(1);

		// Answered: the thread works again, nothing is announced, the status drops "waiting".
		current = detail("Working", [turn(1, "running")]);
		await f.tick();
		expect(f.status.at(-1)).toBe("☁ 1 working");

		// A later question is a new card.
		current = detail("Waiting", [turn(1, "running")], pq(2));
		await f.tick();
		expect(sink.questions.map(q => q.id)).toEqual([1, 2]);
		expect(f.notes.at(-1)?.[0]).toContain("question 2?");
	});

	test("a question with the same id in another turn is a different question", async () => {
		const f = fakeHost();
		const sink = new Sink();
		let current = detail("Waiting", [turn(1, "running")], pq(1, { turn: 1 }));
		const w = new Watcher(f.host, { thread: async () => current, events: async () => ({ session: "", events: [] }) }, "omp-dev", sink);
		await w.watch("coder-k3x9fa");
		current = detail("Waiting", [turn(1, "done"), turn(2, "running")], pq(1, { turn: 2 }));
		await f.tick();
		expect(sink.questions.map(q => `${q.turn}:${q.id}`)).toEqual(["1:1", "2:1"]);
	});

	test("a finished turn goes to the sink once with the events collected while watching; held cards are flushed every poll", async () => {
		const f = fakeHost();
		const sink = new Sink();
		let current = detail("Working", [turn(1, "running")]);
		let evs: ThreadEvent[] = [{ at: "a1", kind: "tool", text: "read a.go" }];
		const api: WatchApi = { thread: async () => current, events: async () => ({ session: "s", events: evs }) };
		const w = new Watcher(f.host, api, "omp-dev", sink);
		await w.watch("coder-k3x9fa");
		evs = [{ at: "a2", kind: "assistant", text: "done" }];
		current = detail("Idle", [turn(1, "done", { title: "T", reply: "ok" })]);
		await f.tick();
		await f.tick();
		expect(sink.turns).toHaveLength(1);
		expect(sink.turns[0]?.events.map(e => e.at)).toEqual(["a1", "a2"]);
		expect(sink.flushed).toBeGreaterThanOrEqual(1);
	});

	test("watching an idle thread whose turns already finished posts no cards", async () => {
		const f = fakeHost();
		const sink = new Sink();
		const d = detail("Idle", [turn(1, "done", { reply: "old" })]);
		await new Watcher(f.host, { thread: async () => d, events: async () => ({ session: "", events: [] }) }, "omp-dev", sink).watch("coder-k3x9fa");
		expect(sink.turns).toEqual([]);
		expect(sink.questions).toEqual([]);
		expect(f.status.at(-1)).toBeUndefined();
	});
});

describe("status and rendering of waiting threads", () => {
	test("status text", () => {
		expect(statusText(0, 0)).toBeUndefined();
		expect(statusText(2, 0)).toBe("☁ 2 working");
		expect(statusText(2, 1)).toBe("☁ 2 working · 1 waiting");
		expect(statusText(0, 1)).toBe("☁ 0 working · 1 waiting");
	});

	test("live and show lines carry the pending question", () => {
		const d = detail("Waiting", [turn(1, "running")], pq(1, { remaining: 3 }));
		expect(liveLines(d, 1, [])[1]).toBe("? question 1? — /cloud answer k3x9fa");
		expect(showLines(d, [])).toContain("  ? question 1? (+2 more)");
		expect(liveLines(detail("Working", [turn(1, "running")]), 1, [])).toHaveLength(1);
	});

	test("help names the hub and the answer verb", () => {
		const text = helpLines().join("\n");
		expect(text).toContain("no arguments opens the hub");
		expect(text).toContain("/cloud answer <id> [text]");
	});
});
