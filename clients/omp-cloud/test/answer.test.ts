import { describe, expect, test } from "bun:test";
import { answerThread, askUser, OTHER, parseAnswerText, pendingQuestions } from "../src/answer";
import type { AnswerBody, Question, ThreadDetail, ThreadTurn } from "../src/types";
import { fakeHost } from "./fakes";

const q = (id: number, over: Partial<Question> = {}): Question => ({
	id,
	ask: 1,
	key: `k${id}`,
	text: `question ${id}`,
	askedAt: "2026-10-19T10:00:00Z",
	options: [{ label: "postgres", description: "prod" }, { label: "sqlite" }, { label: "mysql" }],
	multi: false,
	recommended: 0,
	...over,
});

const turn = (id: number, questions: Question[]): ThreadTurn => ({ id, state: "running", text: "t", by: "x", queuedAt: "", questions });

describe("pendingQuestions", () => {
	test("is the unanswered questions of the oldest pending ask, in order", () => {
		const d = {
			turns: [
				{ ...turn(1, [q(1, { answeredAt: "x" })]), state: "done" },
				turn(2, [q(3, { ask: 2 }), q(2, { ask: 1 }), q(1, { ask: 1, answeredAt: "x" }), q(4, { ask: 1 })]),
			],
		} as Pick<ThreadDetail, "turns">;
		const p = pendingQuestions(d);
		expect(p?.turn).toBe(2);
		expect(p?.questions.map(x => x.id)).toEqual([2, 4]);
	});

	test("nothing pending, or questions null from an operator without them", () => {
		expect(pendingQuestions({ turns: [turn(1, [q(1, { answeredAt: "x" })])] })).toBeUndefined();
		expect(pendingQuestions({ turns: [{ id: 1, state: "done", text: "", by: "", queuedAt: "", questions: null }] })).toBeUndefined();
	});
});

describe("parseAnswerText", () => {
	const cases: [string, Partial<Question>, AnswerBody][] = [
		["postgres", {}, { selected: ["postgres"] }],
		["  SQLite ", {}, { selected: ["sqlite"] }],
		["the second one", {}, { answer: "the second one" }],
		["postgres, sqlite", {}, { answer: "postgres, sqlite" }],
		["postgres, sqlite", { multi: true }, { selected: ["postgres", "sqlite"] }],
		["postgres, oracle", { multi: true }, { answer: "postgres, oracle" }],
		["postgres, postgres", { multi: true }, { selected: ["postgres"] }],
		["yes", { options: null }, { answer: "yes" }],
	];
	for (const [text, over, want] of cases) {
		test(`${JSON.stringify(text)} ${over.multi ? "(multi) " : ""}-> ${JSON.stringify(want)}`, () => expect(parseAnswerText(q(1, over), text)).toEqual(want));
	}
});

describe("askUser", () => {
	test("single choice: a labelled option, the recommendation marked, 'Other' types free text", async () => {
		const seen: string[][] = [];
		const h = fakeHost({
			select: async (_t, items) => {
				seen.push(items);
				return items[1];
			},
		});
		expect(await askUser(h.host.ui, "t", q(1))).toEqual({ selected: ["sqlite"] });
		expect(seen[0]).toEqual(["postgres (recommended) — prod", "sqlite", "mysql", OTHER]);

		const other = fakeHost({ select: async (_t, items) => items.at(-1), input: async () => " my own " });
		expect(await askUser(other.host.ui, "t", q(1))).toEqual({ answer: "my own" });
	});

	test("cancelling at any step answers nothing", async () => {
		const h = fakeHost({ select: async () => undefined, input: async () => undefined });
		expect(await askUser(h.host.ui, "t", q(1))).toBeUndefined();
		const typed = fakeHost({ select: async () => OTHER, input: async () => "   " });
		expect(await askUser(typed.host.ui, "t", q(1))).toBeUndefined();
	});

	test("without options the user types the answer", async () => {
		const h = fakeHost({ select: async () => undefined, input: async () => "use utf-8" });
		expect(await askUser(h.host.ui, "t", q(1, { options: [] }))).toEqual({ answer: "use utf-8" });
	});

	test("multi select repeats until Done; chosen options leave the list", async () => {
		const picks = ["postgres (recommended) — prod", "mysql", "Done (2 selected)"];
		const lists: string[][] = [];
		const h = fakeHost({
			select: async (_t, items) => {
				lists.push(items);
				const want = picks.shift();
				return items.find(i => i === want);
			},
		});
		expect(await askUser(h.host.ui, "t", q(1, { multi: true }))).toEqual({ selected: ["postgres", "mysql"] });
		expect(lists[0]).toEqual(["postgres (recommended) — prod", "sqlite", "mysql", OTHER]);
		expect(lists[1]).toEqual(["sqlite", "mysql", "Done (1 selected)", OTHER]);
		expect(lists[2]).toEqual(["sqlite", "Done (2 selected)", OTHER]);
	});

	test("a host that cannot prompt says how to answer instead", async () => {
		const h = fakeHost();
		await expect(askUser(h.host.ui, "t", q(1))).rejects.toThrow("/cloud answer <id> <text>");
	});
});

describe("answerThread", () => {
	function client(detail: ThreadDetail) {
		const posted: { turn: number; id: number; body: AnswerBody }[] = [];
		return {
			posted,
			thread: async () => detail,
			answer: async (_ns: string, _t: string, turnId: number, id: number, body: AnswerBody) => {
				posted.push({ turn: turnId, id, body });
				return q(id, { selected: body.selected, answer: body.answer, answeredAt: "now" });
			},
		};
	}
	const detail = (questions: Question[]): ThreadDetail => ({ turns: [turn(5, questions)] }) as ThreadDetail;

	test("with text it answers the first pending question only", async () => {
		const c = client(detail([q(1), q(2)]));
		const h = fakeHost();
		const out = await answerThread(c, "ns", "coder-k3x9fa", h.host.ui, "postgres");
		expect(c.posted).toEqual([{ turn: 5, id: 1, body: { selected: ["postgres"] } }]);
		expect(out.left).toBe(1);
	});

	test("without text it prompts for every question of the ask in order and stops at a cancel", async () => {
		const c = client(detail([q(1), q(2), q(3)]));
		const answers = ["postgres", "sqlite", undefined];
		const h = fakeHost({
			select: async (_t, items) => {
				const a = answers.shift();
				return a === undefined ? undefined : items.find(i => i.startsWith(a));
			},
		});
		const out = await answerThread(c, "ns", "coder-k3x9fa", h.host.ui);
		expect(c.posted.map(p => [p.id, p.body])).toEqual([
			[1, { selected: ["postgres"] }],
			[2, { selected: ["sqlite"] }],
		]);
		expect(out).toMatchObject({ left: 1 });
		expect(out.answered).toHaveLength(2);
	});

	test("a thread with nothing pending is a clear error", async () => {
		const c = client(detail([q(1, { answeredAt: "x" })]));
		await expect(answerThread(c, "ns", "coder-k3x9fa", fakeHost().host.ui, "hi")).rejects.toThrow("☁ coder-k3x9fa has no question waiting");
	});
});
