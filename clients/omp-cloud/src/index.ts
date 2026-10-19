import { fileURLToPath } from "node:url";
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { cardRenderer, CARD_TYPE, loadTui, type TuiModule } from "./cards";
import { registerCloudTool } from "./tool";
import { Cloud } from "./verbs";

/** This extension's own directory (the package root), which is never shipped to the cloud. */
const OWN_ROOT = fileURLToPath(new URL("..", import.meta.url));

export default async function ompCloud(pi: ExtensionAPI): Promise<void> {
	const cloud = new Cloud(pi, OWN_ROOT);
	let tui: TuiModule | undefined;

	// Everything registers before the first await: omp reads registrations during load.
	pi.registerMessageRenderer(CARD_TYPE, cardRenderer(() => tui));

	// `/cloud` is the only user entry point; there is deliberately no input-prefix trigger.
	pi.registerCommand("cloud", {
		description: "Hand work to a cloud agent and manage cloud threads (no arguments: the hub)",
		getArgumentCompletions: prefix => cloud.complete(prefix),
		handler: (args, ctx) => cloud.handle(args, ctx),
	});

	registerCloudTool(pi, cloud);

	// switchSession replaces the session's context (and clears its timers); re-arm the watcher.
	pi.on("session_switch", (_event, ctx) => {
		cloud.rebind(ctx);
	});

	// Cards held back while the agent was busy appear once it has ended.
	pi.on("agent_end", (_event, ctx) => {
		ctx.setTimeout(() => cloud.flushCards(ctx), 100);
	});

	// pi-tui is provided by the host. Without it the cards render as plain text and the hub
	// falls back to an inventory card.
	tui = await loadTui();
	cloud.setTui(tui);
}
