import { Type, type Static } from "@sinclair/typebox";

/**
 * One capture envelope as the instance forwards it (docs/PROTOCOL.md 1): Lua's fields plus `map`, `recv_ms` and
 * `seq`. The controller sets `src` and `instance` from the link address, never from the payload. The messages are
 * checked by the sidecar's ingest, not here.
 */
export const Envelope = Type.Object({
	v: Type.Integer(),
	tick: Type.Integer(),
	speed: Type.Optional(Type.Number()),
	msgs: Type.Array(Type.Unknown()),
	map: Type.String({ minLength: 1, maxLength: 200 }),
	recv_ms: Type.Integer(),
	seq: Type.Integer(),
	/** Nothing older of this instance waits to be sent (docs/PROTOCOL.md 1); `ended`: the instance stopped. */
	caught_up: Type.Optional(Type.Boolean()),
	ended: Type.Optional(Type.Boolean()),
});
export type Envelope = Static<typeof Envelope>;

/**
 * Instance to controller, about once a second: the envelopes Lua printed since the last one, split per map. A request,
 * so the instance knows they were delivered: Clusterio drops an event it cannot forward (the controller is away) and
 * reports nothing to the sender, while a request fails or gets its response. A response means the controller queued
 * them (ingest.ts).
 */
export class EnvelopesRequest {
	declare ["constructor"]: typeof EnvelopesRequest;
	static type = "request" as const;
	static src = "instance" as const;
	static dst = "controller" as const;
	static plugin = "timeshade" as const;

	constructor(
		public envelopes: Envelope[],
	) { }

	static jsonSchema = Type.Object({
		envelopes: Type.Array(Envelope),
	});

	static fromJSON(json: Static<typeof EnvelopesRequest.jsonSchema>) {
		return new this(json.envelopes);
	}
}

/**
 * The same as an event: what waits when the instance stops, whose link then routes no responses back (instance.ts).
 */
export class EnvelopesEvent {
	declare ["constructor"]: typeof EnvelopesEvent;
	static type = "event" as const;
	static src = "instance" as const;
	static dst = "controller" as const;
	static plugin = "timeshade" as const;

	constructor(
		public envelopes: Envelope[],
	) { }

	static jsonSchema = EnvelopesRequest.jsonSchema;

	static fromJSON(json: Static<typeof EnvelopesRequest.jsonSchema>) {
		return new this(json.envelopes);
	}
}

export class ViewTokenResponse {
	constructor(
		/** A bearer token for the map proxy (audience "timeshade.view"). */
		public token: string,
		/** When the token expires, Unix ms. */
		public expiresMs: number,
		/** Whether the map server (the sidecar) is running right now. */
		public running: boolean,
	) { }

	static jsonSchema = Type.Object({
		token: Type.String(),
		expiresMs: Type.Number(),
		running: Type.Boolean(),
	});

	static fromJSON(json: Static<typeof ViewTokenResponse.jsonSchema>) {
		return new this(json.token, json.expiresMs, json.running);
	}
}

/**
 * Web UI to controller: a short-lived token for the map viewer, which runs in an iframe and passes it as ?token= (it
 * cannot send the web UI's session token, and should not carry it in a URL).
 */
export class ViewTokenRequest {
	declare ["constructor"]: typeof ViewTokenRequest;
	static type = "request" as const;
	static src = "control" as const;
	static dst = "controller" as const;
	static plugin = "timeshade" as const;
	static permission = "timeshade.view" as const;
	static Response = ViewTokenResponse;
}
