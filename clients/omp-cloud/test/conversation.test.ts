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

describe("upload stripping", () => {
	const header = JSON.stringify({ type: "session", version: 3, id: OLD_ID, cwd: "/work/app" });
	const msg = (id: string, parentId: string | null, role: string, content: unknown = "x") =>
		JSON.stringify({ type: "message", id, parentId, message: { role, content } });
	const card = (id: string, parentId: string | null, customType = "omp-cloud") =>
		JSON.stringify({ type: "custom_message", id, parentId, customType, content: "[cloud] x", display: true, details: { kind: "turn" } });
	const build = (...lines: string[]) => Buffer.from(`${[header, ...lines].join("\n")}\n`);
	const parse = (b: Buffer) =>
		b
			.toString("utf8")
			.trimEnd()
			.split("\n")
			.map(l => JSON.parse(l) as { id?: string; parentId?: string | null; type: string; customType?: string });

	test("drops cards and re-links their children to the nearest surviving ancestor", () => {
		const keep = msg("e1", null, "user");
		const other = card("k1", "e1", "something-else");
		const out = rewriteConversation(
			build(keep, card("c1", "e1"), card("c2", "c1"), msg("e2", "c2", "assistant"), card("c3", "e2"), msg("e3", "c3", "user"), other),
			`/s/${TS}_${OLD_ID}.jsonl`,
			"nid",
		);
		const entries = parse(out.data);
		expect(entries.map(e => e.id)).toEqual(["nid", "e1", "e2", "e3", "k1"]);
		expect(Object.fromEntries(entries.filter(e => e.id).map(e => [e.id, e.parentId]))).toEqual({ e1: null, e2: "e1", e3: "e2", k1: "e1" });
		expect(out.droppedCards).toBe(3);
		// Lines that need no change stay byte-identical.
		const lines = out.data.toString("utf8").split("\n");
		expect(lines).toContain(keep);
		expect(lines).toContain(other);
	});

	test("text that merely mentions the card type is not a card", () => {
		const chatter = msg("e1", null, "user", 'a custom_message with customType "omp-cloud"');
		const out = rewriteConversation(build(chatter), "/s/x.jsonl", "nid");
		expect(out.droppedCards).toBe(0);
		expect(out.data.toString("utf8").split("\n")).toContain(chatter);
	});

	test("a conversation without cards is unchanged apart from the header", () => {
		const body = [msg("e1", null, "user"), msg("e2", "e1", "assistant")];
		const out = rewriteConversation(build(...body), "/s/x.jsonl", "nid");
		expect(out.data.toString("utf8").split("\n").slice(1)).toEqual([...body, ""]);
	});

	test("a delegation cuts the assistant message with the pending tool call and everything after it", () => {
		const call = (id: string) => msg("a1", "e1", "assistant", [{ type: "text", text: "delegating" }, { type: "toolCall", id, name: "cloud", arguments: { op: "delegate" } }]);
		const earlier = msg("a0", null, "assistant", [{ type: "toolCall", id: "call_0", name: "read", arguments: {} }]);
		const lines = [earlier, msg("e1", "a0", "user"), call("call_42"), msg("r1", "a1", "toolResult"), card("c1", "r1"), msg("e9", "c1", "user")];
		const out = rewriteConversation(build(...lines), "/s/x.jsonl", "nid", { dropFromToolCall: "call_42" });
		expect(parse(out.data).map(e => e.id)).toEqual(["nid", "a0", "e1"]);
		expect(out.droppedForToolCall).toBe(4);

		// The call's id must name a tool call of an assistant message: user text that repeats it is not cut.
		const mention = msg("e1", null, "user", "call_42 is the id");
		const kept = rewriteConversation(build(mention, msg("e2", "e1", "assistant")), "/s/x.jsonl", "nid", { dropFromToolCall: "call_42" });
		expect(kept.droppedForToolCall).toBe(0);
		expect(parse(kept.data).map(e => e.id)).toEqual(["nid", "e1", "e2"]);
	});

	test("blob references of cut content are not shipped", () => {
		const withBlob = msg("a1", "e1", "assistant", [{ type: "toolCall", id: "call_7", name: "cloud", arguments: { img: `blob:sha256:${HASH}` } }]);
		const out = rewriteConversation(build(msg("e1", null, "user"), withBlob), "/s/x.jsonl", "nid", { dropFromToolCall: "call_7" });
		expect(out.blobHashes).toEqual([]);
	});
});
