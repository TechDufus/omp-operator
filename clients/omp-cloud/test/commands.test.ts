import { describe, expect, test } from "bun:test";
import {
	completions,
	parseCommand,
	resolveCommand,
	resolveThread,
	USAGE_LAUNCH,
	USAGE_SEND,
	type Command,
} from "../src/commands";

const NAMES = ["coder-k3x9fa", "coder-ab12cd", "coder-k3zzzz", "docs-q9w8e7"];

function run(args: string, names: string[] = NAMES): Command {
	return resolveCommand(parseCommand(args), names);
}

describe("command parsing", () => {
	const cases: [string, Command][] = [
		["fix the bug", { kind: "launch", task: "fix the bug" }],
		["  fix the bug  ", { kind: "launch", task: "fix the bug" }],
		["new stop the flake", { kind: "launch", task: "stop the flake" }],
		["NEW   Stop   the flake", { kind: "launch", task: "Stop   the flake" }],
		["new", { kind: "error", message: USAGE_LAUNCH }],
		["stop k3x9fa", { kind: "stop", thread: "coder-k3x9fa" }],
		["STOP k3x9fa", { kind: "stop", thread: "coder-k3x9fa" }],
		["", { kind: "hub" }],
		["   ", { kind: "hub" }],
		["answer k3x9fa", { kind: "answer", thread: "coder-k3x9fa", text: "" }],
		["answer k3x9fa  use the second one", { kind: "answer", thread: "coder-k3x9fa", text: "use the second one" }],
		["answer", { kind: "error", message: "☁ usage: /cloud answer <id>" }],
		["LIST", { kind: "list" }],
		["list everything please", { kind: "list" }],
		["send k3x9fa hi there", { kind: "send", thread: "coder-k3x9fa", text: "hi there" }],
		["send k3x9fa", { kind: "error", message: USAGE_SEND }],
		["send", { kind: "error", message: USAGE_SEND }],
		["pull coder-ab12cd", { kind: "pull", thread: "coder-ab12cd" }],
		["watch ab12cd", { kind: "watch", thread: "coder-ab12cd" }],
		["show docs", { kind: "show", thread: "docs-q9w8e7" }],
		["resume q9w8e7", { kind: "resume", thread: "docs-q9w8e7" }],
		["delete k3x9fa", { kind: "delete", thread: "coder-k3x9fa" }],
		["unwatch", { kind: "unwatch" }],
		["status", { kind: "status" }],
		["help", { kind: "help" }],
		["pull", { kind: "error", message: "☁ usage: /cloud pull <id>" }],
	];
	for (const [args, want] of cases) {
		test(JSON.stringify(args), () => expect(run(args)).toEqual(want));
	}

	test("an id verb with no matching thread explains how to launch a task that starts with the verb", () => {
		expect(run("stop the flaky test")).toEqual({
			kind: "error",
			message: '☁ no thread matches "the"; to start a cloud task beginning with "stop", use /cloud new <task>',
		});
	});

	test("an ambiguous prefix lists the candidates", () => {
		const c = run("pull coder");
		expect(c.kind).toBe("error");
		expect(c.kind === "error" && c.message).toContain("coder-k3x9fa, coder-ab12cd, coder-k3zzzz");
	});

	test("parsing alone does not need any thread names", () => {
		expect(parseCommand("stop k3x9fa")).toEqual({ kind: "stop", token: "k3x9fa", text: "" });
		expect(parseCommand("send k3x9fa hi there")).toEqual({ kind: "send", token: "k3x9fa", text: "hi there" });
	});
});

describe("resolveThread", () => {
	test("exact name beats prefix; 6-character suffix; unique prefix", () => {
		const names = ["a-xxxxxx", "a-xxxxxx-b", "c-yyyyyy"];
		expect(resolveThread("a-xxxxxx", names)).toEqual({ name: "a-xxxxxx" });
		expect(resolveThread("yyyyyy", names)).toEqual({ name: "c-yyyyyy" });
		expect(resolveThread("c-", names)).toEqual({ name: "c-yyyyyy" });
		expect(resolveThread("a-", names)).toEqual({ error: "ambiguous", candidates: ["a-xxxxxx", "a-xxxxxx-b"] });
		expect(resolveThread("zzz", names)).toEqual({ error: "none", candidates: [] });
	});
});

describe("completions", () => {
	test("verbs while the first word is typed", () => {
		expect(completions("pu", NAMES)?.map(c => c.value)).toEqual(["pull"]);
		expect(completions("", NAMES)?.map(c => c.value)).toContain("resume");
		expect(completions("zz", NAMES)).toBeNull();
	});

	test("cached thread names after an id verb; the value carries the verb", () => {
		expect(completions("pull ", NAMES)?.map(c => c.value)).toEqual(NAMES.map(n => `pull ${n}`));
		expect(completions("pull coder-k3", NAMES)?.map(c => c.label)).toEqual(["coder-k3x9fa", "coder-k3zzzz"]);
	});

	test("nothing is offered after the id or for verbs without one", () => {
		expect(completions("send coder-k3x9fa ", NAMES)).toBeNull();
		expect(completions("list ", NAMES)).toBeNull();
		expect(completions("fix the ", NAMES)).toBeNull();
	});
});
