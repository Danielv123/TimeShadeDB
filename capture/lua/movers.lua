-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap module/movers.lua (Blue Oak Model License 1.0.0, see capture/README.md).
--
-- Movers (docs/v3/PROTOCOL.md sections 3.1 and 6): a scan of what moves, a walk polling core.WALK movers a tick, a wheel
-- of due train reads, journeys minted by the scan and followed through joins, splits and Universal Edges crossings.
--
-- Trains are read every core.STEP tiles and sent by the chord, heading and heartbeat rules (core.train_read), always with
-- the game tick of the read, with every carriage's position and orientation (`car`) on first sight, stop, start, end
-- switch and resync, and `es` when the leading end switched. Vehicles and players are sent by dead reckoning
-- (core.poll); vehicles carry their orientation.
--
-- Journeys, registrations, the wheel and each mover's last sample live in storage and change only in events and
-- on_tick steps that run identically on every peer. The resync only reads: it may run on one peer alone (the standalone
-- mod resyncs on each load).
local name = ...
local P = type(name) == "string" and name:match("^(.*[./])[^./]*$") or ""
local core = require(P .. "core")
local outbox = require(P .. "outbox")

local movers = {}

--- The id of this game in journey ids: the Clusterio instance id, 0 standalone. The host sets it.
movers.instance = function()
	return 0
end

--- A train's or vehicle's registration for on_object_destroyed: its number only.
local function register(ref)
	return (script.register_on_object_destroyed(ref))
end

local vehicle_names
--- Every car and spider-vehicle prototype name (get_vehicles without a type returns rolling stock too). A file-local
--- cache of a pure function of the prototypes, the same on every peer.
local function vehicles()
	if not vehicle_names then
		vehicle_names = {}
		for vname in pairs(prototypes.get_entity_filtered{ { filter = "type", type = { "car", "spider-vehicle" } } }) do
			vehicle_names[#vehicle_names + 1] = vname
		end
	end
	return vehicle_names
end

--- Rolling stock as the viewer draws it; any other type is a wagon ("C").
local KINDS = { locomotive = "L", ["cargo-wagon"] = "C", ["fluid-wagon"] = "F", ["artillery-wagon"] = "A" }

--- A colour as "rrggbb": the entity's own, else its prototype's; nil without either.
local function hex(e)
	local c = e.color or e.prototype.color
	if not c then
		return nil
	end
	return string.format("%02x%02x%02x", math.floor(c.r * 255 + 0.5), math.floor(c.g * 255 + 0.5),
		math.floor(c.b * 255 + 0.5))
end

--- Train tr's carriage kinds from its front (upper case when the carriage faces the front), the colours of its first
--- locomotive from the front and from the back, and what each end's carriage is (name, facing) and the force, into
--- mover m. Read again whenever the train changes (core.claim clears m.o), so a read needs no carriage list.
local function shape(m, tr)
	local kinds, first, last = {}, nil, nil
	local cars = tr.carriages
	for i, c in ipairs(cars) do
		if i <= core.KINDS_MAX then
			local k = KINDS[c.type] or "C"
			kinds[i] = c.is_headed_to_trains_front and k or k:lower()
		end
		if c.type == "locomotive" then
			first, last = first or c, c
		end
	end
	m.o, m.cf, m.cb = table.concat(kinds), first and hex(first), last and hex(last)
	local f, b = cars[1], cars[#cars]
	m.ef, m.eb = f, b
	-- the carriage behind each end, for the heading (nil for a single carriage)
	m.sf, m.sb = cars[2], #cars > 1 and cars[#cars - 1] or nil
	m.nf, m.nb, m.hf, m.hb = f.name, b.name, f.is_headed_to_trains_front, b.is_headed_to_trains_front
	m.force, m.n = f.force.name, #cars
end

--- Every carriage from the leading end as { x, y, orientation }, at most core.CAR_MAX.
local function car_list(tr, fwd)
	local cars = tr.carriages
	local n = #cars
	local out = {}
	for i = 1, math.min(n, core.CAR_MAX) do
		local c = cars[fwd and i or n + 1 - i]
		local p = c.position
		out[i] = { core.round(p.x, 2), core.round(p.y, 2), core.round(c.orientation, 3) % 1 }
	end
	return out
end

--- A new journey for train or vehicle `key`, registered.
local function mint(st, ref, kind, key, t)
	local j = core.mint(movers.instance(), t, key, kind)
	core.claim(st, j, kind, key, ref, kind == "t" and #ref.carriages or nil, register)
	return j
end

--- Journey j's gone, if it was ever sampled and the capture is configured, then forget it. Unconfigured (before a
--- Clusterio boot's config, or with the standalone mod turned off) only the storage side runs: a gone queued then would
--- be flushed much later under a wrong envelope tick.
function movers.gone(st, j)
	st.fast[j] = nil
	local m = st.movers[j]
	if not m then
		return
	end
	if m.s and st.configured then
		outbox.move(m.s, { j = j, gone = true })
	end
	core.forget(st, j)
end

--- What a train looks like now: its leading carriage (front_stock while running forwards), position, surface and
--- heading: from the carriage behind it towards it (PROTOCOL.md 3.1, as in illisis); a single carriage's own
--- orientation in the direction of travel. m must be shaped.
local function train_view(m, r, fwd)
	local e = fwd and m.ef or m.eb
	local pos = e.position
	local second = fwd and m.sf or m.sb
	local h
	if second then
		local sp = second.position
		h = core.heading(pos.x, pos.y, sp.x, sp.y)
	else
		local headed = fwd and m.hf or m.hb
		h = core.heading(pos.x, pos.y, nil, nil, e.orientation, headed ~= fwd)
	end
	return e, pos, e.surface_index, h
end

--- The fields every sample of mover m carries. Trains take the leading carriage's name and the force from the shape;
--- vehicles and players read them from src.
local function sample(j, m, src, x, y, h, fwd)
	local o, c = m.o, m.cf
	if m.kind == "t" then
		local name = m.nf
		if not fwd then
			o, c, name = o and core.flip_kinds(o), m.cb, m.nb
		end
		return { j = j, k = "t", f = m.force, name = name, x = x, y = y, h = h, n = m.n, o = o, c = c }
	end
	return { j = j, k = m.kind, f = src.force.name, name = src.name, x = x, y = y, h = h, n = m.n, o = o, c = c }
end

--- The name of surface index si for mover m, read only when it changes.
local function surface_name(m, si)
	if m.ssi ~= si or not m.s then
		m.s, m.ssi = game.get_surface(si).name, si
	end
	return m.s
end

--- One poll of journey j. A train reads its leading end, a vehicle its position, a player its physical controller. A
--- handle gone invalid without its event ends the journey. how: "stop" (left the moving set). Returns the samples sent.
local function poll(st, j, t, how)
	local m = st.movers[j]
	if not m then
		return 0
	end
	local r = m.ref
	-- a loaded save can hold a player who is no longer connected, and no on_player_left_game is raised for them
	if not (r and r.valid) or (m.kind == "p" and not r.connected) then
		movers.gone(st, j)
		return 0
	end
	if m.kind == "t" then
		local speed = r.speed
		local fwd = core.forward(m.fwd, speed)
		if not (m.o and m.ef and m.ef.valid and m.eb and m.eb.valid and (m.sf == nil or m.sf.valid)) then
			shape(m, r)
		end
		-- a moving train is read again when it has gone core.STEP tiles; at rest it leaves the wheel
		if speed ~= 0 then
			local due = t + core.next_read(speed)
			st.fast[j] = due
			local slot = due % core.WHEEL
			local list = st.wheel[slot] or {}
			st.wheel[slot] = list
			list[#list + 1] = j
		elseif st.fast[j] then
			st.fast[j] = nil
		end
		local e, pos, si, h = train_view(m, r, fwd)
		local read = { x = pos.x, y = pos.y, t = t, h = h, si = si, moving = speed ~= 0 and how ~= "stop", fwd = fwd }
		local emits = core.train_read(m, read)
		if #emits == 0 then
			return 0
		end
		surface_name(m, si)
		for _, emit in ipairs(emits) do
			local v = emit.read
			local s = sample(j, m, e, v.x, v.y, v.h, v.fwd)
			if v.t ~= t then
				s.tick = v.t
			end
			if not v.moving then
				s.stop = true
			end
			if emit.why == "es" then
				s.es = true
			end
			if emit.why ~= "chord" and (core.with_car(emit.why) or m.reshape) then
				s.car = car_list(r, v.fwd)
				m.reshape = nil
			end
			outbox.move(m.s, s)
		end
		return #emits
	end
	local pos, si, h, src
	if m.kind == "v" then
		src, pos, si = r, r.position, r.surface_index
		if not m.o then
			m.o, m.cf = r.type == "spider-vehicle" and "S" or "C", r.color and hex(r)
		end
		h = core.heading(pos.x, pos.y, nil, nil, r.orientation, false)
	else
		src, pos, si = r, r.physical_position, r.physical_surface_index
	end
	if not core.poll(m, pos.x, pos.y, si, t, how) then
		return 0
	end
	surface_name(m, si)
	local s = sample(j, m, src, pos.x, pos.y, h, true)
	if m.kind == "p" then
		s.name, s.f = r.name, r.force.name
	end
	if how == "stop" or (m.vx == 0 and m.vy == 0) then
		s.stop = true
	end
	outbox.move(m.s, s)
	return 1
end
movers.poll = poll

--- The scan: what moves now, minting a journey for each moving train and vehicle without one. A mover of the last walk
--- that is not in this one has stopped: one read with how = "stop".
local function scan(st, t)
	local walk, seen = {}, {}
	local function add(j)
		if not seen[j] then
			seen[j] = true
			walk[#walk + 1] = j
		end
	end
	for _, tr in pairs(game.train_manager.get_trains{ is_moving = true }) do
		local j = st.by_train[tr.id]
		if j and st.fast[j] and st.fast[j] >= t then
			seen[j] = true -- on the wheel: not walked, and not stopped
		else
			-- overdue: its slot came round on a tick the step skipped, so the walk takes it back
			if j then
				st.fast[j] = nil
			end
			add(j or mint(st, tr, "t", tr.id, t))
		end
	end
	for _, v in pairs(game.get_vehicles{ is_moving = true, type = vehicles() }) do
		add(st.by_unit[v.unit_number] or mint(st, v, "v", v.unit_number, t))
	end
	for _, p in pairs(movers.players()) do
		local j = "p:" .. p.name
		st.movers[j] = st.movers[j] or { kind = "p" }
		st.movers[j].ref = p
		add(j)
	end
	for _, j in ipairs(st.walk) do
		if not seen[j] then
			poll(st, j, t, "stop")
		end
	end
	st.walk, st.wi, st.next_scan = walk, 1, t + core.SCAN
end

--- The connected players. The game tests replace it: headless has none.
function movers.players()
	return game.connected_players
end

--- The per-tick step: the trains a busier tick left over, then this tick's wheel slot (at most core.FAST reads), then
--- up to core.WALK movers of the walk; with the walk done and core.SCAN ticks passed, a scan instead.
function movers.step(st, t)
	local slot = t % core.WHEEL
	local reads = 0
	while reads < core.FAST do
		local j = core.pop(st.carry)
		if j == nil then
			break
		end
		if st.fast[j] and st.fast[j] <= t then -- else read since, stopped or gone
			reads = reads + 1
			poll(st, j, t)
		end
	end
	local list = st.wheel[slot]
	if list then
		st.wheel[slot] = nil
		for _, j in ipairs(list) do
			if st.fast[j] == t then -- else stale: read since, stopped or gone
				if reads < core.FAST then
					reads = reads + 1
					poll(st, j, t)
				else
					core.push(st.carry, j)
				end
			end
		end
	end
	if st.wi > #st.walk then
		if t >= st.next_scan then
			scan(st, t)
		end
		return
	end
	for _ = 1, core.WALK do
		local j = st.walk[st.wi]
		if not j then
			return
		end
		st.wi = st.wi + 1
		if not st.fast[j] then
			poll(st, j, t)
		end
	end
end

--- Every train and vehicle gets a journey, so a parked one is known to the resync. Runs at install and at a Clusterio
--- config, both on every peer.
function movers.seed(st, t)
	for _, tr in pairs(game.train_manager.get_trains{}) do
		if not st.by_train[tr.id] then
			mint(st, tr, "t", tr.id, t)
		end
	end
	for _, v in pairs(game.get_vehicles{ type = vehicles() }) do
		if not st.by_unit[v.unit_number] then
			mint(st, v, "v", v.unit_number, t)
		end
	end
	st.seeded = true
end

--- A sample of every known mover as it is now, parked ones included, trains with `car`. Read-only: no mover state
--- changes, so a resync on one peer cannot desync the game. Returns the count.
function movers.resync(st, t)
	local n = 0
	for j, m in pairs(st.movers) do
		local r = m.ref
		if r and r.valid and not (m.kind == "p" and not r.connected) then
			local s
			if m.kind == "t" then
				local fwd = core.forward(m.fwd, r.speed)
				-- shaped into a copy, so the mover itself is left as it is
				local tmp = { kind = "t" }
				shape(tmp, r)
				local e, pos, si, h = train_view(tmp, r, fwd)
				s = sample(j, tmp, e, pos.x, pos.y, h, fwd)
				s.car = car_list(r, fwd)
				s.stop = r.speed == 0 or nil
				s.surface = game.get_surface(si).name
			elseif m.kind == "v" then
				local tmp = { kind = "v", o = r.type == "spider-vehicle" and "S" or "C", cf = r.color and hex(r) }
				s = sample(j, tmp, r, r.position.x, r.position.y,
					core.heading(0, 0, nil, nil, r.orientation, false), true)
				s.stop = r.speed == 0 or nil
				s.surface = r.surface.name
			else
				s = sample(j, m, r, r.physical_position.x, r.physical_position.y, nil, true)
				s.name, s.f = r.name, r.force.name
				s.surface = game.get_surface(r.physical_surface_index).name
			end
			local surface = s.surface
			s.surface = nil
			outbox.move(surface, s)
			n = n + 1
		end
	end
	return n
end

--- The new train takes old_train_id_1's journey, else old_train_id_2's; the other of two ends with gone.
function movers.on_train_created(st, e)
	if not st then
		return
	end
	local tr = e.train
	local ended = core.train_created(st, e.old_train_id_1, e.old_train_id_2, tr.id, tr, #tr.carriages, register)
	if ended then
		movers.gone(st, ended)
	end
end

--- Universal Edges source (ue.transfers.extend's capture): the crossing carries the journey. It reads storage only.
function movers.capture(st, ep, h)
	if not st then
		return nil
	end
	local j
	if ep.type == "train" then
		j = st.by_train[h.id]
	else
		j = st.by_unit[h.unit_number]
	end
	return j and { j = j } or nil
end

--- Universal Edges destination (materialized): take the source's journey over any entry, and tell the server, which
--- from then on accepts j only from this instance.
function movers.materialized(st, ep, h, data, meta)
	if not (st and type(data) == "table" and type(data.j) == "string") then
		return
	end
	local displaced
	if ep.type == "train" then
		displaced = core.adopt(st, data.j, "t", h.id, h, #h.carriages, register)
	else
		displaced = core.adopt(st, data.j, "v", h.unit_number, h, nil, register)
	end
	if displaced then
		movers.gone(st, displaced)
	end
	-- a crossing UE refuses after materialized destroys the train unsampled; with s set, gone clears the owner
	st.movers[data.j].s = (ep.type == "train" and h.front_stock or h).surface.name
	outbox.send{ type = "adopt", j = data.j, trace = meta.trace }
	outbox.send{ type = "trace", op = "timeshade.journey", phase = "adopted", level = "verbose", trace = meta.trace,
		journey = data.j, edge = ep.edge, cid = ep.cid }
end

return movers
