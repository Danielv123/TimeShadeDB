-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap module/movers.lua (Blue Oak Model License 1.0.0, see capture/README.md).
--
-- Movers (docs/v3/PROTOCOL.md sections 3.1 and 6): a scan of what moves, a walk polling core.WALK movers a tick, a wheel
-- of due train reads, journeys minted by the scan and followed through joins, splits and Universal Edges crossings.
--
-- Trains are read every core.STEP tiles. Each read notes the rail under the leading joint: the curved pieces and ramps
-- it enters, joined by straight rails, are the train's exact path (docs/v3/RAIL-GEOMETRY.md 8), sent with the next
-- sample (`rp`). So only the timing needs samples (core.train_read): a sample carries the game tick of its read, the
-- speed (`v`) and the leading carriage's own orientation (`h`), every carriage's position and orientation (`car`) on
-- first sight, stop, start, end switch and resync, `es` when the leading end switched, and `ru` with the pieces under the
-- whole train where a new rail path begins. Vehicles and players are sent by dead reckoning (core.poll); vehicles carry
-- their orientation.
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
	m.ob = core.flip_kinds(m.o) -- the kinds read from the back end, for samples while it leads
	local f, b = cars[1], cars[#cars]
	m.ef, m.eb = f, b
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

--- What a train looks like now: its leading carriage (front_stock while running forwards), position, surface, heading:
--- the leading carriage's own orientation turned to the direction of travel (the rail tangent at its centre,
--- PROTOCOL.md 3.1), and that orientation unrounded (turns). m must be shaped.
local function train_view(m, r, fwd)
	local e = fwd and m.ef or m.eb
	local pos = e.position
	local headed = fwd and m.hf or m.hb
	local o = e.orientation
	if headed ~= fwd then
		o = o + 0.5
	end
	return e, pos, e.surface_index, core.heading(pos.x, pos.y, nil, nil, o, false), o
end

-- Rail pieces by unit number: { kind, direction, x, y } for curved pieces and ramps (core.RAIL_KINDS), false for straight
-- rails. Rails never change, so this only saves API reads and decides nothing; file-local, emptied past RAIL_CACHE.
local RAIL_CACHE = 65536
local rail_cache, rail_cached = {}, 0

--- The piece info of rail entity `rail` with unit number un.
local function rail_info(rail, un)
	local info = rail_cache[un]
	if info == nil then
		local kind = core.RAIL_KINDS[rail.type]
		if kind then
			local p = rail.position
			info = { kind, rail.direction, math.floor(p.x + 0.5), math.floor(p.y + 0.5) }
		else
			info = false
		end
		if rail_cached >= RAIL_CACHE then
			rail_cache, rail_cached = {}, 0
		end
		rail_cache[un] = info
		rail_cached = rail_cached + 1
	end
	return info
end

--- The curved pieces and ramps under train tr as `rp` entries { x, y, direction, kind }, from the rear to the leading
--- end, at most `max` (those nearest the leading end); nil when there are none.
local function rails_under(tr, fwd, max)
	local list = tr.get_rails() -- from the train's front end to its back end
	local out = {}
	local n = #list
	for i = 1, n do
		local rail = list[fwd and i or n + 1 - i] -- leading end first
		local info = rail_info(rail, rail.unit_number)
		if info then
			out[#out + 1] = { info[3], info[4], info[2], info[1] }
			if #out >= max then
				break
			end
		end
	end
	-- rear first
	local m = #out
	if m == 0 then
		return nil
	end
	for i = 1, math.floor(m / 2) do
		out[i], out[m + 1 - i] = out[m + 1 - i], out[i]
	end
	return out
end

--- Where a train enters and leaves piece info ({ kind, direction, x, y }) when it runs towards its front end (ahead_front)
--- or its back end: entry x, y, outward direction, then exit x, y, outward direction.
local function piece_way(info, ahead_front)
	local en = core.piece_ends(info[1], info[2])
	local px, py = info[3], info[4]
	if ahead_front then
		return px + en[4], py + en[5], en[6], px + en[1], py + en[2], en[3]
	end
	return px + en[1], py + en[2], en[3], px + en[4], py + en[5], en[6]
end

--- The curved pieces and ramps between rail unit `stop` (or `stop2`) and the rail under the leading joint, found by
--- walking back from it over the rails under the train: rear first as { info, ahead_front, unit number }, or nil when
--- the walk does not reach either rail within `max` rails. The train itself went over those connections last, so the
--- natural rail behind each is the one it came from.
local function fill(le, stop, stop2, max)
	local walk = le.make_copy()
	walk.flip_direction()
	local found = {}
	for _ = 1, max do
		if not walk.move_natural() then
			return nil
		end
		local rail = walk.rail
		local un = rail.unit_number
		if un == stop or un == stop2 then
			local out = {}
			for i = #found, 1, -1 do
				out[#out + 1] = found[i]
			end
			return out
		end
		local info = rail_info(rail, un)
		if info then
			-- walking backwards the end ahead is the train's entry; the train runs towards the other
			found[#found + 1] = { info, walk.direction ~= defines.rail_direction.front, un }
		end
	end
	return nil
end

--- Appends piece info, run towards its front end when ahead_front, to m's recorded path when it continues it (unit
--- number un). Returns whether it did.
local function record(m, info, ahead_front, un)
	local ix, iy, id, xx, xy, xd = piece_way(info, ahead_front)
	local ex = m.ex
	if not core.connects(ex[1], ex[2], ex[3], ix, iy, id) then
		return false
	end
	local rp = m.rp
	rp[#rp + 1] = { info[3], info[4], info[2], info[1] }
	m.rpn = m.rpn + 1
	m.ex, m.xu = { xx, xy, xd }, un
	return true
end

--- The rail end the leading joint runs towards, as { x, y, outward direction }: where the rail path goes on from.
local function exit_of(le)
	local loc = le.location
	return { math.floor(loc.position.x + 0.5), math.floor(loc.position.y + 0.5), loc.direction }
end

-- core.LIVE_GAP at the game's speed (2 s of real time), set by each step: a game at speed 0.2 needs a sample every 24
-- ticks for the same. Game state, so the same on every peer.
local live_gap = core.LIVE_GAP

--- The fields every sample of mover m carries. Trains take the leading carriage's name and the force from the shape;
--- vehicles and players read them from src.
local function sample(j, m, src, x, y, h, fwd)
	local o, c = m.o, m.cf
	if m.kind == "t" then
		local name = m.nf
		if not fwd then
			o, c, name = m.ob or (o and core.flip_kinds(o)), m.cb, m.nb
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
		local fwd -- core.forward, inlined: this runs for every train read
		if speed ~= 0 then
			fwd = speed > 0
		else
			fwd = m.fwd ~= false
		end
		if not (m.o and m.ef and m.ef.valid and m.eb and m.eb.valid) then
			shape(m, r)
		end
		if not m.rp then
			m.rp, m.rpn = {}, 0
		end
		local e, pos, si, h, o = train_view(m, r, fwd)
		local switched = m.fwd ~= nil and fwd ~= m.fwd
		-- the leading joint: the carriage centre plus half the joint distance along its axis (RAIL-GEOMETRY.md 3)
		local a = o * 2 * math.pi
		local jx, jy = pos.x + 2 * math.sin(a), pos.y - 2 * math.cos(a)
		local v = math.abs(speed)
		if m.jx and not switched then
			-- the odometer: chords between reads, lengthened to the arc by the turn between them
			local dx, dy = jx - m.jx, jy - m.jy
			local turn = (o - m.jo) % 1
			if turn > 0.5 then
				turn = 1 - turn
			end
			turn = turn * 2 * math.pi
			m.odo = (m.odo or 0) + math.sqrt(dx * dx + dy * dy) * (1 + turn * turn / 24)
		end
		local accel
		if speed ~= 0 then
			if m.lt and t > m.lt and t - m.lt <= 2 * core.SLOW then
				accel = (v - m.lv) / (t - m.lt)
			elseif m.e and not m.moving and (m.odo or 0) - m.e.u > 0.05 then
				-- the first read after a start: uniform acceleration from rest over the distance since the stop
				accel = v * v / (2 * (m.odo - m.e.u))
			end
		end
		local turned = o ~= m.jo
		m.jx, m.jy, m.jo, m.lv, m.lt = jx, jy, o, v, t
		-- a moving train is read again when it has gone core.STEP tiles; at rest it leaves the wheel
		if speed ~= 0 then
			local due = t + core.next_read(speed, accel)
			if m.e and m.moving and due > m.e.t + live_gap then
				-- read when the live gap ends, so no moving train goes longer without a sample
				due = math.max(t + 1, m.e.t + live_gap)
			end
			st.fast[j] = due
			local slot = due % core.WHEEL
			local list = st.wheel[slot] or {}
			st.wheel[slot] = list
			list[#list + 1] = j
		elseif st.fast[j] then
			st.fast[j] = nil
		end
		-- the rail under the leading joint: a curved piece or ramp it entered must continue the path recorded so far. It is
		-- read while the leading carriage turns or the last rail read was a curved piece or ramp: on a straight the
		-- carriage's orientation stays exactly the same until its front joint is on a curve, and a read comes at most 5
		-- tiles later, still on that piece (5.078 at least). A ramp shows no turn, so on a straight the rail is also read
		-- every core.RAMP_READS reads: at most 15 tiles apart, and a ramp is 16 long. (A path without its ramps is right in
		-- plan, but its rail distance is short by about 0.8 tile a ramp, and the carriages behind one are drawn that far off.)
		local le, un
		local gap = false
		local look = turned or m.curved ~= false
		if look then
			m.sk = 0
		else
			local sk = (m.sk or 0) + 1
			look = sk >= core.RAMP_READS
			m.sk = look and 0 or sk
		end
		if m.ex and not switched and look then
			le = fwd and r.front_end or r.back_end
			local rail = le.rail
			un = rail.unit_number
			if un ~= m.ru then
				local info = rail_info(rail, un)
				m.curved = info and true or false
				-- the train runs towards le.direction's end
				local ahead_front = le.direction == defines.rail_direction.front
				if info and not record(m, info, ahead_front, un) then
					-- a piece went by unseen: find it behind, else the path breaks here
					local missed = fill(le, m.xu, nil, core.FILL_MAX)
					gap = true
					if missed then
						local ok = true
						for _, q in ipairs(missed) do
							ok = ok and record(m, q[1], q[2], q[3])
						end
						gap = not (ok and record(m, info, ahead_front, un))
					end
				elseif not info and turned and m.ru then
					-- The carriage turns, yet the rail under the train's end is straight: a curved piece may have passed
					-- under the end between two reads that did not look at the rail (the end leads the carriage, so a
					-- short piece can come and go before the carriage turns). Walk back to the last rail looked at, or
					-- the last piece recorded, and record what continues the path (a piece already recorded does not).
					local missed = fill(le, m.ru, m.xu, core.SEEN_MAX)
					if missed then
						for _, q in ipairs(missed) do
							record(m, q[1], q[2], q[3])
						end
					end
				end
			end
			m.ru = un
		end
		local read = { x = pos.x, y = pos.y, t = t, h = h, si = si, moving = speed ~= 0 and how ~= "stop", fwd = fwd,
			v = v, u = m.odo or 0, np = m.rpn, gap = gap,
			full = #m.rp >= core.RP_MAX or (m.e ~= nil and m.moving and t - m.e.t >= live_gap) }
		local emits = core.train_read(m, read)
		if #emits == 0 then
			return 0
		end
		surface_name(m, si)
		local started = false
		for _, emit in ipairs(emits) do
			local rv = emit.read
			local s = sample(j, m, e, rv.x, rv.y, rv.h, rv.fwd)
			s.v = core.round(rv.v, 4)
			if rv.t ~= t then
				s.tick = rv.t
			end
			if not rv.moving then
				s.stop = true
			end
			if emit.why == "es" then
				s.es = true
			end
			local car = emit.why ~= "vertex" and (core.with_car(emit.why) or m.reshape)
			if core.run_start(emit.why) then
				-- a new rail path: the pieces under the train (this is always the current read)
				s.ru, s.rp = true, rails_under(r, rv.fwd, core.rp_start(car and m.n or nil))
				started = true
			else
				-- the pieces entered up to this read
				local rp = m.rp
				local n = rv.np - (m.rpn - #rp)
				if n > 0 then
					local sent, rest = {}, {}
					for i = 1, #rp do
						if i <= n then
							sent[#sent + 1] = rp[i]
						else
							rest[#rest + 1] = rp[i]
						end
					end
					s.rp, m.rp = sent, rest
				end
			end
			if car then
				s.car = car_list(r, rv.fwd)
				m.reshape = nil
			end
			outbox.move(m.s, s)
		end
		if started then
			-- the path goes on from the rail under the leading joint now
			if not le then
				le = fwd and r.front_end or r.back_end
				un = le.rail.unit_number
			end
			m.rp, m.ex, m.xu, m.ru, m.curved = {}, exit_of(le), un, un, nil
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
	live_gap = math.max(core.SLOW, math.floor(core.LIVE_GAP * math.max(0.5, math.min(1, game.speed)) + 0.5))
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
				s.v = core.round(math.abs(r.speed), 4)
				-- a resync sample begins a rail path from the pieces under the train; the next sample's pieces, entered since
				-- the last sample before it, continue that path where they are not already under the train
				s.ru, s.rp = true, rails_under(r, fwd, core.rp_start(#r.carriages))
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
