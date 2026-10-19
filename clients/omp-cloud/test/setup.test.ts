import { afterEach, describe, expect, test } from "bun:test";
import { Database } from "bun:sqlite";
import { lstat, mkdir, readdir, readFile, symlink, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { run } from "../src/exec";
import { buildSetup, type SetupOptions } from "../src/setup";
import { cleanup, tmp } from "./helpers";

afterEach(cleanup);

const HASH = "b".repeat(64);

async function put(path: string, content: string): Promise<void> {
	await mkdir(join(path, ".."), { recursive: true });
	await writeFile(path, content);
}

async function listAll(dir: string, prefix = ""): Promise<string[]> {
	const out: string[] = [];
	for (const e of await readdir(dir, { withFileTypes: true })) {
		const rel = prefix + e.name;
		if (e.isDirectory()) out.push(...(await listAll(join(dir, e.name), `${rel}/`)));
		else out.push(rel);
	}
	return out.sort();
}

async function extract(archive: string): Promise<string> {
	const dir = await tmp("omp-cloud-x-");
	const r = await run("tar", ["-xzf", archive, "-C", dir]);
	expect(r.code).toBe(0);
	return dir;
}

interface Layout {
	agentDir: string;
	ownRoot: string;
	opts: SetupOptions;
}

async function layout(over: Partial<SetupOptions> = {}): Promise<Layout> {
	const agentDir = await tmp("omp-cloud-agent-");
	const ownRoot = await tmp("omp-cloud-own-");
	const elsewhere = await tmp("omp-cloud-else-");
	await put(join(elsewhere, "AGENTS.md"), "# OMP User Preferences\n");
	await symlink(join(elsewhere, "AGENTS.md"), join(agentDir, "AGENTS.md"));
	await put(join(agentDir, "RULES.md"), "rules\n");
	await put(join(agentDir, "WATCHDOG.md"), "never shipped\n");
	await put(join(agentDir, "cloud.json"), '{"token":"never shipped"}');
	await put(join(agentDir, "config.yml"), "modelRoles:\n  default: a/b\n");
	await put(join(agentDir, "skills", "demo", "SKILL.md"), "---\nname: demo\n---\n");
	await put(join(agentDir, "extensions", "keep.ts"), "// keep\n");
	await put(join(agentDir, "extensions", "herd.ts"), "// excluded\n");
	await put(join(ownRoot, "package.json"), "{}");
	await symlink(ownRoot, join(agentDir, "extensions", "omp-cloud"));
	await put(join(agentDir, "memories", "mnemopi", "notes.txt"), "plain file\n");
	await put(join(agentDir, "blobs", HASH), "blob-bytes");
	await put(join(agentDir, "blobs", `${"c".repeat(64)}.png`), "unreferenced");
	return {
		agentDir,
		ownRoot,
		opts: {
			agentDir,
			ship: { settings: true, extensions: true, memory: true },
			excludeExtensions: ["herd.ts"],
			ownRoot,
			blobHashes: [HASH],
			tmpDir: await tmp("omp-cloud-out-"),
			...over,
		},
	};
}

describe("buildSetup", () => {
	test("ships the setup, minus excluded extensions, this extension, and anything not on the allowlist", async () => {
		const l = await layout();
		const r = await buildSetup(l.opts);
		const dir = await extract(r.path);
		expect(await listAll(dir)).toEqual([
			"AGENTS.md",
			"RULES.md",
			"blobs/" + HASH,
			"config.yml",
			"extensions/keep.ts",
			"memories/mnemopi/notes.txt",
			"skills/demo/SKILL.md",
		]);
		// Symlinks are dereferenced into regular files.
		expect((await lstat(join(dir, "AGENTS.md"))).isFile()).toBe(true);
		expect(await readFile(join(dir, "AGENTS.md"), "utf8")).toBe("# OMP User Preferences\n");
		expect(r.bytes).toBeGreaterThan(0);
	});

	test("ship flags drop settings, extensions and memory; user context and skills always ship", async () => {
		const l = await layout({ ship: { settings: false, extensions: false, memory: false } });
		const dir = await extract((await buildSetup(l.opts)).path);
		expect(await listAll(dir)).toEqual(["AGENTS.md", "RULES.md", "blobs/" + HASH, "skills/demo/SKILL.md"]);
	});

	test("a Mnemopi SQLite database round-trips through serialize, including uncheckpointed WAL writes; sidecars are skipped", async () => {
		const l = await layout();
		const dbPath = join(l.agentDir, "memories", "mnemopi", "mnemopi.db");
		const bankDb = join(l.agentDir, "memories", "mnemopi", "banks", "global", "bank.db");
		await mkdir(join(bankDb, ".."), { recursive: true });
		const dbs: Database[] = [];
		for (const p of [dbPath, bankDb]) {
			const db = new Database(p);
			db.run("PRAGMA journal_mode=WAL");
			db.run("PRAGMA wal_autocheckpoint=0");
			db.run("CREATE TABLE memories(id INTEGER PRIMARY KEY, text TEXT)");
			db.run("INSERT INTO memories(text) VALUES ('omp-cloud scratch codeword is plover'), ('second')");
			dbs.push(db);
		}
		// The writes live in the -wal files, which are still open.
		expect((await lstat(`${dbPath}-wal`)).size).toBeGreaterThan(0);

		const r = await buildSetup(l.opts);
		for (const db of dbs) db.close();
		expect(r.warnings).toEqual([]);

		const dir = await extract(r.path);
		const files = await listAll(dir);
		expect(files).toContain("memories/mnemopi/mnemopi.db");
		expect(files).toContain("memories/mnemopi/banks/global/bank.db");
		expect(files.filter(f => /-(wal|shm)$/.test(f))).toEqual([]);
		for (const rel of ["memories/mnemopi/mnemopi.db", "memories/mnemopi/banks/global/bank.db"]) {
			const copy = new Database(join(dir, rel), { readonly: true });
			expect(copy.query("SELECT text FROM memories ORDER BY id").all()).toEqual([
				{ text: "omp-cloud scratch codeword is plover" },
				{ text: "second" },
			]);
			copy.close();
		}
	});

	test("an archive over the cap is an error naming the remedies", async () => {
		const l = await layout();
		// Incompressible content beyond 64 MiB.
		const big = new Uint8Array(65 * 1024 * 1024);
		for (let i = 0; i < big.length; i += 65536) crypto.getRandomValues(big.subarray(i, Math.min(i + 65536, big.length)));
		await Bun.write(join(l.agentDir, "skills", "huge.bin"), big);
		await expect(buildSetup(l.opts)).rejects.toThrow(/omp setup is [\d.]+ MB, over the 64 MB limit.*excludeExtensions/);
	}, 60_000);
});
