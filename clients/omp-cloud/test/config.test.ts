import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { agentDir, loadConfig, NOT_CONFIGURED, resolveConnection, resolveToken, selectAgent, selectNamespace } from "../src/config";
import type { Whoami } from "../src/types";
import { cleanup, tmp } from "./helpers";

afterEach(cleanup);

async function setup(user?: object, repo?: object): Promise<{ env: Record<string, string>; repoRoot: string; dir: string }> {
	const dir = await tmp();
	const repoRoot = await tmp();
	if (user) await writeFile(join(dir, "cloud.json"), JSON.stringify(user));
	if (repo) {
		await mkdir(join(repoRoot, ".omp"));
		await writeFile(join(repoRoot, ".omp", "cloud.json"), JSON.stringify(repo));
	}
	return { env: { PI_CODING_AGENT_DIR: dir }, repoRoot, dir };
}

describe("loadConfig", () => {
	test("defaults when nothing is configured", async () => {
		const { env } = await setup();
		const cfg = await loadConfig({ env });
		expect(cfg).toMatchObject({
			maxUploadMB: 512,
			forceBundle: false,
			ship: { settings: true, extensions: true, memory: true },
			excludeExtensions: [],
			repo: { secretFiles: [], secretEnv: [] },
		});
		expect(cfg.url).toBeUndefined();
	});

	test("agentDir is PI_CODING_AGENT_DIR or ~/.omp/agent", () => {
		expect(agentDir({ PI_CODING_AGENT_DIR: "/x/agent" }, "/home/u")).toBe("/x/agent");
		expect(agentDir({}, "/home/u")).toBe("/home/u/.omp/agent");
	});

	test("agent and namespace: env beats the repo file beats the user file", async () => {
		const { env, repoRoot } = await setup(
			{ url: "http://u.example/", namespace: "user-ns", agent: "user-agent", maxUploadMB: 64, forceBundle: true, ship: { memory: false } },
			{ namespace: "repo-ns", agent: "repo-agent", secretFiles: [".env"], secretEnv: ["A", "B"] },
		);
		let cfg = await loadConfig({ env, repoRoot });
		expect(cfg).toMatchObject({
			url: "http://u.example",
			namespace: "repo-ns",
			agent: "repo-agent",
			maxUploadMB: 64,
			forceBundle: true,
			ship: { settings: true, extensions: true, memory: false },
			repo: { secretFiles: [".env"], secretEnv: ["A", "B"] },
		});

		cfg = await loadConfig({ env: { ...env, OMP_CLOUD_NAMESPACE: "env-ns", OMP_CLOUD_AGENT: "env-agent", OMP_CLOUD_URL: "http://e.example" }, repoRoot });
		expect(cfg).toMatchObject({ url: "http://e.example", namespace: "env-ns", agent: "env-agent" });

		cfg = await loadConfig({ env });
		expect(cfg).toMatchObject({ namespace: "user-ns", agent: "user-agent" });
	});

	test("an invalid config file is a readable error", async () => {
		const { env, dir } = await setup();
		await writeFile(join(dir, "cloud.json"), "{nope");
		await expect(loadConfig({ env })).rejects.toThrow(/cloud\.json is not valid/);
	});
});

describe("token resolution", () => {
	test("environment, then token, then tokenCommand", async () => {
		const { env } = await setup({ url: "http://u", token: "file-token", tokenCommand: ["printf", "cmd-token\n"] });
		const cfg = await loadConfig({ env });
		expect(await resolveToken(cfg, { ...env, OMP_CLOUD_TOKEN: "env-token" })).toBe("env-token");
		expect(await resolveToken(cfg, env)).toBe("file-token");
		delete cfg.token;
		expect(await resolveToken(cfg, env)).toBe("cmd-token");
	});

	test("tokenCommand failure is reported", async () => {
		const { env } = await setup({ tokenCommand: ["sh", "-c", "echo boom >&2; exit 3"] });
		await expect(resolveToken(await loadConfig({ env }), env)).rejects.toThrow("tokenCommand failed: boom");
	});

	test("a missing URL or token is the not-configured error", async () => {
		const { env } = await setup({ url: "http://u" });
		await expect(resolveConnection(await loadConfig({ env }), env)).rejects.toThrow(NOT_CONFIGURED);
		const { env: env2 } = await setup({ token: "t" });
		await expect(resolveConnection(await loadConfig({ env: env2 }), env2)).rejects.toThrow(NOT_CONFIGURED);
		const ok = await setup({ url: "http://u", token: "t" });
		expect(await resolveConnection(await loadConfig({ env: ok.env }), ok.env)).toEqual({ url: "http://u", token: "t" });
	});
});

describe("agent and namespace selection", () => {
	const agent = (namespace: string, name: string) => ({
		namespace,
		name,
		ready: true,
		defaultModel: "m",
		modelPatterns: [],
		gitHosts: [],
		maxUploadBytes: 1,
	});
	const who = (...a: [string, string][]): Whoami => ({ user: "token:dev", agents: a.map(([n, m]) => agent(n, m)) });

	test("the only agent is used", () => {
		expect(selectAgent(who(["omp-dev", "coder"]), {})).toMatchObject({ namespace: "omp-dev", name: "coder" });
	});

	test("none available", () => {
		expect(() => selectAgent(who(), {})).toThrow("☁ no cloud agent is available to token:dev");
	});

	test("several agents are listed", () => {
		expect(() => selectAgent(who(["a", "x"], ["b", "y"]), {})).toThrow(/several cloud agents are available \(a\/x, b\/y\)/);
	});

	test("config narrows by namespace and agent", () => {
		const w = who(["a", "x"], ["b", "x"], ["b", "y"]);
		expect(selectAgent(w, { namespace: "b", agent: "y" })).toMatchObject({ namespace: "b", name: "y" });
		expect(selectAgent(w, { agent: "y" })).toMatchObject({ namespace: "b", name: "y" });
		expect(() => selectAgent(w, { namespace: "a", agent: "y" })).toThrow("a/y is not available");
	});

	test("namespace: configured, else the only one", () => {
		expect(selectNamespace(who(["a", "x"], ["a", "y"]), {})).toBe("a");
		expect(selectNamespace(who(["a", "x"], ["b", "y"]), { namespace: "b" })).toBe("b");
		expect(() => selectNamespace(who(["a", "x"], ["b", "y"]), {})).toThrow(/several namespaces/);
	});
});
