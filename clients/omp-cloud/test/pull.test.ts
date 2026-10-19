import { afterEach, describe, expect, test } from "bun:test";
import { readdir, readFile, stat, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { run } from "../src/exec";
import { extractConversation, installConversation, isClean, pullBundle, threadBranch } from "../src/pull";
import { git } from "../src/snapshot";
import { cleanup, commitFile, remoteFixture, tmp } from "./helpers";

afterEach(cleanup);

const THREAD = "coder-k3x9fa";

/** A second clone of the remote plays the cloud pod: it commits on omp-cloud/<thread> and bundles the result. */
async function cloudResult(remoteBare: string, root: string, files: Record<string, string>, bundleName: string) {
	const pod = join(root, `pod-${bundleName}`);
	await git(root, ["clone", "-q", remoteBare, pod]);
	await git(pod, ["config", "user.name", "Cloud"]);
	await git(pod, ["config", "user.email", "cloud@example.com"]);
	await git(pod, ["config", "commit.gpgsign", "false"]);
	await git(pod, ["checkout", "-q", "-b", threadBranch(THREAD)]);
	for (const [p, c] of Object.entries(files)) await commitFile(pod, p, c);
	const bundle = join(root, `${bundleName}.bundle`);
	const base = await git(pod, ["rev-parse", "origin/main"]);
	await git(pod, ["bundle", "create", bundle, `refs/heads/${threadBranch(THREAD)}`, `^${base}`]);
	return { bundle, head: await git(pod, ["rev-parse", "HEAD"]), base, pod };
}

describe("pullBundle", () => {
	test("creates omp-cloud/<thread> from a result bundle without touching the checked-out branch", async () => {
		const f = await remoteFixture();
		const res = await cloudResult(f.bare, f.root, { "hello.txt": "hello\n" }, "r1");
		const before = await git(f.work, ["rev-parse", "HEAD"]);

		const out = await pullBundle({ root: f.work, thread: THREAD, bundlePath: res.bundle, baseCommit: res.base, remote: "origin" });

		expect(out).toEqual({ branch: `omp-cloud/${THREAD}`, head: res.head });
		expect(await git(f.work, ["rev-parse", `refs/heads/omp-cloud/${THREAD}`])).toBe(res.head);
		expect(await git(f.work, ["show", `omp-cloud/${THREAD}:hello.txt`])).toBe("hello");
		expect(await git(f.work, ["branch", "--show-current"])).toBe("main");
		expect(await git(f.work, ["rev-parse", "HEAD"])).toBe(before);
	});

	test("fast-forwards the branch when it is checked out, and force-moves it otherwise", async () => {
		const f = await remoteFixture();
		const first = await cloudResult(f.bare, f.root, { "one.txt": "1\n" }, "r1");
		await pullBundle({ root: f.work, thread: THREAD, bundlePath: first.bundle, baseCommit: first.base, remote: "origin" });
		await git(f.work, ["switch", "-q", `omp-cloud/${THREAD}`]);

		// Cloud continues the same branch: a longer result that contains the first.
		await commitFile(first.pod, "two.txt", "2\n");
		const second = join(f.root, "r2.bundle");
		await git(first.pod, ["bundle", "create", second, `refs/heads/omp-cloud/${THREAD}`, `^${first.base}`]);
		const tip = await git(first.pod, ["rev-parse", "HEAD"]);

		await pullBundle({ root: f.work, thread: THREAD, bundlePath: second, baseCommit: first.base, remote: "origin" });
		expect(await git(f.work, ["rev-parse", "HEAD"])).toBe(tip);
		expect(await readFile(join(f.work, "two.txt"), "utf8")).toBe("2\n");

		// From main, the branch is moved with branch -f.
		await git(f.work, ["switch", "-q", "main"]);
		await commitFile(first.pod, "three.txt", "3\n");
		const third = join(f.root, "r3.bundle");
		await git(first.pod, ["bundle", "create", third, `refs/heads/omp-cloud/${THREAD}`, `^${first.base}`]);
		const tip3 = await git(first.pod, ["rev-parse", "HEAD"]);
		await pullBundle({ root: f.work, thread: THREAD, bundlePath: third, baseCommit: first.base, remote: "origin" });
		expect(await git(f.work, ["rev-parse", `refs/heads/omp-cloud/${THREAD}`])).toBe(tip3);
		expect(await git(f.work, ["branch", "--show-current"])).toBe("main");
	});

	test("a repository that lacks the base commit gets an actionable error", async () => {
		const f = await remoteFixture();
		const res = await cloudResult(f.bare, f.root, { "x.txt": "x\n" }, "r1");
		const other = await tmp();
		await git(other, ["init", "-q", "-b", "main"]);
		await expect(
			pullBundle({ root: other, thread: THREAD, bundlePath: res.bundle, baseCommit: res.base, remote: "origin" }),
		).rejects.toThrow(`your repository lacks the thread's base commit ${res.base}; git fetch origin, then retry`);
	});
});

describe("isClean", () => {
	test("reports uncommitted and untracked changes", async () => {
		const f = await remoteFixture();
		expect(await isClean(f.work)).toBe(true);
		await writeFile(join(f.work, "new.txt"), "x");
		expect(await isClean(f.work)).toBe(false);
	});
});

describe("conversation transfer", () => {
	test("extracts the newest session file (top level first) and installs it with its artifacts", async () => {
		const src = await tmp();
		await writeFile(join(src, "old.jsonl"), "old");
		await writeFile(join(src, "new.jsonl"), "new");
		await run("touch", ["-d", "2020-01-01", join(src, "old.jsonl")]);
		await run("mkdir", ["-p", join(src, "new", "sub")]);
		await writeFile(join(src, "new", "sub", "agent.jsonl"), "nested-newer");
		await writeFile(join(src, "new", "artifact.txt"), "a");
		const tarball = join(await tmp(), "conv.tar.gz");
		expect((await run("tar", ["-czf", tarball, "-C", src, "."])).code).toBe(0);

		const out = await tmp();
		const jsonl = await extractConversation(tarball, out);
		expect(jsonl).toBe(join(out, "new.jsonl"));

		const sessionDir = join(await tmp(), "sessions", "bucket");
		const dest = await installConversation(jsonl, sessionDir);
		expect(dest).toBe(join(sessionDir, "new.jsonl"));
		expect(await readFile(dest, "utf8")).toBe("new");
		expect((await stat(join(sessionDir, "new", "artifact.txt"))).isFile()).toBe(true);
		expect(await readdir(sessionDir)).toEqual(["new", "new.jsonl"]);
	});

	test("a tarball without a session file is an error", async () => {
		const src = await tmp();
		await writeFile(join(src, "x.txt"), "x");
		const tarball = join(await tmp(), "c.tar");
		await run("tar", ["-cf", tarball, "-C", src, "."]);
		await expect(extractConversation(tarball, await tmp())).rejects.toThrow("holds no session file");
	});
});
