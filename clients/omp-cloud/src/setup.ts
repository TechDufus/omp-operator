import { copyFile, mkdir, readdir, realpath, stat, symlink, writeFile } from "node:fs/promises";
import { join } from "node:path";
import type { Database as BunDatabase } from "bun:sqlite";
import { run } from "./exec";
import { mb } from "./snapshot";
import type { ShipConfig } from "./config";

export const MAX_SETUP_BYTES = 64 * 1024 * 1024;
export const MEMORY_WARNING = "memory copied without a snapshot; it may miss the last few writes";

export interface SetupOptions {
	agentDir: string;
	ship: ShipConfig;
	excludeExtensions: string[];
	/** This extension's own directory; never shipped. */
	ownRoot: string;
	/** `blob:sha256:` hashes the conversation references. */
	blobHashes: string[];
	/** Scratch directory for staging and the archive. */
	tmpDir: string;
}

export interface SetupResult {
	path: string;
	bytes: number;
	warnings: string[];
}

type DatabaseCtor = new (path: string, options?: { readonly?: boolean }) => BunDatabase;

async function exists(p: string): Promise<boolean> {
	try {
		await stat(p);
		return true;
	} catch {
		return false;
	}
}

/** Stages `src` as a symlink; `tar -h` follows it when packing. */
async function stageLink(src: string, dest: string): Promise<boolean> {
	if (!(await exists(src))) return false;
	await mkdir(join(dest, ".."), { recursive: true });
	await symlink(await realpath(src), dest);
	return true;
}

// bun:sqlite exists only under Bun and an extension host may not expose it; a static
// import would fail the whole extension at load time, so load it lazily and fall back.
async function loadDatabase(): Promise<DatabaseCtor | undefined> {
	try {
		const mod = (await import("bun:sqlite")) as { Database: DatabaseCtor };
		return mod.Database;
	} catch {
		return undefined;
	}
}

const SIDECAR_RE = /\.db-(wal|shm)$|-(wal|shm)$/;

/**
 * Copies a memory tree, writing each SQLite database as a consistent
 * snapshot (`serialize`) and skipping the -wal/-shm sidecars. Without a usable
 * bun:sqlite the database and its sidecars are copied as they are.
 */
async function stageMemory(src: string, dest: string, Database: DatabaseCtor | undefined, warnings: string[]): Promise<void> {
	await mkdir(dest, { recursive: true });
	for (const entry of await readdir(src, { withFileTypes: true })) {
		const from = join(src, entry.name);
		const to = join(dest, entry.name);
		const isDir = entry.isDirectory() || (entry.isSymbolicLink() && (await stat(from)).isDirectory());
		if (isDir) {
			await stageMemory(from, to, Database, warnings);
			continue;
		}
		if (entry.name.endsWith(".db")) {
			if (Database) {
				let db: BunDatabase | undefined;
				try {
					db = new Database(from, { readonly: true });
					await writeFile(to, db.serialize());
					continue;
				} catch {
					// fall through to the plain copy
				} finally {
					db?.close();
				}
			}
			if (!warnings.includes(MEMORY_WARNING)) warnings.push(MEMORY_WARNING);
			await copyFile(from, to);
			for (const side of ["-wal", "-shm"]) {
				if (await exists(from + side)) await copyFile(from + side, to + side);
			}
			continue;
		}
		if (SIDECAR_RE.test(entry.name)) {
			// Copied alongside its database only in the fallback above.
			continue;
		}
		await copyFile(from, to);
	}
}

/**
 * Packs the user's omp setup into `<tmpDir>/setup.tar.gz`: AGENTS.md, RULES.md,
 * skills, and per `ship` the settings, extensions and Mnemopi memory, plus the
 * blobs the conversation references.
 */
export async function buildSetup(opts: SetupOptions): Promise<SetupResult> {
	const stage = join(opts.tmpDir, "setup-stage");
	await mkdir(stage, { recursive: true });
	const warnings: string[] = [];
	const dir = opts.agentDir;

	for (const name of ["AGENTS.md", "RULES.md", "skills"]) await stageLink(join(dir, name), join(stage, name));
	if (opts.ship.settings) await stageLink(join(dir, "config.yml"), join(stage, "config.yml"));

	if (opts.ship.extensions) {
		const extDir = join(dir, "extensions");
		const own = await realpath(opts.ownRoot).catch(() => opts.ownRoot);
		let names: string[] = [];
		try {
			names = await readdir(extDir);
		} catch {
			// no extensions directory
		}
		for (const name of names) {
			if (opts.excludeExtensions.includes(name)) continue;
			const real = await realpath(join(extDir, name)).catch(() => undefined);
			if (!real || real === own) continue;
			await stageLink(join(extDir, name), join(stage, "extensions", name));
		}
	}

	if (opts.ship.memory) {
		const mem = join(dir, "memories", "mnemopi");
		if (await exists(mem)) await stageMemory(mem, join(stage, "memories", "mnemopi"), await loadDatabase(), warnings);
	}

	if (opts.blobHashes.length > 0) {
		const blobDir = join(dir, "blobs");
		let files: string[] = [];
		try {
			files = await readdir(blobDir);
		} catch {
			// no blob store
		}
		const wanted = new Set(opts.blobHashes);
		for (const f of files) {
			const hash = f.split(".")[0] ?? "";
			if (wanted.has(hash)) await stageLink(join(blobDir, f), join(stage, "blobs", f));
		}
	}

	const out = join(opts.tmpDir, "setup.tar.gz");
	const r = await run("tar", ["-czhf", out, "-C", stage, "."]);
	if (r.code !== 0) throw new Error(`cannot pack the omp setup: ${r.stderr.trim().slice(0, 300) || `tar exit ${r.code}`}`);
	const bytes = (await stat(out)).size;
	if (bytes > MAX_SETUP_BYTES) {
		throw new Error(
			`omp setup is ${mb(bytes)} MB, over the ${mb(MAX_SETUP_BYTES)} MB limit; use excludeExtensions or ship.memory/ship.extensions in cloud.json`,
		);
	}
	return { path: out, bytes, warnings };
}
