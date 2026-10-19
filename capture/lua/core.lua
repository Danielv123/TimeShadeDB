-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap module/capture_core.lua (Blue Oak Model License 1.0.0, see capture/README.md).
--
-- Pure rules of the TimeShadeDB capture (docs/v3/PROTOCOL.md): constants, the gridworld clip, chunk keys, FIFO
-- queues, the chart debounce, the bounded chart cache, train emission (chord, heading, heartbeat), dead reckoning for
-- vehicles and players, journeys, the byte-exact line cut, and the storage migrations. State comes in as arguments and
-- no Factorio global is touched, so these run in plain Lua (capture/test/run_unit.mjs) as well as in the game.
--
-- Written for Lua 5.2 (Factorio) and kept valid on 5.3/5.4 (the unit test runner): no math.atan2 without a fallback,
-- and numbers that end up in strings go through string.format("%d").
local core = {}

core.PROTOCOL = 1 -- the envelope's v

-- Terrain.
core.CAP = 20000 -- chart reads waiting; event pushes past this are dropped and counted; resync pushes are exempt
core.CHARTS = 2 -- chart reads a tick
core.DEBOUNCE = 600 -- ticks from one read of a chunk to the next (a chunk charted again sooner waits)
core.CACHE = 16384 -- last sent charts kept (deflated); a miss only re-sends an unchanged chart
core.RESYNC_READS = 64 -- chunk positions the resync walk reads a tick
core.RESYNC_CHUNKS = 16 -- the walk stops for the tick once this many charted chunks are queued
core.RESYNC_QUEUE = 64 -- the walk waits while this many chart reads are queued, so the queue stays short
core.RESYNC_CHARTS = 4 -- chart reads a tick while a resync runs (50,000 chunks in about 210 s)
core.MACHINES = 8 -- resync machines and rails sent a tick

-- Movers.
core.WALK = 8 -- movers polled a tick
core.SCAN = 30 -- ticks at least from one scan to the next
core.EPS = 1 -- tiles off the dead-reckoned prediction before a vehicle or player sample
core.STEP = 4 -- tiles a moving train goes between reads
core.SLOW = 120 -- the longest wait between reads of a crawling train
core.WHEEL = 128 -- slots of the wheel of due reads; more than core.SLOW
core.FAST = 32 -- train reads a tick at most
core.HEARTBEAT = 600 -- ticks from a mover's sample to its next while nothing else asks for one
core.CHORD_EPS = 0.25 -- tiles a read may lie off the chord from the last sample to the current read (trains)
core.SED_EPS = 1 -- tiles a read may lie off its time-interpolated place on that chord (trains)
core.HEAD_EPS = 2 / 360 -- orientation change since the last sample that sends one (trains; about 2 degrees)
core.BUF_MAX = 16 -- reads kept since a train's last sample; a full buffer sends one
core.CAR_MAX = 100 -- carriages listed in `car`; with 200 kinds and a 200-byte name a sample stays under a line

-- Transport.
core.LIMIT = 3500 -- bytes a printed line stays under (Clusterio IPC)
core.MOVE_MAX = 3 -- samples without `car` a move message carries
core.SURFACES_MAX = 13 -- surfaces a surfaces message carries
core.KINDS_MAX = 200 -- carriage kinds sent

local atan2 = math.atan2 or math.atan

local function int(v)
	return string.format("%d", v)
end

--- The tile's chunks [x1, x2) x [y1, y2), or nil without a tile. Config checks make size a multiple of 64.
function core.bounds(tile)
	if not tile then
		return nil
	end
	local c, h = tile.size / 32, tile.size / 64
	return { x1 = tile.x * c - h, y1 = tile.y * c - h, x2 = tile.x * c + h, y2 = tile.y * c + h }
end

--- Whether the surface named `surface` on planet `planet` (or nil) is one of the tile's: tile.surface always, any other
--- planet unless the world's exclude list names it. Without a tile, none is.
function core.tiled(tile, surface, planet)
	if not tile then
		return false
	end
	if surface == tile.surface then
		return true
	end
	if planet == nil then
		return false
	end
	for _, name in pairs(tile.exclude or {}) do
		if name == planet then
			return false
		end
	end
	return true
end

--- Whether chunk (cx, cy) is captured: on a tiled surface only inside the tile's bounds; any other surface whole.
function core.captured(tile, b, surface, cx, cy, planet)
	if not core.tiled(tile, surface, planet) then
		return true
	end
	return cx >= b.x1 and cx < b.x2 and cy >= b.y1 and cy < b.y2
end

--- A LuaSurface's planet name, or nil (a platform's, a lab's).
function core.planet(s)
	return s.planet and s.planet.name
end

-- A chunk's key, per surface and force index, as one number: si * 2^40 + fi * 2^34 + (x + 2^16) * 2^17 + (y + 2^16).
-- Chunk coordinates stay within +-31,250 (the 1,000,000-tile map limit), so x and y take 17 bits each; forces stay below
-- 64 (6 bits) and surfaces below 2^13, so the key stays below 2^53 and is exact in Lua 5.2's doubles. A number, not a
-- string: this runs for every chart event, and making a string costs more than the rest of the handler.
local K40, K34, K17, K16 = 2 ^ 40, 2 ^ 34, 2 ^ 17, 2 ^ 16

function core.key(si, fi, x, y)
	return si * K40 + fi * K34 + (x + K16) * K17 + (y + K16)
end

--- The surface index of a key.
function core.key_surface(key)
	return math.floor(key / K40)
end

--- A FIFO: items at [head, tail].
function core.queue()
	return { head = 1, tail = 0 }
end

function core.size(q)
	return q.tail - q.head + 1
end

--- Append item. With cap, refuse it (false) once cap items wait.
function core.push(q, item, cap)
	if cap and core.size(q) >= cap then
		return false
	end
	q.tail = q.tail + 1
	q[q.tail] = item
	return true
end

function core.peek(q)
	return q[q.head]
end

function core.pop(q)
	if q.head > q.tail then
		return nil
	end
	local item = q[q.head]
	q[q.head] = nil
	q.head = q.head + 1
	if q.head > q.tail then
		q.head, q.tail = 1, 0
	end
	return item
end

-- Bounded chart cache: two generations of at most cap/2 entries each. Lookups find the young generation first and
-- promote an old hit; when the young one is full it becomes the old one and the previous old one is dropped. So at most
-- cap entries are held, every operation is O(1), and recently used keys survive (an approximate LRU).

function core.cache(cap)
	return { half = math.max(1, math.floor(cap / 2)), young = {}, old = {}, n = 0 }
end

function core.cache_set(c, k, v)
	if c.young[k] == nil then
		if c.n >= c.half then
			c.old, c.young, c.n = c.young, {}, 0
		end
		c.n = c.n + 1
	end
	c.young[k] = v
	c.old[k] = nil
end

function core.cache_get(c, k)
	local v = c.young[k]
	if v ~= nil then
		return v
	end
	v = c.old[k]
	if v ~= nil then
		core.cache_set(c, k, v)
	end
	return v
end

function core.cache_del(c, k)
	if c.young[k] ~= nil then
		c.young[k] = nil
		c.n = c.n - 1
	end
	c.old[k] = nil
end

--- Entries held (young plus old); at most twice half.
function core.cache_count(c)
	local n = 0
	for _ in pairs(c.old) do
		n = n + 1
	end
	return c.n + n
end

-- Chart debounce. A chunk read at tick t cools down until t + core.DEBOUNCE. A chunk charted while waiting in the queue
-- is not queued twice; one charted while cooling is marked dirty and queued when its cool-down ends. The cool-down list
-- is a FIFO, because every read adds now + DEBOUNCE with now never decreasing, so the state holds at most the chunks read
-- in the last DEBOUNCE ticks plus the queue (bounded by core.CAP).
--
-- raw[key] keeps the raw chart of an event read while its chunk cools down, so the re-read at the end of the cool-down
-- can skip helpers.encode_string when nothing changed (deflating 2 KiB is most of a read's cost). It is dropped when a
-- cool-down ends with no new event, so it holds at most core.CHARTS x core.DEBOUNCE charts (2.4 MiB at the defaults).

function core.debounce()
	return { queue = core.queue(), pending = {}, cool = {}, dirty = {}, due = core.queue(), raw = {}, drops = 0 }
end

--- An on_chunk_charted for item ({ key, ... }). forced items (resync, deletion) skip the cool-down and the cap.
--- Returns "queued", "dirty", "waiting" (already queued) or "dropped".
function core.charted(d, item, forced)
	local key = item.key
	if forced then
		core.push(d.queue, item)
		if key then -- the resync's sentinel has none
			d.pending[key] = true
			d.dirty[key] = nil
		end
		return "queued"
	end
	if d.pending[key] then
		return "waiting"
	end
	if d.cool[key] then
		d.dirty[key] = item
		return "dirty"
	end
	if core.push(d.queue, item, core.CAP) then
		d.pending[key] = true
		return "queued"
	end
	d.drops = d.drops + 1
	return "dropped"
end

--- The next item to read at tick now, or nil. The caller reads it; the chunk then cools down.
function core.take(d, now)
	local item = core.pop(d.queue)
	if item == nil then
		return nil
	end
	local key = item.key
	if key then
		d.pending[key] = nil
		if not d.cool[key] then
			d.cool[key] = true
			core.push(d.due, { key = key, t = now + core.DEBOUNCE })
		end
	end
	return item
end

--- End the cool-downs due at tick now; dirty chunks join the queue. Returns how many were queued.
function core.cooled(d, now)
	local n = 0
	while true do
		local head = core.peek(d.due)
		if not head or head.t > now then
			return n
		end
		core.pop(d.due)
		d.cool[head.key] = nil
		local item = d.dirty[head.key]
		if item then
			d.dirty[head.key] = nil
			if core.charted(d, item) == "queued" then
				n = n + 1
			else
				d.raw[head.key] = nil
			end
		else
			d.raw[head.key] = nil
		end
	end
end

--- Whether a train leads with its front: towards its front while speed > 0, its back while < 0, and the end it last led
--- with (prev, nil before any) while stopped; the front to begin with.
function core.forward(prev, speed)
	if speed ~= 0 then
		return speed > 0
	end
	return prev ~= false
end

--- Ticks until a train at `speed` (tiles a tick, not 0) is read again: core.STEP tiles on, within [1, core.SLOW].
function core.next_read(speed)
	return math.min(core.SLOW, math.max(1, math.ceil(core.STEP / math.abs(speed))))
end

function core.round(v, places)
	local m = 10 ^ places
	return math.floor(v * m + 0.5) / m
end

local function round3(v)
	return core.round(v, 3) % 1
end

--- A train's leading end's heading as an orientation (0 north, 0.25 east, 3 decimals): from its second carriage's
--- position (sx, sy) towards the leading one's (x, y); with one carriage, its own orientation, half a turn on when it
--- leads with its back.
function core.heading(x, y, sx, sy, orientation, back)
	if sx then
		return round3(atan2(x - sx, sy - y) / (2 * math.pi))
	end
	return round3(orientation + (back and 0.5 or 0))
end

--- The smaller angle between two orientations, in turns (0..0.5).
function core.turn(a, b)
	local d = math.abs(a - b) % 1
	return math.min(d, 1 - d)
end

--- A train's kinds read from its other end: the order reversed, each carriage's case swapped.
function core.flip_kinds(o)
	return (o:reverse():gsub("%a", function(ch)
		return ch:lower() == ch and ch:upper() or ch:lower()
	end))
end

--- One poll of a vehicle or player m at (x, y) on surface index si. Returns true when a sample is due: on the second poll
--- (the first only records, so the velocity is measured), past core.EPS off the dead-reckoned prediction on either
--- axis, on a surface change, or after core.HEARTBEAT ticks. how = "stop" sends at zero velocity.
function core.poll(m, x, y, si, tick, how)
	local first = m.pt == nil
	local vx, vy = 0, 0
	if not first and how ~= "stop" and m.psi == si and tick > m.pt then
		vx, vy = (x - m.px) * 60 / (tick - m.pt), (y - m.py) * 60 / (tick - m.pt)
	end
	m.px, m.py, m.pt, m.psi = x, y, tick, si
	local due = how ~= nil
	if not due and not first then
		if m.t == nil or si ~= m.si or tick - m.t >= core.HEARTBEAT then
			due = true
		else
			local s = (tick - m.t) / 60
			due = math.abs(x - m.x - m.vx * s) > core.EPS or math.abs(y - m.y - m.vy * s) > core.EPS
		end
	end
	if due then
		m.x, m.y, m.vx, m.vy, m.t, m.si = x, y, vx, vy, tick, si
	end
	return due
end

--- Whether some buffered read lies too far from the chord e -> c: more than core.CHORD_EPS off the line, or more than
--- core.SED_EPS off where linear motion in time along the chord puts it (synchronised distance).
function core.off_chord(e, buf, c)
	local dx, dy, dt = c.x - e.x, c.y - e.y, c.t - e.t
	local len = math.sqrt(dx * dx + dy * dy)
	for i = 1, #buf do
		local p = buf[i]
		local px, py = p.x - e.x, p.y - e.y
		local perp
		if len < 1e-6 then
			perp = math.sqrt(px * px + py * py)
		else
			perp = math.abs(dx * py - dy * px) / len
		end
		if perp > core.CHORD_EPS then
			return true
		end
		local f = dt > 0 and (p.t - e.t) / dt or 0
		local ex, ey = px - dx * f, py - dy * f
		if math.sqrt(ex * ex + ey * ey) > core.SED_EPS then
			return true
		end
	end
	return false
end

--- One read r = { x, y, t, h, si, moving, fwd } of train m. Returns the emits: a list of { read, why } in order, where
--- why is "first", "stop", "start", "es" (leading end switched), "surface", "heartbeat", "heading", "full" (the read
--- buffer is full) or "chord" (a past read sent as a vertex; it carries its own tick). core.with_car tells which carry
--- the carriage geometry. m keeps the last sent read (m.e), the reads since (m.buf) and the moving and leading-end state.
---
--- The chord rule: the reads since the last sample must all lie within core.CHORD_EPS of the straight line from that
--- sample to the current read, and within core.SED_EPS of where constant speed along it puts them. When one does not, the
--- previous read is sent as a vertex and becomes the chord's new start. So the polyline of samples stays within a
--- quarter tile of every read, which are core.STEP tiles apart.
function core.train_read(m, r)
	local out = {}
	local e = m.e
	local why
	if e == nil then
		why = "first"
	elseif r.si ~= e.si then
		why = "surface"
	elseif not r.moving then
		if m.moving then
			why = "stop"
		elseif r.t - e.t >= core.HEARTBEAT then
			why = "heartbeat"
		end
	elseif m.fwd ~= nil and r.fwd ~= m.fwd then
		why = "es" -- also when it starts off the other way after a stop
	elseif not m.moving then
		why = "start"
	elseif r.t - e.t >= core.HEARTBEAT then
		why = "heartbeat"
	end
	local buf = m.buf or {}
	if why ~= "first" and why ~= "surface" and #buf > 0 then
		-- before an end switch the last read of the old leading end is always sent: the new end is a train length away
		if why == "es" or core.off_chord(e, buf, r) then
			local v = buf[#buf]
			out[#out + 1] = { read = v, why = "chord" }
			e, buf = v, {}
		end
	end
	if why == nil and r.moving then
		if r.h and e.h and core.turn(r.h, e.h) > core.HEAD_EPS then
			why = "heading"
		elseif #buf >= core.BUF_MAX then
			why = "full"
		end
	end
	if why then
		out[#out + 1] = { read = r, why = why }
		m.e, m.buf = r, {}
	else
		if r.moving then
			buf[#buf + 1] = r
		end
		m.e, m.buf = e, buf
	end
	m.moving, m.fwd = r.moving, r.fwd
	return out
end

--- Whether an emit's why sends the carriage geometry.
function core.with_car(why)
	return why == "first" or why == "stop" or why == "start" or why == "es" or why == "force"
end

--- A journey id, "<instance>.<tick>.<train id>", or "<instance>.<tick>.v<unit>" for a vehicle.
function core.mint(instance, tick, key, kind)
	return int(instance) .. "." .. int(tick) .. "." .. (kind == "v" and "v" or "") .. int(key)
end

local function index(st, kind)
	return kind == "t" and st.by_train or st.by_unit
end

--- Give journey j to the train or vehicle `key` (handle ref, n carriages), creating its mover if none. The registration
--- moves to ref; register(ref) -> registration number is the caller's impure part.
function core.claim(st, j, kind, key, ref, n, register)
	local m = st.movers[j]
	if not m then
		m = { kind = kind }
		st.movers[j] = m
	end
	if m.reg and st.reg[m.reg] == j then
		st.reg[m.reg] = nil
	end
	if m.key and m.key ~= key and index(st, m.kind)[m.key] == j then
		index(st, m.kind)[m.key] = nil
	end
	-- the train's kinds and colours are read again at its next sample; its geometry changed, so that sample carries car
	m.ref, m.key, m.n, m.o = ref, key, n, nil
	m.reshape = true
	m.reg = register(ref)
	st.reg[m.reg] = j
	index(st, kind)[key] = j
end

--- on_train_created takes old_train_id_1's journey, else old_train_id_2's; with both, it keeps the first and returns
--- the second, which the caller ends with gone. It never mints: a train with neither waits for a scan.
function core.train_created(st, old1, old2, id, ref, n, register)
	local j1 = old1 and st.by_train[old1]
	local j2 = old2 and st.by_train[old2]
	if old1 then
		st.by_train[old1] = nil
	end
	if old2 then
		st.by_train[old2] = nil
	end
	local j = j1 or j2
	if j then
		core.claim(st, j, "t", id, ref, n, register)
	end
	if j1 and j2 and j1 ~= j2 then
		return j2
	end
	return nil
end

--- A crossing's destination adopts the source's journey over any entry, and samples it afresh. Returns the journey the
--- handle carried before, if another: the caller ends it with gone.
function core.adopt(st, j, kind, key, ref, n, register)
	local old = index(st, kind)[key]
	core.claim(st, j, kind, key, ref, n, register)
	local m = st.movers[j]
	m.pt, m.t, m.e, m.buf, m.moving, m.fwd = nil, nil, nil, nil, nil, nil
	if old and old ~= j then
		return old
	end
	return nil
end

--- Forget journey j, with its registration and its index entry.
function core.forget(st, j)
	local m = st.movers[j]
	if not m then
		return
	end
	if m.reg and st.reg[m.reg] == j then
		st.reg[m.reg] = nil
	end
	if m.key and index(st, m.kind)[m.key] == j then
		index(st, m.kind)[m.key] = nil
	end
	st.movers[j] = nil
end

--- The envelope's head up to the messages: '{"v":1,"tick":<tick>[,"speed":<speed>],"msgs":['.
function core.head(tick, speed)
	local s = '{"v":' .. core.PROTOCOL .. ',"tick":' .. int(tick)
	if speed and speed ~= 1 then
		s = s .. ',"speed":' .. string.format("%.6g", speed)
	end
	return s .. ',"msgs":['
end

--- Lines of envelopes from each message's own JSON, each line under limit bytes, counted on the real strings. A part
--- too big for a line of its own is dropped and counted. Returns the lines and the count.
function core.cut(tick, parts, limit, speed)
	local head = core.head(tick, speed)
	local fixed = #head + 2
	local lines, batch, size, dropped = {}, {}, fixed, 0
	for _, p in ipairs(parts) do
		if fixed + #p >= limit then
			dropped = dropped + 1
		else
			local add = #p + (#batch > 0 and 1 or 0)
			if size + add >= limit then
				lines[#lines + 1] = head .. table.concat(batch, ",") .. "]}"
				batch, size, add = {}, fixed, #p
			end
			batch[#batch + 1] = p
			size = size + add
		end
	end
	if #batch > 0 then
		lines[#lines + 1] = head .. table.concat(batch, ",") .. "]}"
	end
	return lines, dropped
end

core.MIGRATIONS = {
	--- 1: the persistent capture state. Everything here changes only in events and on_tick steps that run identically on
	--- every peer, so a multiplayer game stays in sync; what only decides what is printed lives in file-locals instead.
	function(s)
		s.configured, s.seeded, s.forces = false, false, {}
		s.movers, s.by_train, s.by_unit = {}, {}, {}
		s.walk, s.wi, s.next_scan = {}, 1, 0
		s.reg = {}
		s.wheel, s.fast, s.carry = {}, {}, core.queue()
		s.file_seq = 0
	end,
}

--- Run the migrations above s.schema, in order. A schema newer than this code's is an error, and s is left untouched.
function core.migrate(s)
	local schema = s.schema or 0
	if type(schema) ~= "number" or schema > #core.MIGRATIONS then
		error("timeshade storage schema " .. tostring(schema) .. " is newer than this code's " .. #core.MIGRATIONS)
	end
	for i = schema + 1, #core.MIGRATIONS do
		core.MIGRATIONS[i](s)
		s.schema = i
	end
	return s
end

return core
