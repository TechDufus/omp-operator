import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { ApiError, CloudClient } from "../src/api";
import { loadConfig } from "../src/config";
import { launchHandoff } from "../src/launch";
import { run } from "../src/exec";
import { git } from "../src/snapshot";
import type { HandoffRequest, WhoamiAgent } from "../src/types";
import { cleanup, commitFile, remoteFixture, REMOTE_URL, tmp } from "./helpers";

afterEach(cleanup);

interface Captured {
	method: string;
	path: string;
	auth: string | null;
	keys: string[];
	request?: HandoffRequest;
	parts: Record<string, Blob | string>;
}

let server: ReturnType<typeof Bun.serve>;
let captured: Captured[] = [];
const reply = (): Response =>
	Response.json({ name: "coder-k3x9fa", namespace: "omp-dev", warnings: ["model x is not available on coder; using y"] }, { status: 201 });

beforeAll(() => {
	server = Bun.serve({
		port: 0,
		async fetch(req) {
			const url = new URL(req.url);
			const c: Captured = { method: req.method, path: url.pathname, auth: req.headers.get("authorization"), keys: [], parts: {} };
			captured.push(c);
			if (req.method === "POST" && url.pathname.endsWith("/threads")) {
				const fd = await req.formData();
				for (const [k, v] of fd.entries()) {
					c.keys.push(k);
					c.parts[k] = v;
				}
				c.request = JSON.parse(await (c.parts.request as Blob).text()) as HandoffRequest;
				return reply();
			}
			if (url.pathname.endsWith("/whoami")) return Response.json({ user: "token:dev", agents: [] });
			if (url.pathname.endsWith("/result")) {
				return new Response("bundle-bytes", { headers: { "X-Omp-Branch": "omp-cloud/t", "X-Omp-Head": "abc", "X-Omp-Base": "def" } });
			}
			return Response.json({ error: "not found" }, { status: 404 });
		},
	});
});

afterAll(() => server.stop(true));

const base = () => `http://127.0.0.1:${server.port}`;

describe("CloudClient", () => {
	test("sends the bearer token and surfaces the server's error message", async () => {
		captured = [];
		const client = new CloudClient(base(), "tok");
		await client.whoami();
		expect(captured[0]?.auth).toBe("Bearer tok");
		const err = await client.thread("ns", "missing").catch((e: unknown) => e);
		expect(err).toBeInstanceOf(ApiError);
		expect((err as ApiError).status).toBe(404);
		expect((err as ApiError).message).toBe("not found");
	});

	test("downloads the result bundle with its headers", async () => {
		const client = new CloudClient(base(), "tok");
		const dest = join(await tmp(), "r.bundle");
		const h = await client.downloadResult("ns", "t", dest);
		expect(h).toEqual({ branch: "omp-cloud/t", head: "abc", base: "def" });
		expect(await readFile(dest, "utf8")).toBe("bundle-bytes");
	});

	test("multipart parts arrive in the order request, source, conversation, setup", async () => {
		captured = [];
		const dir = await tmp();
		await writeFile(join(dir, "s.bundle"), "SRC");
		await writeFile(join(dir, "setup.tgz"), "SETUP");
		const request = { agent: "coder", prompt: "p" } as HandoffRequest;
		const client = new CloudClient(base(), "tok");
		await client.createThread("omp-dev", {
			request,
			source: join(dir, "s.bundle"),
			conversation: { filename: "c.jsonl", data: new TextEncoder().encode("CONV") },
			setup: join(dir, "setup.tgz"),
		});
		const c = captured[0] as Captured;
		expect(c.path).toBe("/api/v1/namespaces/omp-dev/threads");
		expect(c.keys).toEqual(["request", "source", "conversation", "setup"]);
		expect(await (c.parts.source as Blob).text()).toBe("SRC");
		expect(await (c.parts.conversation as Blob).text()).toBe("CONV");
		expect(await (c.parts.setup as Blob).text()).toBe("SETUP");
		expect((c.parts.conversation as File).name).toBe("c.jsonl");
	});

	test("optional parts are omitted, not sent empty", async () => {
		captured = [];
		await new CloudClient(base(), "tok").createThread("ns", { request: { agent: "a" } as HandoffRequest });
		expect((captured[0] as Captured).keys).toEqual(["request"]);
	});
});

describe("launchHandoff against a fake cloud API", () => {
	const agent = (over: Partial<WhoamiAgent> = {}): WhoamiAgent => ({
		namespace: "omp-dev",
		name: "coder",
		ready: true,
		defaultModel: "anthropic/claude-sonnet-5-5",
		modelPatterns: ["anthropic/*"],
		gitHosts: [],
		maxUploadBytes: 1 << 30,
		...over,
	});

	test("uploads the delta bundle, the rewritten conversation, the setup and the request fields", async () => {
		captured = [];
		const f = await remoteFixture();
		await commitFile(f.work, "b.txt", "unpushed\n");
		await writeFile(join(f.work, "a.txt"), "one\nDIRTY\n");
		await writeFile(join(f.work, ".env"), "SCRATCH_TOKEN=scratch-secret-value-xyz\n");
		await mkdir(join(f.work, ".omp"));
		await writeFile(join(f.work, ".omp", "cloud.json"), JSON.stringify({ secretFiles: [".env"], secretEnv: ["E2E_ENV"] }));

		const agentDir = await tmp("omp-cloud-agent-");
		await writeFile(join(agentDir, "AGENTS.md"), "# OMP User Preferences\n");
		const sessionFile = join(agentDir, "2026-10-18T22-11-00-000Z_0198aaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee.jsonl");
		await writeFile(
			sessionFile,
			[
				JSON.stringify({ type: "session_title", v: 1, title: "t", updatedAt: "x", pad: "" }),
				JSON.stringify({ type: "session", id: "0198aaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee", cwd: f.work }),
				JSON.stringify({ type: "message", message: { role: "user", content: "hello" } }),
				"",
			].join("\n"),
		);

		const env = { PI_CODING_AGENT_DIR: agentDir, E2E_ENV: "env-value-1234" };
		const cfg = await loadConfig({ env, repoRoot: f.work });
		const result = await launchHandoff({
			host: {
				cwd: f.work,
				model: { provider: "anthropic", id: "claude-sonnet-5-5" },
				sessionManager: { getSessionFile: () => sessionFile, getSessionDir: () => agentDir },
			},
			pi: { getThinkingLevel: () => "high" },
			cfg,
			client: new CloudClient(base(), "tok"),
			agent: agent(),
			task: "do it",
			env,
			home: "/home/test",
			agentDir,
			ownRoot: await tmp("omp-cloud-own-"),
		});

		expect(result.response.name).toBe("coder-k3x9fa");
		expect(result.response.warnings).toEqual(["model x is not available on coder; using y"]);

		const c = captured[0] as Captured;
		expect(c.keys).toEqual(["request", "source", "conversation", "setup"]);
		const r = c.request as HandoffRequest;
		expect(r).toMatchObject({
			agent: "coder",
			prompt: "do it",
			model: "anthropic/claude-sonnet-5-5",
			thinking: "high",
			repo: {
				name: "work",
				remoteURL: REMOTE_URL,
				baseRef: "main",
				baseCommit: f.base,
				localBranch: "main",
			},
			paths: { home: "/home/test", workspace: f.work },
			author: { name: "Test User", email: "test@example.com" },
			secretEnv: { E2E_ENV: "env-value-1234" },
		});
		expect(r.repo.handoffCommit).toMatch(/^[0-9a-f]{40}$/);
		expect(r.secretFiles).toEqual([{ path: ".env", content: Buffer.from("SCRATCH_TOKEN=scratch-secret-value-xyz\n").toString("base64") }]);
		expect(r.conversationFile).toMatch(/^2026-10-18T22-11-00-000Z_[0-9a-f-]{36}\.jsonl$/);
		expect(r.conversationFile).not.toContain("0198aaaa");

		const conv = (await (c.parts.conversation as Blob).text()).split("\n");
		const header = JSON.parse(conv[1] as string) as { id: string; parentSession: string };
		expect(r.conversationFile.endsWith(`${header.id}.jsonl`)).toBe(true);
		expect(header.parentSession).toBe(sessionFile);
		expect(conv[2]).toContain('"hello"');

		// The uploaded bundle verifies against a fresh clone of the remote and carries the WIP commit.
		const bundle = join(await tmp(), "up.bundle");
		await Bun.write(bundle, c.parts.source as Blob);
		const fresh = join(await tmp(), "fresh");
		await git(f.root, ["clone", "-q", f.bare, fresh]);
		await git(fresh, ["bundle", "verify", bundle]);
		await git(fresh, ["fetch", "-q", bundle, "refs/omp-cloud/handoff"]);
		expect(await git(fresh, ["rev-parse", "FETCH_HEAD"])).toBe(r.repo.handoffCommit);

		const setup = join(await tmp(), "setup.tar.gz");
		await Bun.write(setup, c.parts.setup as Blob);
		const listing = await run("tar", ["-tzf", setup]);
		expect(listing.stdout).toContain("AGENTS.md");
	});

	test("an unpushed nothing-to-send delta omits the source part", async () => {
		captured = [];
		const f = await remoteFixture();
		const agentDir = await tmp("omp-cloud-agent-");
		const env = { PI_CODING_AGENT_DIR: agentDir };
		await launchHandoff({
			host: { cwd: f.work, model: undefined, sessionManager: { getSessionFile: () => undefined, getSessionDir: () => agentDir } },
			pi: { getThinkingLevel: () => undefined },
			cfg: await loadConfig({ env, repoRoot: f.work }),
			client: new CloudClient(base(), "tok"),
			agent: agent(),
			task: "x",
			env,
			home: "/h",
			agentDir,
			ownRoot: await tmp("omp-cloud-own-"),
		});
		const c = captured[0] as Captured;
		expect(c.keys).toEqual(["request", "setup"]);
		expect(c.request?.conversationFile).toBe("");
		expect(c.request?.model).toBeUndefined();
		expect(c.request?.thinking).toBeUndefined();
		expect(c.request?.repo.handoffCommit).toBe(f.base);
	});

	test("the agent's upload limit caps the bundle", async () => {
		const f = await remoteFixture();
		await commitFile(f.work, "b.txt", "x\n");
		const agentDir = await tmp("omp-cloud-agent-");
		const env = { PI_CODING_AGENT_DIR: agentDir };
		await expect(
			launchHandoff({
				host: { cwd: f.work, model: undefined, sessionManager: { getSessionFile: () => undefined, getSessionDir: () => agentDir } },
				pi: { getThinkingLevel: () => undefined },
				cfg: await loadConfig({ env, repoRoot: f.work }),
				client: new CloudClient(base(), "tok"),
				agent: agent({ maxUploadBytes: 10 }),
				task: "x",
				env,
				home: "/h",
				agentDir,
				ownRoot: await tmp("omp-cloud-own-"),
			}),
		).rejects.toThrow(/repository bundle is .* MB, over the .* MB upload limit/);
	});
});
