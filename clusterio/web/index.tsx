import React from "react";
import type { WebPluginContext } from "@clusterio/web_ui";
import TimeshadePage from "./TimeshadePage.js";

/** The web entry point: the map page at /timeshade, for users with timeshade.view. */
export default async function loadWebPlugin(context: WebPluginContext) {
	const { control, plugin } = context;
	control.hooks.pages.attach(plugin.name, () => [
		{
			path: "/timeshade",
			sidebarName: "Map",
			// Client side only: the proxy and the token request check timeshade.view themselves.
			permission: "timeshade.view",
			content: <TimeshadePage />,
		},
	]);
}
