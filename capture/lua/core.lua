-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap module/capture_core.lua (Blue Oak Model License 1.0.0, see capture/README.md).
--
-- Pure rules of the TimeShadeDB capture (docs/PROTOCOL.md): constants, the gridworld clip, chunk keys, FIFO
-- queues, the chart debounce, the bounded chart cache, train emission (the timing model along the rail path, rail piece
-- ends, docs/RAIL-GEOMETRY.md), dead reckoning for
-- vehicles and players, journeys, the byte-exact line cut, and the storage migrations. State comes in as arguments and
-- no Factorio global is touched, so these run in plain Lua (capture/test/run_unit.mjs) as well as in the game.
--
-- Written for Lua 5.2 (Factorio) and kept valid on 5.3/5.4 (the unit test runner): no math.atan2 without a fallback,
-- and numbers that end up in strings go through string.format("%d").
local core = {}

core.PROTOCOL = 1 -- the envelope's v

-- Terrain.
core.CAP = 20000 -- chart reads the event queue's ring holds before it grows (it doubles; every charted chunk waits once)
core.CHARTS = 1 -- chart reads a tick (60 a second; the heaviest cost in the UPS benchmark)
core.DEBOUNCE = 600 -- ticks from one read of a chunk to the next (a chunk charted again sooner waits)
core.CACHE = 16384 -- last sent charts kept (deflated); a miss only re-sends an unchanged chart
core.RESYNC_READS = 64 -- chunk positions the resync walk reads a tick
core.RESYNC_CHUNKS = 16 -- the walk stops for the tick once this many charted chunks are queued
core.RESYNC_QUEUE = 64 -- the walk waits while this many forced reads are queued, so the forced queue stays short
core.RESYNC_CHARTS = 4 -- chart reads a tick while a resync runs (50,000 chunks in about 210 s)
core.MACHINES = 8 -- resync machines and rails sent a tick
core.RAW_QUEUE = 256 -- a chunk re-queued behind this many reads drops its raw chart (see core.debounce)

-- Movers.
core.WALK = 8 -- movers polled a tick
core.SCAN = 30 -- ticks at least from one scan to the next
core.EPS = 1 -- tiles off the dead-reckoned prediction before a vehicle or player sample
core.STEP = 5 -- tiles a moving train goes at most between reads (less than the shortest curved piece, 5.078)
core.SLOW = 30 -- the longest wait between reads of a moving train (a crawling or just started one)
core.WHEEL = 128 -- slots of the wheel of due reads; more than core.SLOW
core.FAST = 32 -- train reads a tick at most
core.HEARTBEAT = 600 -- ticks from a mover's sample to its next while nothing else asks for one
core.TIME_EPS = 0.1 -- rail distance a train's read may lie off the timing model from the last sample to the current read
core.BUF_MAX = 16 -- reads kept since a train's last sample for that check; a full buffer keeps every other one
core.RP_MAX = 32 -- rail pieces a train sample lists; a train that entered this many since its last sample sends one
core.RP_START_MAX = 120 -- rail pieces under the train a sample lists where a new rail path begins (fewer with `car`)
core.FILL_MAX = 64 -- rails a train's read walks back to find a curved piece it passed unseen (a read came late)
core.RAMP_READS = 3 -- on a straight a train's read looks at the rail under its leading joint every this many reads (ramps)
core.SEEN_MAX = 12 -- rails a turning train's read walks back over a straight to find a piece the reads between missed
core.LIVE_GAP = 120 -- ticks at game speed 1 a moving train goes without a sample at most, so live viewers stay ~2 s behind
core.CAR_MAX = 100 -- carriages listed in `car`; with 200 kinds and a 200-byte name a sample stays under a line

-- Transport. The defaults keep lines short; each host raises them with core.limits (the Clusterio module to 64 KiB
-- lines, the standalone mod's files to 200,000 bytes), so long trains are not cut short.
core.LIMIT = 3500 -- bytes a printed line stays under
core.MOVE_MAX = 3 -- samples without `car` a move message carries
core.SURFACES_MAX = 13 -- surfaces a surfaces message carries
core.KINDS_MAX = 200 -- carriage kinds sent

--- Set the line limit and what it bounds (fields line, car, kinds, rp_start; each optional) for a host's transport.
--- Every peer must use the same values: they decide what mover records hold.
function core.limits(t)
	core.LIMIT = t.line or core.LIMIT
	core.CAR_MAX = t.car or core.CAR_MAX
	core.KINDS_MAX = t.kinds or core.KINDS_MAX
	core.RP_START_MAX = t.rp_start or core.RP_START_MAX
end

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

local floor = math.floor

--- A key's surface index, force index and chunk x, y (exact: every step stays an integer below 2^53; integers on Lua
--- 5.3+ too, through math.floor).
function core.unkey(key)
	local y = floor(key % K17)
	local rest = floor(key / K17)
	local x = rest % 131072
	rest = floor(rest / 131072)
	return floor(rest / 64), rest % 64, x - 65536, y - 65536
end

--- A FIFO: items at [head, tail]. With cap, a ring of cap slots instead: its indices stay in 1..cap, so a queue that
--- never empties (a saturated chart queue) keeps its items in the table's array part instead of a hash part that grows
--- with every push; a full ring doubles (its items moved to the start of a ring twice the size).
function core.queue(cap)
	if cap then
		return { head = 1, n = 0, cap = cap }
	end
	return { head = 1, tail = 0 }
end

function core.size(q)
	if q.cap then
		return q.n
	end
	return q.tail - q.head + 1
end

--- Append item. With cap (a plain FIFO), refuse it (false) once cap items wait. A ring grows instead.
function core.push(q, item, cap)
	local rc = q.cap
	if rc then
		local n = q.n
		if n >= rc then
			local items = {}
			for i = 1, n do
				items[i] = q[(q.head + i - 2) % rc + 1]
			end
			for i = 1, rc do
				q[i] = nil
			end
			for i = 1, n do
				q[i] = items[i]
			end
			rc = 2 * rc
			q.head, q.cap = 1, rc
		end
		q[(q.head + n - 1) % rc + 1] = item
		q.n = n + 1
		return true
	end
	if cap and q.tail - q.head + 1 >= cap then
		return false
	end
	q.tail = q.tail + 1
	q[q.tail] = item
	return true
end

function core.peek(q)
	if q.cap and q.n == 0 then
		return nil
	end
	return q[q.head]
end

function core.pop(q)
	local head = q.head
	local rc = q.cap
	if rc then
		if q.n == 0 then
			return nil
		end
		local item = q[head]
		q[head] = nil
		q.head, q.n = head % rc + 1, q.n - 1
		return item
	end
	if head > q.tail then
		return nil
	end
	local item = q[head]
	q[head] = nil
	head = head + 1
	if head > q.tail then
		q.head, q.tail = 1, 0
	else
		q.head = head
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

-- Chart debounce. A queued read is the chunk's key alone (a number in the game; core.unkey gives the surface, force and
-- position back), so a busy map's queue of 20,000 reads holds no table per read for the garbage collector to walk.
-- Event reads wait in d.queue, a ring of core.CAP slots that doubles when full: a charted chunk waits once (pending), so
-- it holds at most every charted chunk of the map, and a read is only delayed, never dropped. Forced reads (a resync's
-- walk and its sentinel, a deletion) wait in d.forced, which is read first and paces the walk, so a long event queue
-- never stalls a resync. The resync's sentinel is the one table item, { resynced = walk number }.
-- A chunk read at tick t cools down until t + core.DEBOUNCE. A chunk charted while waiting in the queue is not queued
-- twice; one charted while cooling is marked dirty and queued when its cool-down ends. The cool-down list is a FIFO
-- (keys in d.due, their end ticks in d.due_t), because every read adds now + DEBOUNCE with now never decreasing, so the
-- state holds at most the chunks read in the last DEBOUNCE ticks plus the queue (at most the map's charted chunks).
--
-- raw[key] keeps the raw chart of an event read while its chunk cools down, so the re-read at the end of the cool-down
-- can skip helpers.encode_string when nothing changed (deflating 2 KiB is most of a read's cost). It is dropped when a
-- cool-down ends with no new event, and when the chunk is re-queued behind more than core.RAW_QUEUE reads (a long
-- queue would keep a chart per queued chunk). Event reads set it, at most max(core.CHARTS, core.RESYNC_CHARTS) a tick,
-- so it holds at most max(CHARTS, RESYNC_CHARTS) x DEBOUNCE + RAW_QUEUE + 1 charts (2,657 x 2 KiB, 5.2 MiB, at the
-- defaults).

function core.debounce()
	return { queue = core.queue(core.CAP), forced = core.queue(), pending = {}, cool = {}, dirty = {}, due = core.queue(),
		due_t = core.queue(), raw = {}, drops = 0 }
end

--- An on_chunk_charted for chunk key (any value but nil and false; a table is the resync's sentinel, forced only).
--- forced pushes (resync, deletion) skip the cool-down. Returns "queued", "dirty" or "waiting" (already queued).
function core.charted(d, key, forced)
	if forced then
		core.push(d.forced, key)
		if type(key) ~= "table" then
			d.pending[key] = true
			d.dirty[key] = nil
		end
		return "queued"
	end
	if d.pending[key] then
		return "waiting"
	end
	if d.cool[key] then
		d.dirty[key] = true
		return "dirty"
	end
	core.push(d.queue, key)
	d.pending[key] = true
	return "queued"
end

--- The next key to read at tick now and whether it is forced, or nil: from d.forced first, or only from "forced" or
--- "events" (d.queue). The caller reads it; the chunk then cools down. The sentinel comes back as it went in.
function core.take(d, now, from)
	local key, forced
	if from ~= "events" then
		key = core.pop(d.forced)
		forced = key ~= nil
	end
	if key == nil and from ~= "forced" then
		key = core.pop(d.queue)
	end
	if key == nil then
		return nil
	end
	if type(key) ~= "table" then
		d.pending[key] = nil
		if not d.cool[key] then
			d.cool[key] = true
			core.push(d.due, key)
			core.push(d.due_t, now + core.DEBOUNCE)
		end
	end
	return key, forced
end

--- End the cool-downs due at tick now; dirty chunks join the queue. Returns how many were queued.
function core.cooled(d, now)
	local n = 0
	local due, due_t, cool, dirty, raw = d.due, d.due_t, d.cool, d.dirty, d.raw
	while true do
		local t = core.peek(due_t)
		if not t or t > now then
			return n
		end
		core.pop(due_t)
		local key = core.pop(due)
		cool[key] = nil
		if dirty[key] then
			dirty[key] = nil
			local long = core.size(d.queue) > core.RAW_QUEUE
			if core.charted(d, key) == "queued" then
				n = n + 1
				if long then
					raw[key] = nil
				end
			else
				raw[key] = nil
			end
		else
			raw[key] = nil
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

--- Ticks until a train at `speed` (tiles a tick, not 0), accelerating by `accel` (tiles a tick per tick, or nil), is
--- read again: before it has gone more than core.STEP tiles (rounded down, so a train at 1.3 tiles a tick is read every
--- 3 ticks, not 4), within [1, core.SLOW]. No curved piece may pass between two reads (docs/RAIL-GEOMETRY.md 8). With
--- 5 tiles a train is read about as often as with the 2026-10 rule (4 tiles rounded up); 4 rounded down read 39% more on
--- the benchmark map.
function core.next_read(speed, accel)
	local v = math.abs(speed)
	local w = core.STEP / v
	if accel and accel > 1e-6 then
		w = (math.sqrt(v * v + 2 * accel * core.STEP) - v) / accel
	end
	return math.min(core.SLOW, math.max(1, math.floor(w)))
end

local SCALE = { [0] = 1, 10, 100, 1000 }

function core.round(v, places)
	local m = SCALE[places] or 10 ^ places
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

-- Rail pieces (docs/RAIL-GEOMETRY.md 2 and 8). The capture records the curved pieces and ramps a train's leading
-- joint enters; between them rails are straight. Kinds as PROTOCOL.md 3.1 `rp`: elevated pieces share the ground ones'.
core.RAIL_KINDS = { ["curved-rail-a"] = 1, ["elevated-curved-rail-a"] = 1, ["curved-rail-b"] = 2,
	["elevated-curved-rail-b"] = 2, ["legacy-curved-rail"] = 3, ["rail-ramp"] = 4 }

-- Each kind's ends for the piece built facing north (direction 0): front end offset and outward direction, then the back
-- end's (LuaEntity.get_rail_end). The other directions follow by the spec's 8-fold placement (2.1); ramps turn a quarter
-- per 4 of the direction.
local CANON = { { 0, 2, 8, -1, -3, 15 }, { 1, 2, 7, -2, -2, 14 }, { 1, 4, 8, -2, -3, 14 } }

local function place(i8, x, y)
	if i8 % 2 == 1 then
		x = -x
	end
	if math.floor(i8 / 2) % 2 == 1 then
		x, y = -y, x
	end
	if i8 >= 4 then
		x, y = -x, -y
	end
	return x, y
end

local function place_dir(i8, d)
	if i8 % 2 == 1 then
		d = (16 - d) % 16
	end
	if math.floor(i8 / 2) % 2 == 1 then
		d = (d + 4) % 16
	end
	if i8 >= 4 then
		d = (d + 8) % 16
	end
	return d
end

local ENDS = {}
for kind = 1, 4 do
	for dir = 0, 15 do
		local e
		if kind == 4 then
			local fx, fy = 0, 8
			for _ = 1, math.floor(dir / 4) % 4 do
				fx, fy = -fy, fx
			end
			local fd = (8 + dir - dir % 4) % 16
			e = { fx, fy, fd, -fx, -fy, (fd + 8) % 16 }
		else
			local c, i8 = CANON[kind], math.floor(dir / 2) % 8
			local fx, fy = place(i8, c[1], c[2])
			local bx, by = place(i8, c[4], c[5])
			e = { fx, fy, place_dir(i8, c[3]), bx, by, place_dir(i8, c[6]) }
		end
		ENDS[kind * 16 + dir] = e
	end
end

--- A piece's ends as offsets from its position: { front x, y, outward direction, back x, y, outward direction }.
function core.piece_ends(kind, dir)
	return ENDS[kind * 16 + dir]
end

-- Integer steps of the 16 rail directions: even ones the compass directions, odd ones the half-diagonals (1:2).
local DIR_STEPS = { [0] = { 0, -1 }, { 1, -2 }, { 1, -1 }, { 2, -1 }, { 1, 0 }, { 2, 1 }, { 1, 1 }, { 1, 2 }, { 0, 1 },
	{ -1, 2 }, { -1, 1 }, { -2, 1 }, { -1, 0 }, { -2, -1 }, { -1, -1 }, { -1, -2 } }

--- Whether the rail end (sx, sy) facing sd (outward) continues the end (ex, ey) facing ed: they face each other and the
--- first lies straight ahead of the second, on it or joined by straight rails.
function core.connects(ex, ey, ed, sx, sy, sd)
	if sd ~= (ed + 8) % 16 then
		return false
	end
	local st = DIR_STEPS[ed]
	local px, py = sx - ex, sy - ey
	return math.abs(px * st[2] - py * st[1]) < 1e-6 and px * st[1] + py * st[2] > -1e-6
end

--- The timing model between two samples (docs/RAIL-GEOMETRY.md 8): the rail distance tau ticks after the first, for
--- samples T ticks and L rail distance apart with speeds va and vb (tiles a tick). From rest, when uniform acceleration
--- to vb covers L in less than T, it stands until then; to rest, when uniform braking from va covers L in less than T,
--- it stops early and stands; otherwise a cubic Hermite in time, held within [0, L].
function core.timing_u(L, T, va, vb, tau)
	if T <= 0 then
		return L
	end
	if L > 0 then
		if va <= 0 and vb > 0 and 2 * L < vb * T then
			local tm = 2 * L / vb
			local s = tau - (T - tm)
			if s <= 0 then
				return 0
			end
			return vb * s * s / (2 * tm)
		end
		if vb <= 0 and va > 0 and 2 * L < va * T then
			local tm = 2 * L / va
			local s = math.min(tau, tm)
			return va * s - va * s * s / (2 * tm)
		end
	end
	local f = tau / T
	local f2 = f * f
	local f3 = f2 * f
	local u = (f3 - 2 * f2 + f) * T * va + (3 * f2 - 2 * f3) * L + (f3 - f2) * T * vb
	local lo, hi = 0, L
	if L < 0 then
		lo, hi = L, 0
	end
	if u < lo then
		return lo
	elseif u > hi then
		return hi
	end
	return u
end

--- Whether some buffered read lies more than core.TIME_EPS of rail distance off where the timing model from e to c puts
--- it. Reads carry u, the rail distance the leading joint has gone (an odometer), v, the speed, and t, the tick.
function core.off_timing(e, buf, c)
	local L, T, eu = c.u - e.u, c.t - e.t, e.u
	local va, vb, et, eps = e.v, c.v, e.t, core.TIME_EPS
	if T > 0 and not (L > 0 and ((va <= 0 and vb > 0 and 2 * L < vb * T) or (vb <= 0 and va > 0 and 2 * L < va * T))) then
		-- core.timing_u's cubic Hermite, as a polynomial in tau set up once (this runs for every buffered read at every
		-- train read): u = tau (va + tau (c2 + tau c3)), held within [0, L]
		local c2 = (3 * L / T - 2 * va - vb) / T
		local c3 = (va + vb - 2 * L / T) / (T * T)
		local lo, hi = 0, L
		if L < 0 then
			lo, hi = L, 0
		end
		for i = 1, #buf do
			local q = buf[i]
			local tau = q.t - et
			local u = tau * (va + tau * (c2 + tau * c3))
			if u < lo then
				u = lo
			elseif u > hi then
				u = hi
			end
			local d = q.u - eu - u
			if d > eps or d < -eps then
				return true
			end
		end
		return false
	end
	local timing = core.timing_u
	for i = 1, #buf do
		local q = buf[i]
		local d = q.u - eu - timing(L, T, va, vb, q.t - et)
		if d > eps or d < -eps then
			return true
		end
	end
	return false
end

--- One read r = { x, y, t, h, si, moving, fwd, v, u, gap, full, changed } of train m. Returns the emits: a list of
--- { read, why } in order, where why is "first", "change" (r.changed: the train changed since the last read, a join or a
--- split), "stop", "start", "es" (leading end switched), "surface", "gap" (the leading joint entered a curved piece that
--- does not continue the rail path recorded since the last sample: one was missed), "heartbeat" (also when r.full:
--- core.RP_MAX pieces wait to be sent) or "vertex" (a past read sent late; it carries its own tick). core.with_car tells which carry the carriage geometry,
--- core.run_start which begin a new rail path (PROTOCOL.md 3.1 `ru`). m keeps the last sent read (m.e), the reads since
--- (m.buf) and the moving and leading-end state.
---
--- The rail path from the last sample is known exactly (the pieces in `rp`), so only the timing needs samples: the reads
--- since the last sample must all lie within core.TIME_EPS of rail distance of where the timing model from that sample to
--- the current read puts them. When one does not, the previous read is sent as a vertex and becomes the new start (every
--- buffered read fitted the model to it when it was read). A full buffer keeps every other read.
function core.train_read(m, r)
	local out = {}
	local e = m.e
	local why
	if e == nil then
		why = "first"
	elseif r.si ~= e.si then
		why = "surface"
	elseif r.changed then
		why = "change"
	elseif not r.moving then
		if m.moving then
			why = "stop"
		elseif r.t - e.t >= core.HEARTBEAT then
			why = "heartbeat" -- only for a caller that reads a parked train: the movers step does not (PROTOCOL.md 3.1)
		end
	elseif m.fwd ~= nil and r.fwd ~= m.fwd then
		why = "es" -- also when it starts off the other way after a stop
	elseif not m.moving then
		why = "start"
	elseif r.gap then
		why = "gap"
	elseif r.full or r.t - e.t >= core.HEARTBEAT then
		why = "heartbeat"
	end
	local buf = m.buf or {}
	if why ~= "first" and why ~= "surface" and #buf > 0 then
		-- before an end switch, a gap or a changed train the last read before it is always sent: the new end is a train
		-- length away, the path after a gap is not known, a changed train's leading joint may have jumped
		if why == "es" or why == "gap" or why == "change" or core.off_timing(e, buf, r) then
			local v = buf[#buf]
			out[#out + 1] = { read = v, why = "vertex" }
			e, buf = v, {}
		end
	end
	if why == nil and r.moving and #buf >= core.BUF_MAX then
		local kept = {}
		for i = 2, #buf, 2 do
			kept[#kept + 1] = buf[i]
		end
		buf = kept
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

--- Whether an emit's why begins a new rail path: its sample carries `ru` and the pieces under the whole train.
function core.run_start(why)
	return why == "first" or why == "surface" or why == "es" or why == "gap" or why == "change"
end

--- Whether an emit's why sends the carriage geometry: on first sight, a surface change and a changed train (also the
--- next sample after a crossing, movers.lua's m.reshape). The rail path places every carriage at stops, starts and end
--- switches (RAIL-GEOMETRY.md 8); `car` gives the viewer the carriages' pitches.
function core.with_car(why)
	return why == "first" or why == "surface" or why == "change"
end

local function same_piece(a, b)
	return a[1] == b[1] and a[2] == b[2] and a[3] == b[3] and a[4] == b[4]
end

--- The pieces a stop or start sample lists (PROTOCOL.md 3.1 `rp`): `sent`, the pieces entered since the last sample (travel
--- order, or nil), with the pieces under the train (`under`, rear first, or nil) that came before them added in front and
--- any after them (a piece under the train's end that no read looked at) behind, so a parked train's last sample alone
--- describes its whole body (a keyframe or the live payload holds only that row). Entered pieces that left the train's
--- rear stay. At most max pieces besides the entered ones: pieces under the train nearest its rear are dropped first.
--- Returns nil for none.
function core.stop_pieces(sent, under, max)
	if not under or #under == 0 then
		return sent
	end
	sent = sent or {}
	-- under[a..b] are entered pieces; with none, every piece under the train comes after them
	local a, b
	for i, q in ipairs(under) do
		for _, p in ipairs(sent) do
			if same_piece(p, q) then
				a, b = a or i, i
				break
			end
		end
	end
	local before = a and a - 1 or 0
	local room = math.max(0, max - #sent)
	local after = math.min(#under - (b or 0), room)
	local keep = math.min(before, room - after)
	local out = {}
	for i = before - keep + 1, before do
		out[#out + 1] = under[i]
	end
	for _, p in ipairs(sent) do
		out[#out + 1] = p
	end
	for i = #under - after + 1, #under do
		out[#out + 1] = under[i]
	end
	return #out > 0 and out or nil
end

--- How many rail pieces under the train a run start lists: at most core.RP_START_MAX, and with `car` of n carriages
--- what fits beside it in a line (about 26 bytes a carriage, 18 a piece), at least 8.
function core.rp_start(ncar)
	if not ncar then
		return core.RP_START_MAX
	end
	local fit = math.floor((core.LIMIT - 900 - 26 * math.min(ncar, core.CAR_MAX)) / 18)
	return math.max(8, math.min(core.RP_START_MAX, fit))
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
	m.ref, m.key, m.n, m.o, m.ob = ref, key, n, nil, nil
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
		st.movers[j].changed = true -- its next read begins a new rail path (core.train_read "change")
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
	--- 2: train reads carry the odometer and speed of the timing model (core.train_read). Trains sampled by an older
	--- capture start afresh: their next read is a first sight.
	function(s)
		for _, m in pairs(s.movers or {}) do
			if m.kind == "t" then
				m.e, m.buf, m.moving, m.fwd = nil, nil, nil, nil
			end
		end
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
