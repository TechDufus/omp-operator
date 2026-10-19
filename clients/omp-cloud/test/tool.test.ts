import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, test } from "bun:test";
import { readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { z } from "@oh-my-pi/omptype/zod";
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { loadTui, type CardDetails } from "../src/cards";
import { git } from "../src/snapshot";
import {
	chooseToolName,
	overviewText,
	parseToolParams,
	registerCloudTool,
	runTool,
	statusDetailText,
	toolApproval,
	TOOL_DESCRIPTION,
	type ToolParams,
} from "../src/tool";
import type { AnswerBody, Question, ThreadDetail, ThreadSummary, ThreadTurn, Whoami } from "../src/types";
import { Cloud } from "../src/verbs";
import { fakeHost, fakePi, until, type FakeHost } from "./fakes";
import { cleanup, commitFile, configureRepo, remoteFixture, tmp, type Fixture } from "./helpers";

afterEach(cleanup);

const NAME = "coder-k3x9fa";
const NS = "omp-dev";
const BASE = `/api/v1/namespaces/${NS}/threads`;

interface Req {
	method: string;
	path: string;
	json?: unknown;
	parts?: Record<string, Blob | string>;
}

const state = {
	reqs: [] as Req[],
	phase: "Idle" as ThreadDetail["phase"],
	turns: [] as ThreadTurn[],
	question: null as ThreadDetail["question"],
	result: null as ThreadDetail["result"],
	bundle: undefined as string | undefined,
	listed: [] as ThreadSummary[],
};

const who: Whoami = {
	user: "token:dev",
	agents: [
		{
			namespace: NS,
			name: "coder",
			ready: true,
			defaultModel: "anthropic/claude-sonnet-5-5",
			modelPatterns: ["anthropic/*"],
			gitHosts: [],
			maxUploadBytes: 1 << 30,
			capacity: { maxConcurrentSessions: 4, running: 1 },
			threads: { working: 1, waiting: 1, idle: 3 },
		},
	],
};

const detail = (): ThreadDetail => ({
	namespace: NS,
	name: NAME,
	agent: "coder",
	title: "Add Greet",
	owner: "token:dev",
	phase: state.phase,
	queued: 0,
	model: "anthropic/claude-sonnet-5-5",
	question: state.question,
	result: state.result,
	createdAt: "2026-10-19T10:00:00Z",
	url: "",
	repo: { name: "app", baseCommit: "" },
	turns: state.turns,
});

let server: ReturnType<typeof Bun.serve>;

beforeAll(() => {
	server = Bun.serve({
		port: 0,
		async fetch(req) {
			const url = new URL(req.url);
			const r: Req = { method: req.method, path: url.pathname };
			state.reqs.push(r);
			const p = url.pathname;
			if (p === "/api/v1/whoami") return Response.json(who);
			if (req.method === "POST" && p === BASE) {
				r.parts = {};
				for (const [k, v] of (await req.formData()).entries()) r.parts[k] = v;
				return Response.json({ name: NAME, namespace: NS, warnings: ["model x is not available on coder; using y"] }, { status: 201 });
			}
			if (req.method === "GET" && p === BASE) return Response.json({ threads: state.listed });
			if (req.method === "GET" && p === `${BASE}/${NAME}`) return Response.json(detail());
			if (p === `${BASE}/${NAME}/events`) return Response.json({ session: "s", events: [] });
			if (req.method === "POST" && p === `${BASE}/${NAME}/messages`) {
				r.json = await req.json();
				return Response.json({ turn: { id: 3 } }, { status: 201 });
			}
			if (req.method === "POST" && p === `${BASE}/${NAME}/stop`) return Response.json({ stoppedRunning: true, cancelled: 2 });
			if (req.method === "POST" && p.includes("/questions/") && p.endsWith("/answer")) {
				r.json = await req.json();
				const body = r.json as AnswerBody;
				return Response.json({ id: 2, ask: 1, key: "k", text: "Which database?", askedAt: "", selected: body.selected, answer: body.answer, answeredAt: "now" } satisfies Question);
			}
			if (p === `${BASE}/${NAME}/result`) {
				if (!state.bundle) return Response.json({ error: "no result yet" }, { status: 404 });
				return new Response(await readFile(state.bundle), { headers: { "X-Omp-Branch": `omp-cloud/${NAME}`, "X-Omp-Head": "x", "X-Omp-Base": "y" } });
			}
			return Response.json({ error: "not found" }, { status: 404 });
		},
	});
});

afterAll(() => server.stop(true));

beforeEach(() => {
	state.reqs = [];
	state.phase = "Idle";
	state.turns = [];
	state.question = null;
	state.result = null;
	state.bundle = undefined;
	state.listed = [summary()];
});

function summary(over: Partial<ThreadSummary> = {}): ThreadSummary {
	return {
		namespace: NS,
		name: NAME,
		agent: "coder",
		title: "Add Greet",
		owner: "token:dev",
		phase: "Waiting",
		turns: 1,
		queued: 0,
		model: "",
		latest: null,
		question: { turn: 1, id: 2, ask: 1, text: "Which database?", askedAt: "", remaining: 1 },
		createdAt: "2026-10-19T10:00:00Z",
		url: "",
		...over,
	};
}

async function env(): Promise<Record<string, string>> {
	return {
		OMP_CLOUD_URL: `http://127.0.0.1:${server.port}`,
		OMP_CLOUD_TOKEN: "tok",
		OMP_CLOUD_NAMESPACE: NS,
		OMP_CLOUD_AGENT: "coder",
		PI_CODING_AGENT_DIR: await tmp("omp-cloud-agent-"),
	};
}

const turn = (id: number, over: Partial<ThreadTurn> = {}): ThreadTurn => ({ id, state: "done", text: `t${id}`, by: "token:dev", queuedAt: "", title: `Title ${id}`, reply: `reply ${id}`, ...over });

async function setup(opts: { sessionFile?: string; mode?: string; fixture?: Fixture; hostOpts?: Parameters<typeof fakeHost>[0] } = {}) {
	const f = opts.fixture ?? (await remoteFixture());
	const pi = fakePi();
	const cloud = new Cloud(pi.pi, "/own", await env());
	const host = fakeHost({ cwd: f.work, sessionFile: opts.sessionFile, mode: opts.mode, ...opts.hostOpts });
	return { f, pi, cloud, host };
}

const call = (cloud: Cloud, host: FakeHost, p: ToolParams, id = "call_1") => runTool(cloud, p, host.host, id);

describe("tool operations against the API", () => {
	test("list: agents with capacity and the threads, a waiting one names its question", async () => {
		const { cloud, host } = await setup();
		const out = await call(cloud, host, { op: "list" });
		expect(out.text).toContain(`- ${NS}/coder: ready; running 1 of 4 sessions; 1 working, 1 waiting, 3 idle threads; default model anthropic/claude-sonnet-5-5`);
		expect(out.text).toContain(`- ${NAME} [Waiting] 1 turn: Add Greet — asks: Which database?`);
	});

	test("status resolves the 6-character id and reports phase, the last three turns, the question and the result", async () => {
		state.phase = "Waiting";
		state.turns = [turn(1), turn(2), turn(3), turn(4, { reply: "r".repeat(2000) }), turn(5, { state: "failed", error: "boom", reply: "" })];
		state.question = { turn: 5, id: 2, ask: 1, text: "Which database?", header: "DB", options: [{ label: "postgres", description: "prod" }, { label: "sqlite" }], recommended: 0, askedAt: "", remaining: 2 };
		state.result = { branch: `omp-cloud/${NAME}`, head: "0123456789abcdef", filesChanged: 2, insertions: 5, deletions: 1, pushed: false, pushError: "denied" };
		const { cloud, host } = await setup();
		const text = (await call(cloud, host, { op: "status", thread: "k3x9fa" })).text;
		expect(text).toContain(`${NAME} [Waiting] on coder`);
		expect(text).toContain("Waiting for an answer (turn 5, 2 questions): DB: Which database?");
		expect(text).toContain("1. postgres — prod (recommended); 2. sqlite");
		expect(text).not.toContain("Turn 1 ");
		expect(text).not.toContain("Turn 2 ");
		expect(text).toContain("Turn 3 done: Title 3");
		expect(text).toContain("Turn 5 failed");
		expect(text).toContain("  boom");
		expect(text).toContain(`${"r".repeat(1500)}… [500 more characters]`);
		expect(text).toContain(`Result: omp-cloud/${NAME} @ 01234567 (+5 −1 · 2 files; push failed: denied)`);
	});

	test("send, stop and answer reach the API with the resolved thread", async () => {
		state.turns = [turn(1, { state: "running", questions: [{ id: 1, ask: 1, key: "a", text: "done", askedAt: "", answeredAt: "x" }, { id: 2, ask: 1, key: "b", text: "Which database?", askedAt: "" }] })];
		const { cloud, host } = await setup();
		expect((await call(cloud, host, { op: "send", thread: "k3x9fa", text: "also add Farewell" })).text).toContain("Queued turn 3");
		expect((await call(cloud, host, { op: "stop", thread: NAME })).text).toBe(`Stopped ${NAME}: running turn aborted, 2 queued turns cancelled.`);
		const ans = await call(cloud, host, { op: "answer", thread: "k3x9fa", selected: ["postgres"], answer: " and make it fast " });
		expect(ans.text).toBe(`Answered "Which database?" → postgres, and make it fast on ${NAME}.`);
		const posts = state.reqs.filter(r => r.method === "POST");
		expect(posts.map(r => r.path)).toEqual([`${BASE}/${NAME}/messages`, `${BASE}/${NAME}/stop`, `${BASE}/${NAME}/turns/1/questions/2/answer`]);
		expect(posts[0]?.json).toEqual({ text: "also add Farewell" });
		expect(posts[2]?.json).toEqual({ selected: ["postgres"], answer: "and make it fast" });
	});

	test("input errors: missing task, thread, text, answer, no question waiting, unknown thread", async () => {
		const { cloud, host } = await setup();
		const fails = (p: ToolParams) => call(cloud, host, p).then(() => "", (e: Error) => e.message);
		expect(await fails({ op: "delegate" })).toBe("delegate needs a task");
		expect(await fails({ op: "status" })).toBe("status needs a thread");
		expect(await fails({ op: "send", thread: NAME })).toBe("send needs text");
		expect(await fails({ op: "answer", thread: NAME })).toBe("answer needs selected option labels or an answer text");
		expect(await fails({ op: "answer", thread: NAME, answer: "x" })).toBe(`${NAME} has no question waiting`);
		expect(await fails({ op: "pull", thread: "nosuch" })).toBe('no thread matches "nosuch"');
		state.listed = [summary(), summary({ name: "coder-k3x9fb" })];
		expect(await fails({ op: "stop", thread: "coder" })).toContain("matches several threads");
	});

	test("pull fetches the result branch into the local repository and reports it", async () => {
		const f = await remoteFixture();
		const other = join(f.root, "other");
		await git(f.root, ["clone", "-q", f.bare, other]);
		await configureRepo(other);
		await git(other, ["checkout", "-q", "-b", `omp-cloud/${NAME}`]);
		const head = await commitFile(other, "greet.txt", "hi\n");
		state.bundle = join(f.root, "result.bundle");
		await git(other, ["bundle", "create", state.bundle, `refs/heads/omp-cloud/${NAME}`, `^${f.base}`]);
		state.result = { branch: `omp-cloud/${NAME}`, head, filesChanged: 1, insertions: 1, deletions: 0, pushed: true };

		const { cloud, host } = await setup({ fixture: f });
		const out = await call(cloud, host, { op: "pull", thread: "k3x9fa" });
		expect(out.text).toContain(`Fetched omp-cloud/${NAME} at ${head.slice(0, 8)} (+1 −0, 1 files)`);
		expect(await git(f.work, ["rev-parse", `omp-cloud/${NAME}`])).toBe(head);

		state.bundle = undefined;
		await expect(call(cloud, host, { op: "pull", thread: "k3x9fa" })).rejects.toThrow(`☁ ${NAME} has no result yet`);
	});
});

describe("delegation by the model", () => {
	async function sessionFile(): Promise<string> {
		const id = "0198aaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee";
		const file = join(await tmp(), `2026-10-19T10-00-00-000Z_${id}.jsonl`);
		const lines = [
			{ type: "session", version: 3, id, cwd: "/work" },
			{ type: "message", id: "e1", parentId: null, message: { role: "user", content: "please delegate" } },
			{ type: "custom_message", id: "c1", parentId: "e1", customType: "omp-cloud", content: "[cloud] old", display: true, details: { kind: "turn" } },
			{ type: "message", id: "a1", parentId: "c1", message: { role: "assistant", content: [{ type: "toolCall", id: "call_9", name: "cloud", arguments: { op: "delegate", task: "t" } }] } },
		];
		await writeFile(file, `${lines.map(l => JSON.stringify(l)).join("\n")}\n`);
		return file;
	}

	test("uploads without the pending tool call or cards, tags the thread as the tool's, shows the launch card when the agent is idle, and wakes the model with results", async () => {
		const { cloud, host, pi, f } = await setup({ sessionFile: await sessionFile() });
		await writeFile(join(f.work, "untracked.txt"), "u\n");
		await writeFile(join(f.work, ".env.local"), "TOKEN=1\n");
		host.idle = false; // a tool runs while the agent streams
		state.phase = "Working";
		state.turns = [turn(1, { state: "running" })];

		const out = await call(cloud, host, { op: "delegate", task: "port the build to bazel" }, "call_9");
		expect(out.text).toContain(`Started cloud thread ${NAME} on ${NS}/coder; the result will be branch omp-cloud/${NAME}.`);
		expect(out.text).toContain("do not poll");
		expect(out.text).toContain("model x is not available");

		const post = state.reqs.find(r => r.method === "POST" && r.path === BASE);
		const request = JSON.parse(await (post?.parts?.request as Blob).text()) as { prompt: string; model: string; thinking: string };
		expect(request).toMatchObject({ prompt: "port the build to bazel", model: "anthropic/claude-sonnet-5-5", thinking: "high" });
		const conv = (await (post?.parts?.conversation as Blob).text()).trimEnd().split("\n").map(l => JSON.parse(l) as { id: string });
		expect(conv.map(l => l.id).slice(1)).toEqual(["e1"]);

		// The launch card waits while the agent is busy, then appears without a turn.
		expect(pi.sent).toHaveLength(0);
		host.idle = true;
		cloud.flushCards(host.host);
		expect(pi.sent).toHaveLength(1);
		expect(pi.sent[0]?.options).toEqual({ triggerTurn: false });
		const card = pi.sent[0]?.message.details as CardDetails;
		expect(card).toMatchObject({ kind: "launch", origin: "tool", thread: NAME, agent: "coder", withheld: [".env.local"], untracked: ["untracked.txt"], untrackedTotal: 1 });
		expect(pi.sent[0]?.message.content).toContain("Left on this machine: .env.local.");

		// The watcher is polling; the finished turn reaches the model: a turn when idle, an aside when busy.
		state.phase = "Idle";
		state.turns = [turn(1, { reply: "Ported." })];
		await host.tick();
		await until(() => pi.sent.length === 2);
		expect(pi.sent[1]?.options).toEqual({ triggerTurn: true });
		expect(pi.sent[1]?.message.content).toContain("[cloud] coder-k3x9fa turn 1 done: Title 1");
	});

	test("a thread started with /cloud only displays its cards", async () => {
		const { cloud, host, pi } = await setup();
		state.phase = "Working";
		state.turns = [turn(1, { state: "running" })];
		await cloud.handle("port the build", host.host as never);
		expect(host.notes).toEqual([["☁ Handing off to the cloud…", "info"]]);
		host.timeouts[0]?.();
		await until(() => host.notes.length >= 3);
		expect(host.notes[1]?.[0]).toBe(`☁ ${NAME} started on ${NS}/coder — /cloud watch k3x9fa`);
		expect(host.notes[2]?.[0]).toBe("☁ model x is not available on coder; using y");
		await until(() => pi.sent.length === 1);
		expect(pi.sent[0]?.options).toEqual({ triggerTurn: false });
		expect((pi.sent[0]?.message.details as CardDetails).origin).toBe("user");

		state.phase = "Idle";
		state.turns = [turn(1)];
		await host.tick();
		await until(() => pi.sent.length === 2);
		expect(pi.sent.map(s => s.options)).toEqual([{ triggerTurn: false }, { triggerTurn: false }]);
	});
});

describe("/cloud and the hub", () => {
	test("without an overlay-capable UI, /cloud posts an inventory card", async () => {
		for (const mode of ["rpc", undefined]) {
			const { cloud, host, pi } = await setup({ mode });
			await cloud.handle("", host.host as never);
			expect(pi.sent).toHaveLength(1);
			const card = pi.sent[0]?.message.details as CardDetails;
			expect(card.kind).toBe("inventory");
			expect(pi.sent[0]?.message.content).toContain(`${NS}/coder running 1/4, threads 1 working 1 waiting 3 idle`);
			expect(pi.sent[0]?.message.content).toContain("k3x9fa Waiting");
		}
	});

	test("in a TUI the hub opens as an overlay; its action runs, then the hub reopens until closed", async () => {
		const tui = await loadTui();
		if (!tui) throw new Error("the host pi-tui is required");
		const script = [["s"], ["q"]];
		const opened: unknown[] = [];
		const custom = (async (factory: (...a: unknown[]) => unknown, options: unknown) => {
			opened.push(options);
			const { promise, resolve } = Promise.withResolvers<unknown>();
			const hub = (await factory({ requestRender() {} }, { fg: (_c: string, t: string) => t, bold: (t: string) => t }, {}, resolve)) as {
				refresh(): Promise<void>;
				handleInput(d: string): void;
				dispose(): void;
			};
			await hub.refresh();
			// The first hub asks to send to the selected thread; the reopened one is closed.
			for (const key of script.shift() ?? ["q"]) hub.handleInput(key);
			const result = await promise;
			hub.dispose();
			return result;
		}) as never;
		const { cloud, host, pi } = await setup({
			mode: "tui",
			hostOpts: { custom, input: async () => "ship it", confirm: true },
		});
		cloud.setTui(tui);
		await cloud.handle("", host.host as never);

		expect(opened).toHaveLength(2);
		expect(opened[0]).toMatchObject({ overlay: true });
		const msg = state.reqs.find(r => r.path === `${BASE}/${NAME}/messages`);
		expect(msg?.json).toEqual({ text: "ship it" });
		expect(host.notes.map(n => n[0])).toContain(`☁ queued turn 3 on ${NAME}`);
		expect(pi.sent).toHaveLength(0);
	});

	test("a host that never builds the overlay falls back to the inventory card", async () => {
		const tui = await loadTui();
		const { cloud, host, pi } = await setup({ mode: "tui", hostOpts: { custom: (async () => undefined) as never } });
		cloud.setTui(tui);
		await cloud.handle("", host.host as never);
		expect((pi.sent[0]?.message.details as CardDetails).kind).toBe("inventory");
	});

	test("/cloud answer with text selects a matching option, anything else is free text", async () => {
		state.turns = [turn(1, { state: "running", questions: [{ id: 2, ask: 1, key: "b", text: "Which database?", askedAt: "", options: [{ label: "postgres" }, { label: "sqlite" }] }] })];
		const { cloud, host } = await setup();
		await cloud.handle("answer k3x9fa SQLite", host.host as never);
		await cloud.handle("answer k3x9fa the one in staging", host.host as never);
		const answers = state.reqs.filter(r => r.path.endsWith("/answer")).map(r => r.json);
		expect(answers).toEqual([{ selected: ["sqlite"] }, { answer: "the one in staging" }]);
		expect(host.notes.filter(n => n[0].startsWith("☁ answered"))).toHaveLength(2);
	});
});

describe("tool registration and schema", () => {
	test("approval tiers: reads are read, thread changes write, delegation and anything unknown exec", () => {
		const cases: [unknown, unknown][] = [
			[{ op: "list" }, "read"],
			[{ op: "status", thread: "x" }, "read"],
			[{ op: "send" }, "write"],
			[{ op: "answer" }, "write"],
			[{ op: "stop" }, "write"],
			[{ op: "pull" }, "write"],
			[{ op: "delegate", task: "t" }, { tier: "exec", reason: "ships the repository, this conversation and your omp setup to a cloud agent" }],
			[{ op: "format-disk" }, { tier: "exec", reason: "ships the repository, this conversation and your omp setup to a cloud agent" }],
			[undefined, { tier: "exec", reason: "ships the repository, this conversation and your omp setup to a cloud agent" }],
			["list", { tier: "exec", reason: "ships the repository, this conversation and your omp setup to a cloud agent" }],
		];
		for (const [args, want] of cases) expect(toolApproval(args)).toEqual(want as never);
	});

	test("the description teaches when to delegate, to check capacity, and not to poll", () => {
		for (const s of ["independent of this machine", "op=list", "capacity", "[cloud] messages", "do not poll", "op=answer", "omp-cloud/<thread>"]) {
			expect(TOOL_DESCRIPTION).toContain(s);
		}
	});

	test("registers one tool, `cloud`, or `cloud_agent` when the name is taken", () => {
		expect(chooseToolName(["read", "bash"])).toBe("cloud");
		expect(chooseToolName(["cloud"])).toBe("cloud_agent");
		const reg = (existing: string[]) => {
			const tools: { name: string; loadMode?: string; approval?: unknown; execute: (...a: unknown[]) => Promise<unknown> }[] = [];
			const pi = {
				zod: z,
				getAllTools: () => existing.map(name => ({ name })),
				registerTool: (t: (typeof tools)[number]) => void tools.push(t),
			} as unknown as ExtensionAPI;
			registerCloudTool(pi, new Cloud(fakePi().pi, "/own", {}));
			return tools;
		};
		expect(reg([]).map(t => t.name)).toEqual(["cloud"]);
		expect(reg(["cloud"]).map(t => t.name)).toEqual(["cloud_agent"]);
		const [tool] = reg([]);
		expect(tool?.loadMode).toBe("essential");
		expect(tool?.approval).toBe(toolApproval);
	});

	test("arguments from the model are validated before use", async () => {
		expect(parseToolParams({ op: "status", thread: "k3x9fa", selected: ["a", 3], junk: 1 })).toEqual({
			op: "status",
			thread: "k3x9fa",
			task: undefined,
			agent: undefined,
			text: undefined,
			selected: ["a"],
			answer: undefined,
		});
		expect(() => parseToolParams({ op: "reboot" })).toThrow("op must be one of delegate, list");
		expect(() => parseToolParams(null)).toThrow("expected an object");
		expect(() => parseToolParams({})).toThrow("op must be one of");
	});

	test("overview and status texts handle empty data", () => {
		expect(overviewText("token:dev", NS, [], [])).toBe(`Cloud agents in ${NS} (you are token:dev):\n- none\nThreads (newest first, up to 20):\n- none`);
		const d = { name: NAME, phase: "Idle", agent: "coder", model: "", turns: [], title: "t" } as unknown as ThreadDetail;
		expect(statusDetailText(d)).toBe(`${NAME} [Idle] on coder, model agent default, 0 turns: t\nResult: none yet`);
	});
});
