import { describe, expect, test } from "bun:test";
import { DONE_WIDGET_MS, doneLines, listLines, liveLines, showLines, Watcher, WIDGET_KEY, type WatchApi } from "../src/watch";
import type { Host, NotifyType, ThreadDetail, ThreadSummary, ThreadTurn } from "../src/types";

function turn(id: number, state: ThreadTurn["state"], extra: Partial<ThreadTurn> = {}): ThreadTurn {
	return { id, state, text: `t${id}`, by: "token:dev", queuedAt: "2026-10-18T10:00:00Z", ...extra };
}

function detail(phase: "Working" | "Idle", turns: ThreadTurn[]): ThreadDetail {
	return {
		namespace: "omp-dev",
		name: "coder-k3x9fa",
		agent: "coder",
		title: "Add Greet",
		owner: "token:dev",
		phase,
		queued: 0,
		model: "anthropic/m",
		createdAt: "2026-10-18T10:00:00Z",
		url: "",
		repo: { name: "app" },
		turns,
	};
}

interface FakeHost {
	host: Host;
	notes: [string, NotifyType | undefined][];
	widgets: (string[] | undefined)[];
	status: (string | undefined)[];
	tick: () => Promise<void>;
	intervals: number;
	cleared: number;
	timeouts: (() => void)[];
}

function fakeHost(): FakeHost {
	const f: FakeHost = { notes: [], widgets: [], status: [], intervals: 0, cleared: 0, timeouts: [], tick: async () => {}, host: undefined as unknown as Host };
	let cb: (() => void) | undefined;
	f.host = {
		cwd: "/x",
		model: undefined,
		sessionManager: { getSessionFile: () => undefined, getSessionDir: () => "/s" },
		isIdle: () => true,
		ui: {
			notify: (m, t) => void f.notes.push([m, t]),
			setWidget: (k, c) => {
				expect(k).toBe(WIDGET_KEY);
				f.widgets.push(c);
			},
			setStatus: (_k, t) => void f.status.push(t),
			confirm: async () => true,
		},
		setInterval: callback => {
			f.intervals++;
			cb = callback;
			return { id: "interval" } as unknown as Timer;
		},
		setTimeout: callback => {
			f.timeouts.push(callback);
			return { id: "timeout" } as unknown as Timer;
		},
		clearTimer: () => void f.cleared++,
	};
	f.tick = async () => {
		cb?.();
		// The fakes resolve in microtasks; one macrotask turn lets the whole poll finish.
		const { promise, resolve } = Promise.withResolvers<void>();
		setImmediate(resolve);
		await promise;
	};
	return f;
}

describe("Watcher", () => {
	test("polls while working, reports each finished turn once, then stops polling", async () => {
		const f = fakeHost();
		const states = [
			detail("Working", [turn(1, "done", { title: "old" }), turn(2, "running")]),
			detail("Working", [turn(1, "done", { title: "old" }), turn(2, "running")]),
			detail("Idle", [turn(1, "done", { title: "old" }), turn(2, "done", { title: "Added Greet", reply: "Done.\nSee tests." })]),
		];
		let i = 0;
		const sinces: (string | undefined)[] = [];
		const api: WatchApi = {
			thread: async () => states[Math.min(i, states.length - 1)] as ThreadDetail,
			events: async (_ns, _name, since) => {
				sinces.push(since);
				return { session: "s", events: i === 1 ? [{ at: "2026-10-18T10:00:05Z", kind: "tool", text: "read a.go" }] : [] };
			},
		};
		const w = new Watcher(f.host, api, "omp-dev");
		await w.watch("coder-k3x9fa");
		expect(f.intervals).toBe(1);
		expect(f.status.at(-1)).toBe("☁ 1 working");
		// Turn 1 was already finished when the watch began: not reported.
		await f.tick();
		expect(f.notes).toEqual([]);

		i = 1;
		await f.tick();
		expect(f.widgets.at(-1)?.[1]).toBe("tool: read a.go");

		i = 2;
		await f.tick();
		expect(f.notes).toEqual([["☁ coder-k3x9fa turn 2 done: Added Greet", "info"]]);
		expect(f.widgets.at(-1)).toEqual(["☁ coder-k3x9fa · turn 2 done", "Done.", "See tests.", "/cloud pull k3x9fa"]);
		expect(f.status.at(-1)).toBeUndefined();
		expect(w.watching).toEqual([]);
		// The since cursor advanced past the event we already have.
		expect(sinces).toContain("2026-10-18T10:00:05Z");

		// Done widget clears 10 minutes later; no further polling happens.
		expect(f.timeouts).toHaveLength(1);
		f.timeouts[0]?.();
		expect(f.widgets.at(-1)).toBeUndefined();
		await f.tick();
		expect(f.notes).toHaveLength(1);
		expect(DONE_WIDGET_MS).toBe(600_000);
	});

	test("a failed turn is an error notification with its error text", async () => {
		const f = fakeHost();
		let d = detail("Working", [turn(1, "running")]);
		const api: WatchApi = { thread: async () => d, events: async () => ({ session: "", events: [] }) };
		const w = new Watcher(f.host, api, "omp-dev");
		await w.watch("coder-k3x9fa");
		d = detail("Idle", [turn(1, "failed", { error: "stopped by token:dev" })]);
		await f.tick();
		expect(f.notes).toEqual([["☁ coder-k3x9fa turn 1 failed: stopped by token:dev", "error"]]);
	});

	test("an idle thread is shown once and not polled", async () => {
		const f = fakeHost();
		const api: WatchApi = {
			thread: async () => detail("Idle", [turn(1, "done", { reply: "all good" })]),
			events: async () => ({ session: "", events: [] }),
		};
		await new Watcher(f.host, api, "omp-dev").watch("coder-k3x9fa");
		expect(f.intervals).toBe(0);
		expect(f.widgets.at(-1)?.join("\n")).toContain("all good");
	});

	test("repeated poll failures stop the watcher with one error", async () => {
		const f = fakeHost();
		let fail = false;
		const api: WatchApi = {
			thread: async () => {
				if (fail) throw new Error("connection refused");
				return detail("Working", [turn(1, "running")]);
			},
			events: async () => ({ session: "", events: [] }),
		};
		const w = new Watcher(f.host, api, "omp-dev");
		await w.watch("coder-k3x9fa");
		fail = true;
		for (let n = 0; n < 5; n++) await f.tick();
		expect(f.notes).toEqual([["☁ lost contact with the cloud API: connection refused", "error"]]);
		expect(w.watching).toEqual([]);
	});
});

describe("rendering", () => {
	test("list lines: short id, phase, turns, latest state, title", () => {
		const t = { name: "coder-k3x9fa", phase: "Idle", turns: 3, latest: { id: 3, state: "done" }, title: "Add Greet" } as unknown as ThreadSummary;
		expect(listLines([t])).toEqual(["☁ cloud threads", "k3x9fa Idle t3 done Add Greet"]);
		expect(listLines([])).toEqual(["☁ no cloud threads"]);
	});

	test("show lines: header, latest reply capped at 12 lines, result, last 8 events", () => {
		const reply = Array.from({ length: 20 }, (_, n) => `line ${n}`).join("\n");
		const d = detail("Idle", [turn(1, "done", { reply, title: "T" })]);
		d.result = { branch: "omp-cloud/coder-k3x9fa", head: "0123456789abcdef", filesChanged: 2, insertions: 5, deletions: 1, pushed: true };
		const events = Array.from({ length: 12 }, (_, n) => ({ at: `a${n}`, kind: "k", text: `e${n}` }));
		const lines = showLines(d, events);
		expect(lines.filter(l => l.startsWith("    line "))).toHaveLength(12);
		expect(lines).toContain("  result: omp-cloud/coder-k3x9fa @ 01234567 (+5 −1, 2 files, pushed)");
		expect(lines.filter(l => /^ {2}k: e/.test(l))).toEqual(["  k: e4", "  k: e5", "  k: e6", "  k: e7", "  k: e8", "  k: e9", "  k: e10", "  k: e11"]);
	});

	test("live widget: header plus the last 10 events, texts clipped to 120", () => {
		const events = Array.from({ length: 15 }, (_, n) => ({ at: `a${n}`, kind: "tool", text: `e${n} ${"x".repeat(200)}` }));
		const lines = liveLines({ name: "n", phase: "Working", turns: [turn(1, "done"), turn(2, "running")] }, 2, events);
		expect(lines).toHaveLength(11);
		expect(lines[0]).toBe("☁ n · Working · turn 2");
		expect(lines[1]?.length).toBeLessThanOrEqual("tool: ".length + 120);
	});

	test("done lines for a failure show the error and the pull hint", () => {
		expect(doneLines({ name: "coder-k3x9fa" }, turn(4, "failed", { error: "boom" }))).toEqual([
			"☁ coder-k3x9fa · turn 4 failed",
			"boom",
			"/cloud pull k3x9fa",
		]);
	});
});
