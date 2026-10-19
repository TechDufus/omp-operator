import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { git, gitRaw, isCredentialPath, parseRemote, takeSnapshot, type SnapshotOptions } from "../src/snapshot";
import { cleanup, commitFile, configureRepo, remoteFixture, REMOTE_URL, tmp } from "./helpers";

afterEach(cleanup);

async function opts(cwd: string, over: Partial<SnapshotOptions> = {}): Promise<SnapshotOptions> {
	return {
		cwd,
		tmpDir: await tmp("omp-cloud-snap-"),
		gitHosts: [],
		forceBundle: false,
		maxBytes: 512 * 1024 * 1024,
		secretFiles: [],
		secretEnv: [],
		env: {},
		home: "/home/test",
		...over,
	};
}

describe("parseRemote", () => {
	const cases: [string, ReturnType<typeof parseRemote>][] = [
		["git@github.com:TechDufus/omp-operator.git", { scheme: "ssh", host: "github.com" }],
		["ssh://git@GitHub.com:22/o/r.git", { scheme: "ssh", host: "github.com" }],
		["https://user@git.example.test:8443/o/r.git", { scheme: "https", host: "git.example.test" }],
		["http://git.example.test/app.git", { scheme: "http", host: "git.example.test" }],
		["git://github.com/o/r.git", undefined],
		["/srv/git/app.git", undefined],
		["file:///srv/git/app.git", undefined],
		["C:\\repos\\app", undefined],
	];
	for (const [raw, want] of cases) {
		test(raw, () => expect(parseRemote(raw)).toEqual(want));
	}
});

describe("takeSnapshot", () => {
	test("delta mode: WIP commit holds edits and untracked files, never ignored ones, and the bundle applies to the remote", async () => {
		const f = await remoteFixture();
		await commitFile(f.work, "b.txt", "unpushed\n");
		await writeFile(join(f.work, "a.txt"), "one\nDIRTY\n");
		await writeFile(join(f.work, "c.txt"), "untracked\n");
		await writeFile(join(f.work, ".env"), "SECRET=hunter2hunter2\n");
		const head = await git(f.work, ["rev-parse", "HEAD"]);

		const snap = await takeSnapshot(
			await opts(f.work, {
				gitHosts: [],
				secretFiles: [".env"],
				secretEnv: ["MY_TOKEN"],
				env: { MY_TOKEN: "tok-123" },
			}),
		);

		expect(snap.mode).toBe("delta");
		expect(snap.remoteURL).toBe(REMOTE_URL);
		expect(snap.baseRef).toBe("main");
		expect(snap.baseCommit).toBe(f.base);
		expect(snap.localBranch).toBe("main");
		expect(snap.root).toBe(f.work);
		expect(snap.name).toBe("work");
		expect(snap.paths).toEqual({ home: "/home/test", workspace: f.work });
		expect(snap.author).toEqual({ name: "Test User", email: "test@example.com" });
		expect(snap.secretEnv).toEqual({ MY_TOKEN: "tok-123" });
		expect(snap.secretFiles).toEqual([{ path: ".env", content: Buffer.from("SECRET=hunter2hunter2\n").toString("base64") }]);

		// The user's HEAD, index and ref namespace are untouched.
		expect(await git(f.work, ["rev-parse", "HEAD"])).toBe(head);
		expect((await gitRaw(f.work, ["status", "--porcelain"])).stdout).toBe(" M a.txt\n?? c.txt\n");
		expect((await gitRaw(f.work, ["rev-parse", "--verify", "-q", "refs/omp-cloud/handoff"])).code).not.toBe(0);

		// A fresh clone of the remote has the base; the bundle must verify and apply there.
		const fresh = join(await tmp(), "fresh");
		await git(f.root, ["clone", "-q", f.bare, fresh]);
		expect(snap.bundlePath).toBeDefined();
		await git(fresh, ["bundle", "verify", snap.bundlePath as string]);
		await git(fresh, ["fetch", "-q", snap.bundlePath as string, "refs/omp-cloud/handoff"]);
		const tip = await git(fresh, ["rev-parse", "FETCH_HEAD"]);
		expect(tip).toBe(snap.handoffCommit);
		expect(await git(fresh, ["show", "FETCH_HEAD:a.txt"])).toBe("one\nDIRTY");
		expect(await git(fresh, ["show", "FETCH_HEAD:c.txt"])).toBe("untracked");
		expect(await git(fresh, ["show", "FETCH_HEAD:b.txt"])).toBe("unpushed");
		expect(await git(fresh, ["ls-tree", "--name-only", "FETCH_HEAD"])).not.toContain(".env");
		expect(await git(fresh, ["rev-parse", "FETCH_HEAD^"])).toBe(head);
		expect(await git(fresh, ["log", "-1", "--format=%an <%ae> | %s", "FETCH_HEAD"])).toBe(
			"Test User <test@example.com> | omp-cloud: local changes at handoff",
		);
	});

	test("a clean, fully pushed tree is a delta with nothing to upload", async () => {
		const f = await remoteFixture();
		const snap = await takeSnapshot(await opts(f.work));
		expect(snap.mode).toBe("delta");
		expect(snap.handoffCommit).toBe(f.base);
		expect(snap.baseCommit).toBe(f.base);
		expect(snap.bundlePath).toBeUndefined();
	});

	test("no remote: full mode with the whole history in the bundle", async () => {
		const dir = await tmp();
		await git(dir, ["init", "-q", "-b", "main"]);
		await configureRepo(dir);
		await commitFile(dir, "a.txt", "one\n");
		await commitFile(dir, "b.txt", "two\n");
		const snap = await takeSnapshot(await opts(dir));
		expect(snap.mode).toBe("full");
		expect(snap.baseCommit).toBe("");
		expect(snap.remoteURL).toBe("");
		expect(snap.baseRef).toBe("");
		expect(snap.bundlePath).toBeDefined();
		const empty = await tmp();
		await git(empty, ["init", "-q"]);
		await git(empty, ["bundle", "verify", snap.bundlePath as string]);
		await git(empty, ["fetch", "-q", snap.bundlePath as string, "refs/omp-cloud/handoff"]);
		expect(await git(empty, ["rev-list", "--count", "FETCH_HEAD"])).toBe("2");
	});

	test("remote present + forceBundle: full mode (no base commit, full-history bundle) but remoteURL and baseRef are still sent", async () => {
		const f = await remoteFixture();
		const snap = await takeSnapshot(await opts(f.work, { forceBundle: true }));
		expect(snap.mode).toBe("full");
		expect(snap.baseCommit).toBe("");
		expect(snap.remoteURL).toBe(REMOTE_URL);
		expect(snap.baseRef).toBe("main");
		expect(snap.bundlePath).toBeDefined();
		const empty = await tmp();
		await git(empty, ["init", "-q"]);
		await git(empty, ["bundle", "verify", snap.bundlePath as string]);
	});

	test("an ssh remote is a delta only when its host is a configured git host; the URL is sent either way", async () => {
		const f = await remoteFixture();
		const url = "git@github.example.test:o/r.git";
		await git(f.work, ["remote", "set-url", "origin", url]);
		const notListed = await takeSnapshot(await opts(f.work));
		expect(notListed).toMatchObject({ mode: "full", baseCommit: "", remoteURL: url, baseRef: "main" });
		const listed = await takeSnapshot(await opts(f.work, { gitHosts: ["GitHub.Example.Test"] }));
		expect(listed).toMatchObject({ mode: "delta", baseCommit: f.base, remoteURL: url, baseRef: "main" });
	});

	test("falls back to origin/HEAD when the branch has no upstream", async () => {
		const f = await remoteFixture();
		await git(f.work, ["remote", "set-head", "origin", "main"]);
		await git(f.work, ["switch", "-q", "-c", "feature"]);
		await commitFile(f.work, "f.txt", "f\n");
		const snap = await takeSnapshot(await opts(f.work));
		expect(snap.mode).toBe("delta");
		expect(snap.baseRef).toBe("main");
		expect(snap.baseCommit).toBe(f.base);
		expect(snap.localBranch).toBe("feature");
	});

	test("a secret file that is not gitignored is refused", async () => {
		const f = await remoteFixture();
		await writeFile(join(f.work, "creds.txt"), "x=1\n");
		await expect(takeSnapshot(await opts(f.work, { secretFiles: ["creds.txt"] }))).rejects.toThrow(
			"creds.txt is not gitignored; refusing to ship it",
		);
	});

	const secretCases: [string, string[], string][] = [
		["missing file", ["nope.env"], "nope.env does not exist"],
		["escaping path", ["../x"], "is not a safe workspace-relative path"],
		["absolute path", ["/etc/passwd"], "is not a safe workspace-relative path"],
		["inside .git", [".git/config"], "is not a safe workspace-relative path"],
	];
	for (const [name, files, want] of secretCases) {
		test(`secret file: ${name}`, async () => {
			const f = await remoteFixture();
			await expect(takeSnapshot(await opts(f.work, { secretFiles: files }))).rejects.toThrow(want);
		});
	}

	test("a secret file over 256 KiB is refused", async () => {
		const f = await remoteFixture();
		await writeFile(join(f.work, ".env"), "x".repeat(256 * 1024 + 1));
		await expect(takeSnapshot(await opts(f.work, { secretFiles: [".env"] }))).rejects.toThrow("is over 256 KiB");
	});

	test("a missing secret env var is an error", async () => {
		const f = await remoteFixture();
		await expect(takeSnapshot(await opts(f.work, { secretEnv: ["NOT_SET"] }))).rejects.toThrow(
			"NOT_SET is not set in this omp's environment",
		);
	});

	test("a repository with no commits is refused", async () => {
		const dir = await tmp();
		await git(dir, ["init", "-q", "-b", "main"]);
		await expect(takeSnapshot(await opts(dir))).rejects.toThrow("the repository has no commits");
	});

	test("a directory outside any repository is refused", async () => {
		const dir = await tmp();
		await mkdir(join(dir, "sub"));
		await expect(takeSnapshot(await opts(join(dir, "sub")))).rejects.toThrow("not a git repository");
	});

	test("an oversized bundle reports its size and the limit", async () => {
		const dir = await tmp();
		await git(dir, ["init", "-q", "-b", "main"]);
		await configureRepo(dir);
		await commitFile(dir, "a.txt", "one\n");
		await expect(takeSnapshot(await opts(dir, { maxBytes: 10 }))).rejects.toThrow(
			/^repository bundle is [\d.]+ MB, over the [\d.]+ MB upload limit$/,
		);
	});
});

describe("credential-named files", () => {
	const cases: [string, boolean][] = [
		[".env", true],
		[".env.local", true],
		[".env.production", true],
		[".env.example", false],
		[".env.sample", false],
		[".env.template", false],
		["infra/prod.tfvars", true],
		["infra/prod.tfvars.json", true],
		["terraform.tfstate", true],
		["terraform.tfstate.backup", true],
		["certs/server.pem", true],
		["certs/server.key", true],
		["a.p12", true],
		["a.pfx", true],
		["a.jks", true],
		["a.keystore", true],
		["home/.ssh/id_rsa", true],
		["id_dsa", true],
		["id_ecdsa", true],
		["id_ed25519", true],
		["id_ed25519.pub", false],
		[".netrc", true],
		[".npmrc", true],
		[".pypirc", true],
		["kubeconfig", true],
		["clusters/dev.kubeconfig", true],
		["talosconfig", true],
		["environment.ts", false],
		["README.md", false],
		["src/keyboard.ts", false],
	];
	for (const [path, want] of cases) {
		test(`${path} -> ${want ? "withheld" : "shipped"}`, () => expect(isCredentialPath(path)).toBe(want));
	}

	test("untracked ones are skipped, tracked ones keep their HEAD content, the rest ships; all are reported", async () => {
		const f = await remoteFixture();
		await commitFile(f.work, ".npmrc", "registry=https://registry.example.test\n");
		await git(f.work, ["push", "-q", "origin", "main"]);
		const base = await git(f.work, ["rev-parse", "HEAD"]);
		await writeFile(join(f.work, ".npmrc"), "//registry.example.test/:_authToken=npm_secret_value\n");
		await writeFile(join(f.work, ".env.local"), "TOKEN=local-secret\n");
		await mkdir(join(f.work, "certs"), { recursive: true });
		await writeFile(join(f.work, "certs", "server.pem"), "-----BEGIN PRIVATE KEY-----\n");
		await writeFile(join(f.work, ".env.example"), "TOKEN=\n");
		await writeFile(join(f.work, "notes.txt"), "shipped\n");

		const snap = await takeSnapshot(await opts(f.work));
		expect(snap.baseCommit).toBe(base);
		expect(snap.withheld).toEqual([".env.local", ".npmrc", "certs/server.pem"]);
		expect(snap.untracked).toEqual([".env.example", "notes.txt"]);
		expect(snap.untrackedTotal).toBe(2);
		expect(snap.wip).toEqual({ files: 2, insertions: 2, deletions: 0 });
		expect(snap.commits).toBe(0);

		const tree = await git(f.work, ["ls-tree", "-r", "--name-only", snap.handoffCommit]);
		expect(tree.split("\n")).toEqual([".env.example", ".gitignore", ".npmrc", "a.txt", "notes.txt"]);
		expect(await git(f.work, ["show", `${snap.handoffCommit}:.npmrc`])).toBe("registry=https://registry.example.test");
		// Nothing was removed from the user's working tree.
		expect((await gitRaw(f.work, ["status", "--porcelain", "--untracked-files=all"])).stdout).toContain("?? .env.local");
	});

	test("a gitignored credential file listed in secretFiles ships as a secret and is not reported as withheld; an unlisted untracked one is", async () => {
		const f = await remoteFixture();
		await writeFile(join(f.work, ".gitignore"), ".env\n.env.local\n");
		await writeFile(join(f.work, ".env.local"), "TOKEN=x\n");
		await writeFile(join(f.work, ".env.production"), "TOKEN=y\n");
		const snap = await takeSnapshot(await opts(f.work, { secretFiles: [".env.local"] }));
		expect(snap.secretFiles.map(s => s.path)).toEqual([".env.local"]);
		expect(snap.withheld).toEqual([".env.production"]);
		expect(await git(f.work, ["ls-tree", "-r", "--name-only", snap.handoffCommit])).not.toContain(".env.");
	});

	test("the untracked listing is capped at 10 names but counts them all", async () => {
		const f = await remoteFixture();
		for (let i = 0; i < 13; i++) await writeFile(join(f.work, `u${String(i).padStart(2, "0")}.txt`), "x\n");
		const snap = await takeSnapshot(await opts(f.work));
		expect(snap.untracked).toHaveLength(10);
		expect(snap.untrackedTotal).toBe(13);
		expect(snap.untracked[0]).toBe("u00.txt");
		expect(snap.wip.files).toBe(13);
	});
});
