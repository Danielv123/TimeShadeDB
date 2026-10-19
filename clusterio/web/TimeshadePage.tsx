import React, { useContext, useEffect, useState } from "react";
import { Alert, Button, Space, Spin } from "antd";
import { ControlContext, PageHeader, PageLayout } from "@clusterio/web_ui";
import { ViewTokenRequest } from "../messages";
import { refreshDelay, viewerUrl } from "./viewer_url";

type State = { url: string; running: boolean } | { error: string } | undefined;

/**
 * The map page: the TimeShadeDB viewer, served by the sidecar through the controller's proxy, in a frame. The frame
 * gets a view token (it cannot send the web UI's own token), fetched over the control link and renewed before it
 * expires; renewing reloads the frame.
 */
export default function TimeshadePage() {
	const control = useContext(ControlContext);
	const [state, setState] = useState<State>();

	useEffect(() => {
		let timer: ReturnType<typeof setTimeout> | undefined;
		let stopped = false;
		const fetchToken = async () => {
			try {
				const res = await control.send(new ViewTokenRequest());
				if (stopped) {
					return;
				}
				setState({ url: viewerUrl(new URL(webRoot, document.location.href).href, res.token), running: res.running });
				timer = setTimeout(fetchToken, refreshDelay(res.expiresMs, Date.now()));
			} catch (err: any) {
				if (!stopped) {
					setState({ error: err?.message ?? String(err) });
					timer = setTimeout(fetchToken, 30_000);
				}
			}
		};
		fetchToken();
		return () => {
			stopped = true;
			clearTimeout(timer);
		};
	}, [control]);

	const extra = state && "url" in state
		? <Button href={state.url} target="_blank" rel="noopener noreferrer">Open in a new tab</Button>
		: undefined;
	return <PageLayout nav={[{ name: "Map" }]}>
		<PageHeader title="Map" extra={extra} />
		{state === undefined && <Spin />}
		{state && "error" in state && <Alert type="error" showIcon message="The map is not available" description={state.error} />}
		{state && "url" in state && <Space direction="vertical" style={{ width: "100%" }}>
			{!state.running && <Alert type="warning" showIcon message="The map server is starting or restarting; the map may not load yet." />}
			<iframe
				title="TimeShadeDB map"
				src={state.url}
				style={{ width: "100%", height: "calc(100vh - 180px)", minHeight: 400, border: 0 }}
				allow="fullscreen"
			/>
		</Space>}
	</PageLayout>;
}
