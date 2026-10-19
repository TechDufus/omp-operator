import { describe, expect, test } from "bun:test";
import type { Theme } from "@oh-my-pi/pi-tui";
import { loadTui } from "../src/cards";
import { HubComponent, type HubApi, type HubResult } from "../src/hub";
import type { ThreadDetail, ThreadSummary, Whoami } from "../src/types";

const NOW = Date.parse("2026-10-19T12:00:00Z");

const summary = (name: string, over: Partial<ThreadSummary> = {}): ThreadSummary => ({
	namespace: "omp-dev",
	name,
	agent: "coder",
	title: `title of ${name}`,
	owner: "token:dev",
	phase: "Idle",
	turns: 2,
	queued: 0,
	model: "",
	latest: { id: 2, state: "done" },
	result: { branch: `omp-cloud/${name}`, head: "0123456789", filesChanged: 3, insertions: 12, deletions: 3, pushed: true },
	createdAt: "2026-10-19T10:00:00Z",
	lastActivityAt: "2026-10-19T11:56:00Z",
	url: "",
	...over,
});

const who: Whoami = {
	user: "token:dev",
	agents: [
		{
			namespace: "omp-dev",
			name: "coder",
			ready: true,
			defaultModel: "anthropic/claude-sonnet-5-5",
			modelPatterns: [],
			gitHosts: [],
			maxUploadBytes: 0,
			capacity: { maxConcurrentSessions: 4, running: 1 },
			threads: { working: 1, waiting: 1, idle: 3 },
		},
		{ namespace: "other", name: "elsewhere", ready: true, defaultModel: "", modelPatterns: [], gitHosts: [], maxUploadBytes: 0 },
	],
};

const theme = { fg: (_c: string, t: string) => t, bold: (t: string) => t } as unknown as Theme;

async function make(threads: ThreadSummary[], api: Partial<HubApi> = {}) {
	const tui = await loadTui();
	if (!tui) throw new Error("the host pi-tui is required for the hub tests");
	const results: (HubResult | undefined)[] = [];
	let renders = 0;
	const calls: string[] = [];
	const full: HubApi = {
		whoami: async () => who,
		listThreads: async () => threads,
		thread: async (_ns, name) => {
			calls.push(`thread ${name}`);
			const t = threads.find(x => x.name === name) as ThreadSummary;
			return {
				...t,
				repo: { name: "app" },
				turns: [
					{ id: 1, state: "done", text: "", by: "", queuedAt: "", reply: "first reply" },
					{ id: 2, state: "done", text: "", by: "", queuedAt: "", title: "Added Greet", reply: "line one\nline two" },
				],
			} as ThreadDetail;
		},
		events: async (_ns, name) => ({ session: "s", events: [{ at: "t", kind: "tool", text: `event of ${name}` }] }),
		...api,
	};
	const hub = new HubComponent({
		api: full,
		ns: "omp-dev",
		theme,
		tui,
		requestRender: () => void renders++,
		done: r => void results.push(r),
		now: () => NOW,
	});
	await hub.refresh();
	return { hub, results, calls, renders: () => renders };
}

describe("HubComponent", () => {
	const threads = [
		summary("coder-k3x9fa", { phase: "Waiting", question: { turn: 2, id: 1, ask: 1, text: "Which database?", header: "DB", options: [{ label: "postgres", description: "prod" }, { label: "sqlite" }], recommended: 0, askedAt: "", remaining: 2 } }),
		summary("coder-ab12cd", { phase: "Working" }),
		summary("coder-zz99yy"),
	];

	test("lists the namespace's agents with capacity and the threads with phase, id, turns, state, diff, age and title", async () => {
		const { hub } = await make(threads);
		const rows = hub.render(120);
		const text = rows.join("\n");
		expect(text).toContain("☁ omp-cloud · token:dev · omp-dev");
		expect(text).toContain("coder  ready · running 1/4 · 1 working · 1 waiting · 3 idle");
		expect(text).not.toContain("elsewhere");
		expect(text).toContain("Threads (3)");
		const row = rows.find(r => r.includes("k3x9fa") && r.includes("Waiting"));
		expect(row).toMatch(/▸ \? k3x9fa Waiting\s+t2\s+done\s+\+12\/−3\s+4m\s+title of coder-k3x9fa/);
		expect(rows.find(r => r.includes("ab12cd"))).toMatch(/ {2}● ab12cd Working/);
	});

	test("shows the selected thread's pending question with options, its latest reply and live events", async () => {
		const { hub } = await make(threads);
		const text = hub.render(120).join("\n");
		expect(text).toContain("? DB: Which database? (+1 more)");
		expect(text).toContain("(•) postgres  ← recommended — prod");
		expect(text).toContain("(•) sqlite");
		expect(text).toContain("Latest — turn 2 done: Added Greet");
		expect(text).toContain("  line one");
		expect(text).toContain("tool: event of coder-k3x9fa");
		expect(text).toContain("s send · a answer · x stop · p pull · r resume · d delete · q close");
	});

	test("moving the selection loads the new thread's detail", async () => {
		const { hub, calls } = await make(threads);
		hub.handleInput("\x1b[B"); // down
		await hub.refresh();
		expect(hub.selected?.name).toBe("coder-ab12cd");
		expect(calls).toContain("thread coder-ab12cd");
		expect(hub.render(120).join("\n")).toContain("event of coder-ab12cd");
		hub.handleInput("k");
		expect(hub.selected?.name).toBe("coder-k3x9fa");
		hub.handleInput("k");
		expect(hub.selected?.name).toBe("coder-k3x9fa");
	});

	test("each action key closes the hub with the action and the selected thread", async () => {
		const keys: [string, string][] = [
			["s", "send"],
			["a", "answer"],
			["x", "stop"],
			["p", "pull"],
			["r", "resume"],
			["d", "delete"],
		];
		for (const [key, action] of keys) {
			const { hub, results } = await make(threads);
			hub.handleInput("j");
			hub.handleInput(key);
			expect(results).toEqual([{ action: action as HubResult["action"], thread: "coder-ab12cd" }]);
		}
	});

	test("q, escape and ctrl+c close without an action", async () => {
		for (const key of ["q", "\x1b", "\x03"]) {
			const { hub, results } = await make(threads);
			hub.handleInput(key);
			expect(results).toEqual([undefined]);
		}
	});

	test("with no threads the action keys do nothing and the hub says how to start", async () => {
		const { hub, results } = await make([]);
		hub.handleInput("s");
		hub.handleInput("d");
		expect(results).toEqual([]);
		expect(hub.render(100).join("\n")).toContain("no threads — hand off work with /cloud <task>");
	});

	test("an API failure is shown in the hub rather than thrown", async () => {
		const { hub } = await make(threads, {
			listThreads: async () => {
				throw new Error("connection refused");
			},
		});
		expect(hub.render(100).join("\n")).toContain("! connection refused");
	});

	test("every row fits the width, however long the title", async () => {
		const { hub } = await make([summary("coder-k3x9fa", { title: "x".repeat(300) })]);
		const tui = await loadTui();
		for (const w of [40, 80]) {
			for (const row of hub.render(w)) expect(tui?.truncateToWidth(row, w)).toBe(row);
		}
	});

	test("polling refreshes the list; after dispose it stops", async () => {
		let n = 0;
		const { hub } = await make(threads, {
			listThreads: async () => {
				n++;
				return threads;
			},
		});
		const before = n;
		await hub.refresh();
		expect(n).toBe(before + 1);
		hub.dispose();
		await hub.refresh();
		expect(n).toBe(before + 1);
	});
});
