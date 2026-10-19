import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { git } from "../src/snapshot";

// Isolate every git call from the developer's own configuration.
process.env.GIT_CONFIG_GLOBAL = "/dev/null";
process.env.GIT_CONFIG_NOSYSTEM = "1";
process.env.GIT_TERMINAL_PROMPT = "0";

const dirs: string[] = [];

export async function tmp(prefix = "omp-cloud-test-"): Promise<string> {
	const d = await mkdtemp(join(tmpdir(), prefix));
	dirs.push(d);
	return d;
}

export async function cleanup(): Promise<void> {
	await Promise.all(dirs.splice(0).map(d => rm(d, { recursive: true, force: true })));
}

export async function configureRepo(dir: string): Promise<void> {
	await git(dir, ["config", "user.name", "Test User"]);
	await git(dir, ["config", "user.email", "test@example.com"]);
	await git(dir, ["config", "commit.gpgsign", "false"]);
}

export async function commitFile(dir: string, path: string, content: string, msg = `add ${path}`): Promise<string> {
	await mkdir(join(dir, path, ".."), { recursive: true });
	await writeFile(join(dir, path), content);
	await git(dir, ["add", "--", path]);
	await git(dir, ["commit", "-q", "-m", msg]);
	return git(dir, ["rev-parse", "HEAD"]);
}

export const REMOTE_URL = "http://git.example.test/app.git";

export interface Fixture {
	root: string;
	bare: string;
	work: string;
	/** The commit the remote's main branch points at. */
	base: string;
}

/**
 * A bare "remote" and a clone of it whose origin URL is an http URL (resolved to
 * the bare repo through insteadOf), with `main` pushed and upstream set.
 */
export async function remoteFixture(): Promise<Fixture> {
	const root = await tmp();
	const bare = join(root, "origin.git");
	const work = join(root, "work");
	await git(root, ["init", "-q", "--bare", "-b", "main", bare]);
	await git(root, ["init", "-q", "-b", "main", work]);
	await configureRepo(work);
	await git(work, ["config", `url.${bare}.insteadOf`, REMOTE_URL]);
	await git(work, ["remote", "add", "origin", REMOTE_URL]);
	await commitFile(work, "a.txt", "one\n");
	await commitFile(work, ".gitignore", ".env\n");
	await git(work, ["push", "-q", "-u", "origin", "main"]);
	const base = await git(work, ["rev-parse", "HEAD"]);
	return { root, bare, work, base };
}
