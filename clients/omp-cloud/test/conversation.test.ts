import { afterEach, describe, expect, test } from "bun:test";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { loadConversation, MAX_CONVERSATION_BYTES, rewriteConversation, uuidv7 } from "../src/conversation";
import { cleanup, tmp } from "./helpers";

afterEach(cleanup);

const OLD_ID = "0198aaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee";
const TS = "2026-10-18T22-11-00-000Z";
const HASH = "a".repeat(64);

/** A fixed-width (256 bytes with its newline) title slot line, without the newline. */
function titleSlot(): string {
	const mk = (pad: string) =>
		JSON.stringify({ type: "session_title", v: 1, title: "My task", updatedAt: "2026-10-18T22:11:00.000Z", pad });
	return mk(" ".repeat(255 - mk("").length));
}

function fixture(): { text: string; lines: string[] } {
	const lines = [
		titleSlot(),
		JSON.stringify({ type: "session", version: 3, id: OLD_ID, timestamp: "2026-10-18T22:11:00.000Z", cwd: "/work/app", title: "t" }),
		JSON.stringify({ type: "message", id: "e1", message: { role: "user", content: [{ type: "text", text: 'type":"session" in text, é ü 日本' }] } }),
		JSON.stringify({ type: "message", id: "e2", message: { role: "user", content: [{ type: "image", data: `blob:sha256:${HASH}` }] } }),
		"",
	];
	return { text: lines.join("\n"), lines };
}

describe("rewriteConversation", () => {
	test("changes only the session header: new id, parentSession set, everything else byte-identical", () => {
		const { text, lines } = fixture();
		const origPath = `/home/u/.omp/agent/sessions/--work-app--/${TS}_${OLD_ID}.jsonl`;
		const newId = uuidv7();
		const out = rewriteConversation(Buffer.from(text), origPath, newId);

		const got = out.data.toString("utf8").split("\n");
		expect(got).toHaveLength(lines.length);
		for (const i of [0, 2, 3, 4]) expect(got[i]).toBe(lines[i] as string);
		const header = JSON.parse(got[1] as string) as Record<string, unknown>;
		expect(header).toEqual({
			type: "session",
			version: 3,
			id: newId,
			timestamp: "2026-10-18T22:11:00.000Z",
			cwd: "/work/app",
			title: "t",
			parentSession: origPath,
		});
		expect(Object.keys(header).slice(0, 6)).toEqual(["type", "version", "id", "timestamp", "cwd", "title"]);
		// The title slot keeps its fixed 256-byte width.
		expect(Buffer.byteLength((got[0] as string) + "\n")).toBe(256);
		expect(out.filename).toBe(`${TS}_${newId}.jsonl`);
		expect(out.filename).toMatch(/^[A-Za-z0-9._-]+\.jsonl$/);
		expect(out.oldId).toBe(OLD_ID);
		expect(out.blobHashes).toEqual([HASH]);
	});

	test("replaces an existing parentSession and tolerates a file without a title slot or trailing newline", () => {
		const header = JSON.stringify({ type: "session", id: OLD_ID, parentSession: "/old/parent.jsonl" });
		const out = rewriteConversation(Buffer.from(`${header}\n{"type":"message"}`), `/s/${TS}_${OLD_ID}.jsonl`, "new-id");
		const [h, rest] = out.data.toString("utf8").split("\n");
		expect(JSON.parse(h as string)).toEqual({ type: "session", id: "new-id", parentSession: `/s/${TS}_${OLD_ID}.jsonl` });
		expect(rest).toBe('{"type":"message"}');
	});

	test("filename falls back to appending the id when the original name does not embed the old id", () => {
		const header = JSON.stringify({ type: "session", id: OLD_ID });
		expect(rewriteConversation(Buffer.from(header), "/s/custom.jsonl", "nid").filename).toBe("custom_nid.jsonl");
	});

	test("a file without a session header is an error", () => {
		expect(() => rewriteConversation(Buffer.from('{"type":"message"}\n'), "/s/x.jsonl")).toThrow("no session header");
	});
});

describe("uuidv7", () => {
	test("has version 7, the RFC 4122 variant and a millisecond timestamp prefix", () => {
		const now = 1_760_000_000_123;
		const id = uuidv7(now);
		expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
		expect(Number.parseInt(id.replace(/-/g, "").slice(0, 12), 16)).toBe(now);
		expect(uuidv7(now)).not.toBe(id);
	});
});

describe("loadConversation", () => {
	test("no file or a file missing on disk is no conversation", async () => {
		expect((await loadConversation(undefined)).kind).toBe("none");
		expect((await loadConversation(join(await tmp(), "gone.jsonl"))).kind).toBe("none");
	});

	test("reads and rewrites an existing session file", async () => {
		const dir = await tmp();
		const file = join(dir, `${TS}_${OLD_ID}.jsonl`);
		await writeFile(file, fixture().text);
		const r = await loadConversation(file);
		expect(r.kind).toBe("ok");
		if (r.kind === "ok") {
			expect(r.oldId).toBe(OLD_ID);
			expect(r.filename.endsWith(`${r.newId}.jsonl`)).toBe(true);
		}
	});

	test("a file over the 64 MiB cap is skipped", async () => {
		const dir = await tmp();
		const file = join(dir, "big.jsonl");
		await Bun.write(file, new Uint8Array(MAX_CONVERSATION_BYTES + 1));
		const r = await loadConversation(file);
		expect(r).toEqual({ kind: "too-large", bytes: MAX_CONVERSATION_BYTES + 1 });
	});
});
