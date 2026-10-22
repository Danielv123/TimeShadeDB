-- SPDX-License-Identifier: BlueOak-1.0.0
-- Partly ported from illisis/minimap test/unit/capture_core_test.lua (Blue Oak Model License 1.0.0, see
-- capture/README.md). Unit tests of capture/lua/core.lua.
local t = require("testlib")
local core = require("core")

local function state()
	return { movers = {}, by_train = {}, by_unit = {}, reg = {} }
end
--- A register() that hands out 1, 2, 3, ... per new handle, the same number again for the same handle.
local function registrar()
	local n, got, by = 0, {}, {}
	return function(ref)
		if not by[ref] then
			n = n + 1
			by[ref], got[n] = n, ref
		end
		return by[ref]
	end, got
end
local function regs(st)
	local n = 0
	for _ in pairs(st.reg) do
		n = n + 1
	end
	return n
end

t.test("flip_kinds: a train read from its other end: the order reversed, each carriage's case swapped", function()
	t.expect_eq(core.flip_kinds("LCf"), "Fcl")
	t.expect_eq(core.flip_kinds("LCCl"), "Lccl")
	t.expect_eq(core.flip_kinds(""), "")
end)

t.test("bounds and the clip: [x1, x2) per axis in chunks, only on the tile's surface and planets", function()
	local tile = { world = "w", x = 0, y = 0, size = 64, surface = "nauvis" }
	t.expect_same(core.bounds(tile), { x1 = -1, y1 = -1, x2 = 1, y2 = 1 })
	local b = core.bounds(tile)
	t.expect_true(core.captured(tile, b, "nauvis", -1, -1), "the low corner is in")
	t.expect_true(core.captured(tile, b, "nauvis", 0, 0), "the high corner is in")
	t.expect_true(not core.captured(tile, b, "nauvis", 1, 0), "x2 is out")
	t.expect_true(not core.captured(tile, b, "nauvis", 0, 1), "y2 is out")
	t.expect_true(core.captured(tile, b, "lab", 50, 50), "a surface that is no planet is captured whole")
	local west = { world = "w", x = -1, y = 2, size = 512, surface = "nauvis", exclude = { "aquilo" } }
	t.expect_same(core.bounds(west), { x1 = -24, y1 = 24, x2 = -8, y2 = 40 })
	local bw = core.bounds(west)
	t.expect_true(core.captured(west, bw, "vulcanus", -24, 24, "vulcanus"), "a tiled planet inside the bounds")
	t.expect_true(not core.captured(west, bw, "vulcanus", 0, 0, "vulcanus"), "a tiled planet outside them")
	t.expect_true(core.captured(west, bw, "aquilo", 0, 0, "aquilo"), "an excluded planet is captured whole")
	t.expect_eq(core.bounds(nil), nil, "no tile, no bounds")
	t.expect_true(core.captured(nil, nil, "nauvis", 1000000, -1000000), "no config: no clipping")
end)

t.test("chunk keys differ per surface and force", function()
	local seen = {}
	for _, k in ipairs({ { 1, 1, -3, 4 }, { 1, 1, 4, -3 }, { 2, 1, -3, 4 }, { 1, 2, -3, 4 }, { 1, 1, 31250, -31250 },
		{ 1, 1, -31250, 31250 }, { 8191, 63, 31250, 31250 }, { 1, 1, 1, 23 }, { 1, 1, 12, 3 } }) do
		local key = core.key(k[1], k[2], k[3], k[4])
		t.expect_true(not seen[key], "distinct: " .. table.concat(k, ","))
		seen[key] = true
		t.expect_eq(core.key_surface(key), k[1], "the surface back from the key")
		t.expect_true(key < 2 ^ 53, "exact in a double")
	end
end)

t.test("the FIFO: order kept; the cap refuses event pushes; a push without a cap passes it", function()
	local q = core.queue()
	t.expect_eq(core.pop(q), nil, "empty")
	t.expect_true(core.push(q, "a", 3) and core.push(q, "b", 3) and core.push(q, "c", 3), "three fit")
	t.expect_true(not core.push(q, "d", 3), "the fourth event push is refused")
	t.expect_true(core.push(q, "forced"), "a resync push is exempt")
	t.expect_eq(core.peek(q), "a")
	for _, want in ipairs({ "a", "b", "c", "forced" }) do
		t.expect_eq(core.pop(q), want)
	end
	t.expect_same(q, { head = 1, tail = 0 }, "an empty queue resets its indexes")
end)

t.test("the ring: order kept across the wrap and when a full ring doubles; its slots stay within 1..cap", function()
	local q = core.queue(3)
	t.expect_eq(core.pop(q), nil, "empty")
	t.expect_eq(core.peek(q), nil, "nothing to peek")
	local want, next_in = {}, 1
	for round = 1, 10 do
		for _ = 1, 3 do
			t.expect_true(core.push(q, next_in), "a ring takes every item")
			want[#want + 1] = next_in
			next_in = next_in + 1
		end
		t.expect_eq(core.size(q), #want, "round " .. round)
		for _ = 1, round % 3 + 1 do
			t.expect_eq(core.peek(q), want[1])
			t.expect_eq(core.pop(q), table.remove(want, 1))
		end
	end
	t.expect_true(q.cap > 3, "it grew: cap " .. q.cap)
	for k in pairs(q) do
		t.expect_true(type(k) == "string" or (k >= 1 and k <= q.cap), "slot " .. tostring(k))
	end
	while #want > 0 do
		t.expect_eq(core.pop(q), table.remove(want, 1))
	end
	t.expect_eq(core.pop(q), nil)
	t.expect_eq(core.size(q), 0)
end)

t.test("chunk keys come back as surface, force and position", function()
	for _, k in ipairs({ { 1, 1, -3, 4 }, { 1, 0, 0, 0 }, { 8191, 63, 31250, -31250 }, { 7, 63, -31250, 31250 },
		{ 2, 5, -1, -1 } }) do
		local si, fi, x, y = core.unkey(core.key(k[1], k[2], k[3], k[4]))
		t.expect_same({ si, fi, x, y }, k, "unkey of " .. table.concat(k, ","))
		t.expect_eq(string.format("%d,%d", x, y), k[3] .. "," .. k[4], "integers")
	end
end)

t.test("chart cache: at most cap entries, recent keys survive, old hits are promoted", function()
	local c = core.cache(4)
	for i = 1, 100 do
		core.cache_set(c, "k" .. i, i)
		t.expect_true(core.cache_count(c) <= 4, "bounded at " .. i)
	end
	t.expect_eq(core.cache_get(c, "k100"), 100, "the newest is kept")
	t.expect_eq(core.cache_get(c, "k99"), 99)
	t.expect_eq(core.cache_get(c, "k1"), nil, "the oldest is gone")
	c = core.cache(4)
	core.cache_set(c, "a", 1)
	core.cache_set(c, "b", 2)
	core.cache_set(c, "c", 3) -- young {c}, old {a, b}
	t.expect_eq(core.cache_get(c, "a"), 1, "an old hit")
	core.cache_set(c, "d", 4) -- a was promoted, so young {c, a} is full: d starts a new generation with old {c, a}
	t.expect_eq(core.cache_get(c, "a"), 1, "the promoted key survived a generation")
	t.expect_eq(core.cache_get(c, "b"), nil, "the unused one did not")
	core.cache_set(c, "x", false)
	t.expect_eq(core.cache_get(c, "x"), false, "false is a value (a deleted chunk)")
	core.cache_del(c, "x")
	t.expect_eq(core.cache_get(c, "x"), nil)
end)

t.test("debounce: a chunk is read at most once per DEBOUNCE ticks; one charted while cooling is read when it ends",
	function()
		local d = core.debounce()
		local key = core.key(1, 1, 0, 0)
		t.expect_eq(core.charted(d, key), "queued")
		t.expect_eq(core.charted(d, key), "waiting", "already queued")
		local got, forced = core.take(d, 100)
		t.expect_eq(got, key, "read at 100")
		t.expect_eq(forced, false, "from the event queue")
		t.expect_eq(core.charted(d, key), "dirty", "charted while cooling")
		t.expect_eq(core.charted(d, key), "dirty")
		t.expect_eq(core.take(d, 101), nil, "nothing queued while it cools")
		t.expect_eq(core.cooled(d, 100 + core.DEBOUNCE - 1), 0, "still cooling")
		t.expect_eq(core.cooled(d, 100 + core.DEBOUNCE), 1, "the cool-down ends: queued again")
		t.expect_eq(core.take(d, 100 + core.DEBOUNCE), key)
		t.expect_eq(core.cooled(d, 100 + 2 * core.DEBOUNCE), 0, "not charted since: nothing to read")
		t.expect_eq(next(d.cool), nil, "no cool-down left")
		t.expect_eq(next(d.dirty), nil, "nothing dirty")
		t.expect_eq(core.size(d.due) + core.size(d.due_t), 0, "the cool-down list is empty")
		t.expect_eq(core.charted(d, key), "queued", "a quiet chunk is queued at once")
	end)

t.test("debounce: forced pushes skip the cool-down; the event queue grows past its ring instead of dropping", function()
	local cap = core.CAP
	core.CAP = 2
	local d = core.debounce()
	core.CAP = cap
	t.expect_eq(core.charted(d, "a"), "queued")
	t.expect_eq(core.charted(d, "b"), "queued")
	t.expect_eq(core.charted(d, "x"), "queued", "past the ring's first size")
	t.expect_eq(d.drops, 0)
	t.expect_eq(core.charted(d, "c", true), "queued", "forced")
	local key, forced = core.take(d, 0)
	t.expect_eq(key, "c", "forced reads first")
	t.expect_eq(forced, true)
	t.expect_eq(core.charted(d, "c", true), "queued", "forced past the cool-down")
	local sentinel = { resynced = 1 }
	t.expect_eq(core.charted(d, sentinel, true), "queued", "the sentinel")
	t.expect_eq(core.take(d, 1, "forced"), "c")
	t.expect_eq(core.take(d, 1, "forced"), sentinel, "the sentinel comes back as it went in")
	t.expect_eq(core.take(d, 1, "forced"), nil, "only forced reads asked for")
	t.expect_eq(core.take(d, 1, "events"), "a")
end)

t.test("debounce: a chunk flickering every tick for an hour is read at most once per DEBOUNCE", function()
	local d = core.debounce()
	local key, n = "hot", 0
	for tick = 0, 216000 do
		core.charted(d, key)
		core.cooled(d, tick)
		if core.take(d, tick) then
			n = n + 1
		end
	end
	t.expect_eq(n, 216000 / core.DEBOUNCE + 1, "reads in an hour")
	t.expect_true(core.size(d.due) <= 1, "the cool-down list holds one entry")
end)

t.test("dead reckoning (vehicles, players): first poll records, sends past EPS on either axis and at the heartbeat",
	function()
		local m = {}
		t.expect_true(not core.poll(m, 0, 0, 1, 0), "first sight only records")
		t.expect_true(core.poll(m, 3, 0, 1, 30), "the second poll sends")
		t.expect_eq(m.vx, 6, "3 tiles in 30 ticks is 6 tiles a second")
		for tick = 60, 600, 30 do
			t.expect_true(not core.poll(m, tick / 10, 0, 1, tick), "on the prediction at " .. tick)
		end
		t.expect_true(core.poll(m, 63, 0, 1, 630), "a heartbeat")
		t.expect_true(not core.poll(m, 66 + 0.9, 0, 1, 660), "0.9 tiles off: within EPS")
		t.expect_true(core.poll(m, 69 + 1.1, 0, 1, 690), "1.1 tiles off: sent")
		t.expect_true(core.poll(m, 70, 0, 2, 691), "a new surface sends")
		t.expect_same({ m.vx, m.vy }, { 0, 0 }, "no velocity across surfaces")
		t.expect_true(core.poll(m, 70, 0, 2, 700, "stop"), "stop sends")
	end)

t.test("journey ids: <instance>.<tick>.<train id>, v<unit> for a vehicle, integers printed as such", function()
	t.expect_eq(core.mint(4242, 600, 32, "t"), "4242.600.32")
	t.expect_eq(core.mint(4242, 600, 750, "v"), "4242.600.v750")
	t.expect_eq(core.mint(0, 1200.0, 7.0, "t"), "0.1200.7")
end)

t.test("joins and splits: one journey through old_train_id_2 joins; a merge ends the other; a split's second waits",
	function()
		local st, register, got = state(), registrar()
		core.claim(st, "j", "t", 55, { id = 55 }, 12, register)
		local prev = 55
		for _, id in ipairs({ 72, 74, 76, 78 }) do
			core.train_created(st, id - 1, prev, id, { id = id }, 12 + (id - 70) / 2, register)
			prev = id
		end
		t.expect_eq(st.by_train[78], "j", "78 carries the journey")
		for _, id in ipairs({ 55, 72, 74, 76 }) do
			t.expect_eq(st.by_train[id], nil, "the old id " .. id .. " is cleared")
		end
		t.expect_eq(regs(st), 1, "only the current train is registered")
		t.expect_eq(got[st.movers.j.reg].id, 78)
		t.expect_true(st.movers.j.reshape, "a changed train sends its carriages next")
		local s2, reg2 = state(), registrar()
		core.claim(s2, "a", "t", 10, { id = 10 }, 1, reg2)
		core.claim(s2, "b", "t", 11, { id = 11 }, 1, reg2)
		t.expect_eq(core.train_created(s2, 10, 11, 12, { id = 12 }, 2, reg2), "b", "b ends")
		core.forget(s2, "b")
		t.expect_eq(regs(s2), 1)
		core.claim(s2, "c", "t", 20, { id = 20 }, 2, reg2)
		core.train_created(s2, 20, nil, 21, { id = 21 }, 1, reg2)
		t.expect_eq(core.train_created(s2, 20, nil, 22, { id = 22 }, 1, reg2), nil)
		t.expect_eq(s2.by_train[21], "c", "the first half takes the journey")
		t.expect_eq(s2.by_train[22], nil, "the second waits for a scan")
	end)

t.test("adoption returns the displaced journey and resets the mover's sampling state", function()
	local st, register = state(), registrar()
	local ref = { id = 40 }
	core.claim(st, "x", "t", 40, ref, 1, register)
	core.train_read(st.movers.x, { x = 0, y = 0, t = 0, h = 0.25, si = 1, moving = true, fwd = true, v = 0, u = 0 })
	t.expect_eq(core.adopt(st, "y", "t", 40, ref, 3, register), "x", "x is displaced")
	core.forget(st, "x")
	t.expect_eq(st.by_train[40], "y")
	t.expect_eq(st.reg[st.movers.y.reg], "y")
	t.expect_true(st.movers.y.e == nil and st.movers.y.moving == nil, "y is sampled afresh")
	t.expect_eq(core.adopt(st, "y", "t", 40, ref, 3, register), nil, "re-adopting y displaces nothing")
end)

--- The parts of each line, checking its envelope head and tail.
local function parts_of(lines, tick, speed)
	local head, out = core.head(tick, speed), {}
	for _, line in ipairs(lines) do
		t.expect_eq(string.sub(line, 1, #head), head, "the envelope's head")
		t.expect_eq(string.sub(line, -2), "]}", "the envelope's tail")
		out[#out + 1] = string.sub(line, #head + 1, -3)
	end
	return table.concat(out, ",")
end

t.test("envelope head: v, tick and speed only when it is not 1", function()
	t.expect_eq(core.head(5000000), '{"v":1,"tick":5000000,"msgs":[')
	t.expect_eq(core.head(7, 1), '{"v":1,"tick":7,"msgs":[')
	t.expect_eq(core.head(7, 0.5), '{"v":1,"tick":7,"speed":0.5,"msgs":[')
end)

t.test("the cut: every line is under 3,500 bytes, a 10-digit tick and a speed included; parts kept in order", function()
	local chunk = '{"d":"' .. string.rep("A", 2750) .. '","f":"player","s":"nauvis","type":"chunk","x":-12,"y":7}'
	local sample = '{"f":"player","j":"4242.1332.32","k":"t","n":16,"name":"locomotive","x":20001.5,"y":-2.25}'
	local parts = { chunk }
	for i = 1, 40 do
		parts[#parts + 1] = sample
		if i % 13 == 0 then
			parts[#parts + 1] = chunk
		end
	end
	for _, tick in ipairs({ 0, 4294967295 }) do
		local lines, dropped = core.cut(tick, parts, core.LIMIT, 0.25)
		t.expect_eq(dropped, 0)
		for _, line in ipairs(lines) do
			t.expect_true(#line < 3500, "a line of " .. #line .. " bytes")
		end
		t.expect_eq(parts_of(lines, tick, 0.25), table.concat(parts, ","), "every part, once, in order")
	end
	t.expect_eq(#core.cut(7, {}, core.LIMIT), 0, "nothing to print")
end)

t.test("the cut: a part filling a line to 3,499 bytes is kept; one byte more is dropped and counted", function()
	local head = core.head(4294967295)
	local fits = string.rep("x", 3499 - #head - 2)
	local lines, dropped = core.cut(4294967295, { "a", fits, "b" }, core.LIMIT)
	t.expect_eq(dropped, 0)
	t.expect_eq(#lines, 3, "a line each: the big part fits only alone")
	t.expect_eq(#lines[2], 3499)
	lines, dropped = core.cut(4294967295, { "a", fits .. "x", "b" }, core.LIMIT)
	t.expect_eq(dropped, 1, "the oversized part is dropped and counted")
	t.expect_eq(parts_of(lines, 4294967295), "a,b")
end)

t.test("migrations: a new table gets every schema; it is never wiped; a newer schema is refused", function()
	local s = core.migrate({})
	t.expect_eq(s.schema, #core.MIGRATIONS)
	t.expect_true(s.configured == false and s.seeded == false and s.wi == 1 and s.next_scan == 0 and s.file_seq == 0)
	for _, k in ipairs({ "forces", "movers", "by_train", "by_unit", "walk", "reg", "wheel", "fast" }) do
		t.expect_same(s[k], {}, k)
	end
	s.marker = "kept"
	core.migrate(s)
	t.expect_eq(s.marker, "kept")
	local newer = { schema = #core.MIGRATIONS + 1, marker = "untouched" }
	t.expect_error(function() core.migrate(newer) end, "is newer than this code")
	t.expect_eq(newer.marker, "untouched")
end)

t.test("a train keeps the end it last led with while stopped, and starts on its front", function()
	t.expect_true(core.forward(nil, 0))
	t.expect_true(not core.forward(nil, -0.3))
	t.expect_true(not core.forward(false, 0), "stopped after backing: still the back")
	t.expect_true(core.forward(true, 0))
end)

t.test("heading: from the second carriage towards the leading one, or the carriage's orientation", function()
	t.expect_eq(core.heading(10, 0, 3, 0), 0.25, "east")
	t.expect_eq(core.heading(0, 10, 0, 3), 0.5, "south")
	t.expect_eq(core.heading(0, 0, 7, 0), 0.75, "west")
	t.expect_eq(core.heading(0, 0, 0, 7), 0, "north")
	t.expect_eq(core.heading(1, 1, 0, 2), 0.125, "north-east, 3 decimals")
	t.expect_eq(core.heading(0, 0, nil, nil, 0.25, true), 0.75, "leading with its back: half a turn on")
	t.expect_eq(core.heading(0, 0, nil, nil, 0.9996, false), 0, "rounded, then wrapped: never 1")
	t.expect_near(core.turn(0.99, 0.01), 0.02, 1e-9, "turn wraps")
end)

t.test("next_read: the ticks a train takes to move STEP tiles, between 1 and SLOW, sooner while it accelerates",
	function()
		t.expect_eq(core.STEP, 5)
		t.expect_true(core.STEP < 5.0779, "less than the shortest curved piece (curved-rail-b)")
		t.expect_eq(core.next_read(1.2), 4, "never more than STEP tiles")
		t.expect_eq(core.next_read(1.3889), 3, "top speed: 4.17 tiles")
		t.expect_eq(core.next_read(-1), 5)
		t.expect_eq(core.next_read(6), 1)
		t.expect_eq(core.next_read(0.001), core.SLOW)
		t.expect_eq(core.SLOW, 30)
		-- from 0.05 tiles a tick at 0.01 a tick per tick: 0.05 w + 0.005 w^2 = 5 at w = 27.0
		t.expect_eq(core.next_read(0.05, 0.01), 27)
		t.expect_eq(core.next_read(0.5, -0.01), 10, "braking: the current speed")
		t.expect_true(core.WHEEL > core.SLOW)
	end)

--- A read of a train on a straight along x: rail distance u (the odometer), tick, speed v.
local function read(u, tick, v, moving, fwd, gap)
	return { x = u, y = 0, t = tick, h = 0.25, si = 1, moving = moving ~= false, fwd = fwd ~= false, v = v or 0,
		u = u, gap = gap }
end
local function whys(out)
	local w = {}
	for i, e in ipairs(out) do
		w[i] = e.why
	end
	return table.concat(w, ",")
end

t.test("train emits: first, then at constant speed only on the heartbeat; a full buffer keeps every other read",
	function()
		local m = {}
		t.expect_eq(whys(core.train_read(m, read(0, 0, 0.4))), "first")
		local sent = {}
		for i = 1, 100 do
			local out = core.train_read(m, read(4 * i, 10 * i, 0.4))
			for _, e in ipairs(out) do
				sent[#sent + 1] = e.why .. "@" .. e.read.t
			end
			t.expect_true(#m.buf <= core.BUF_MAX, "the buffer stays bounded")
		end
		t.expect_eq(table.concat(sent, " "), "heartbeat@600", "the timing model holds: nothing but the heartbeat")
	end)

t.test("train emits: uniform acceleration fits the timing model; a change of acceleration sends a vertex", function()
	local m = {}
	-- from 0.2 tiles a tick at 0.005 a tick per tick for 60 ticks: the model is exact
	core.train_read(m, read(0, 0, 0.2))
	for k = 1, 6 do
		local tau = 10 * k
		t.expect_eq(whys(core.train_read(m, read(0.2 * tau + 0.0025 * tau * tau, tau, 0.2 + 0.005 * tau))), "")
	end
	-- then it cruises at 0.5: the reads after t = 60 no longer fit one cubic from t = 0
	local u60, out = 0.2 * 60 + 0.0025 * 3600, nil
	local sent = {}
	for k = 1, 12 do
		out = core.train_read(m, read(u60 + 0.5 * 10 * k, 60 + 10 * k, 0.5))
		for _, e in ipairs(out) do
			sent[#sent + 1] = e.why .. "@" .. e.read.t
		end
	end
	t.expect_true(#sent >= 1 and sent[1]:match("^vertex@"), "a vertex: " .. table.concat(sent, " "))
	local vt = tonumber(sent[1]:match("@(%d+)"))
	t.expect_true(vt >= 60 and vt <= 90, "near the change, at " .. vt)
end)

t.test("train emits: a stop read late is braking that ended early; stop, start and end switch send", function()
	local m = {}
	core.train_read(m, read(0, 0, 0.3))
	-- brakes uniformly from 0.3 to rest in 60 ticks (9 tiles), read at 20 and 40; the stop read at 75
	t.expect_eq(whys(core.train_read(m, read(0.3 * 20 - 0.0025 * 400, 20, 0.25))), "")
	t.expect_eq(whys(core.train_read(m, read(0.3 * 40 - 0.0025 * 1600, 40, 0.2 - 0.1))), "")
	t.expect_eq(whys(core.train_read(m, read(9, 75, 0, false))), "stop", "one sample: the model stops early")
	t.expect_eq(whys(core.train_read(m, read(9, 90, 0, false))), "", "still at rest")
	t.expect_eq(whys(core.train_read(m, read(9.5, 130, 0.1, true))), "start", "moving again")
	t.expect_eq(whys(core.train_read(m, read(12, 150, 0.15, true))), "")
	local out = core.train_read(m, read(-5, 160, 0.2, true, false))
	t.expect_eq(whys(out), "vertex,es", "the old end's last read, then the switch")
	t.expect_true(core.with_car("first") and core.with_car("surface"))
	t.expect_true(not core.with_car("vertex") and not core.with_car("gap") and not core.with_car("heartbeat")
		and not core.with_car("stop") and not core.with_car("start") and not core.with_car("es"))
	t.expect_eq(core.rp_start(nil), core.RP_START_MAX)
	t.expect_eq(core.rp_start(3), core.RP_START_MAX)
	t.expect_eq(core.rp_start(108), 8, "a long train's car leaves little room")
	t.expect_true(core.run_start("first") and core.run_start("es") and core.run_start("gap"))
	t.expect_true(not core.run_start("stop") and not core.run_start("start") and not core.run_start("vertex"))
	t.expect_eq(whys(core.train_read(m, read(-5, 700, 0, false, false))), "stop")
	t.expect_eq(whys(core.train_read(m, read(-5, 1300, 0, false, false))), "heartbeat", "a parked train's heartbeat")
end)

t.test("train emits: a gap sends the last read before it, then the read that found it", function()
	local m = {}
	core.train_read(m, read(0, 0, 0.4))
	core.train_read(m, read(4, 10, 0.4))
	t.expect_eq(whys(core.train_read(m, read(8, 20, 0.4, true, true, true))), "vertex,gap")
	local full = read(12, 30, 0.4)
	full.full = true
	t.expect_eq(whys(core.train_read(m, full)), "heartbeat", "RP_MAX pieces waiting send a sample")
end)

t.test("train emits: a changed train sends the last read before it, then a new rail path with car", function()
	local m = {}
	core.train_read(m, read(0, 0, 0.4))
	core.train_read(m, read(4, 10, 0.4))
	local r = read(-3, 20, 0.4) -- a split took the front carriage: the leading joint jumped back
	r.changed = true
	local out = core.train_read(m, r)
	t.expect_eq(whys(out), "vertex,change")
	t.expect_true(core.run_start("change") and core.with_car("change"))
	r = read(-3, 30, 0, false)
	r.changed = true
	t.expect_eq(whys(core.train_read(m, r)), "change", "changed while it stands: the change, at rest")
	t.expect_eq(whys(core.train_read(m, read(-3, 60, 0.1))), "start")
	local st = state()
	local register = registrar()
	core.claim(st, "j", "t", 1, { id = 1 }, 3, register)
	st.by_train[1] = "j"
	core.train_created(st, 1, nil, 2, { id = 2 }, 2, register)
	t.expect_true(st.movers.j.changed, "train_created marks the journey's train changed")
end)

t.test("stop pieces: the pieces under the train before and after the entered ones, within the budget", function()
	local function p(i)
		return { i, 0, 4, 1 }
	end
	local function ids(list)
		local out = {}
		for i, q in ipairs(list or {}) do
			out[i] = q[1]
		end
		return table.concat(out, ",")
	end
	t.expect_eq(core.stop_pieces(nil, nil, 120), nil)
	t.expect_eq(ids(core.stop_pieces({ p(1) }, nil, 120)), "1")
	t.expect_eq(ids(core.stop_pieces(nil, { p(1), p(2) }, 120)), "1,2", "none entered: every piece under the train")
	t.expect_eq(ids(core.stop_pieces({ p(3), p(4) }, { p(1), p(2), p(3), p(4) }, 120)), "1,2,3,4")
	t.expect_eq(ids(core.stop_pieces({ p(2), p(3) }, { p(3), p(4) }, 120)), "2,3,4",
		"an entered piece that left the rear stays; one under the end no read looked at follows")
	t.expect_eq(ids(core.stop_pieces({ p(3), p(4) }, { p(1), p(2), p(3), p(4) }, 3)), "2,3,4", "the rear dropped first")
	t.expect_eq(ids(core.stop_pieces({ p(3), p(4) }, { p(1), p(2), p(3), p(4) }, 1)), "3,4", "entered pieces are kept")
	t.expect_eq(ids(core.stop_pieces({ p(1) }, { p(5), p(6) }, 120)), "1,5,6", "nothing in common: entered first")
end)

t.test("timing model: braking, starting and cruising as RAIL-GEOMETRY.md 8", function()
	t.expect_near(core.timing_u(30, 60, 0.5, 0.5, 30), 15, 1e-12)
	t.expect_eq(core.timing_u(2, 100, 0, 0.2, 50), 0, "a late start stands first")
	t.expect_near(core.timing_u(2, 100, 0, 0.2, 90), 0.5, 1e-12)
	t.expect_near(core.timing_u(3, 100, 0.2, 0, 30), 3, 1e-12, "a late stop stopped early")
	t.expect_eq(core.timing_u(5, 0, 1, 1, 0), 5)
end)

-- The shared rail geometry vectors (docs/v3/RAIL-GEOMETRY.md 7; run_unit.mjs passes them as __vectors).
local KIND = { ["curved-rail-a"] = 1, ["elevated-curved-rail-a"] = 1, ["curved-rail-b"] = 2,
	["elevated-curved-rail-b"] = 2, ["legacy-curved-rail"] = 3, ["rail-ramp"] = 4 }

t.test("rail vectors: piece ends match the game's", function()
	local V = __vectors()
	local n = 0
	for _, p in ipairs(V.pieces) do
		local k = KIND[p.type]
		t.expect_eq(core.RAIL_KINDS[p.type], k, p.type)
		if k then
			local e = core.piece_ends(k, p.dir)
			t.expect_same({ p.x + e[1], p.y + e[2], e[3] }, p.front_end, p.type .. "/" .. p.dir .. " front end")
			t.expect_same({ p.x + e[4], p.y + e[5], e[6] }, p.back_end, p.type .. "/" .. p.dir .. " back end")
			n = n + 1
		end
	end
	t.expect_true(n >= 40, n .. " pieces")
end)

t.test("rail vectors: consecutive curved pieces of every recorded train path connect, run the right way round",
	function()
		local V = __vectors()
		local pairs_checked = 0
		for _, tr in ipairs(V.trains) do
			local prev -- the exit end of the previous curved piece: { x, y, outward direction }
			for _, p in ipairs(tr.path) do
				local k = KIND[p.type]
				if k then
					local e = core.piece_ends(k, p.dir)
					-- a piece run back to front (rev) is entered at its back end
					local ix, iy, id = p.x + e[1], p.y + e[2], e[3]
					local xx, xy, xd = p.x + e[4], p.y + e[5], e[6]
					if p.rev then
						ix, iy, id, xx, xy, xd = xx, xy, xd, ix, iy, id
					end
					if prev then
						t.expect_true(core.connects(prev[1], prev[2], prev[3], ix, iy, id), tr.source .. ": " .. p.type)
						t.expect_true(not core.connects(prev[1], prev[2], prev[3], xx, xy, xd),
							tr.source .. ": not the other way round")
						pairs_checked = pairs_checked + 1
					end
					prev = { xx, xy, xd }
				end
			end
		end
		t.expect_true(pairs_checked >= 10, pairs_checked .. " pairs")
		t.expect_true(not core.connects(0, 0, 4, 3, 1, 12), "off the line")
		t.expect_true(not core.connects(0, 0, 4, -3, 0, 12), "behind")
		t.expect_true(core.connects(0, 0, 5, 4, 2, 13), "two half-diagonal tiles on")
	end)

t.test("off_timing: the same verdict as core.timing_u read by read", function()
	-- reads along a train's true motion (cruising, accelerating, a late start, a late stop) and offsets around the
	-- tolerance, checked against the plain loop
	local function slow(e, buf, c)
		for _, q in ipairs(buf) do
			local d = q.u - e.u - core.timing_u(c.u - e.u, c.t - e.t, e.v, c.v, q.t - e.t)
			if math.abs(d) > core.TIME_EPS then
				return true
			end
		end
		return false
	end
	local n, off = 0, 0
	for _, case in ipairs({ { 30, 60, 0.5, 0.5 }, { 8, 40, 0, 0.4 }, { 2, 100, 0, 0.2 }, { 3, 100, 0.2, 0 },
		{ 24, 30, 1.0, 0.6 }, { 60, 120, 0.9, 0.1 }, { -4, 30, 0.1, 0.1 }, { 5, 0, 1, 1 } }) do
		local L, T, va, vb = case[1], case[2], case[3], case[4]
		for _, shift in ipairs({ -0.15, -0.099, 0, 0.099, 0.15 }) do
			for k = 1, 15 do
				local tau = T * k / 16
				local buf = { { t = 100 + tau, u = 7 + core.timing_u(L, T, va, vb, tau) + (k == 8 and shift or 0) } }
				local e, c = { t = 100, u = 7, v = va }, { t = 100 + T, u = 7 + L, v = vb }
				local want = slow(e, buf, c)
				t.expect_eq(core.off_timing(e, buf, c), want, string.format("L %g T %g k %d shift %g", L, T, k, shift))
				n, off = n + 1, off + (want and 1 or 0)
			end
		end
	end
	t.expect_true(off > 10 and off < n, off .. " of " .. n .. " off")
end)

t.test("rail vectors: the timing model", function()
	local V = __vectors()
	t.expect_true(#V.timing >= 50)
	for _, c in ipairs(V.timing) do
		local L, T, va, vb, tau, u = c[1], c[2], c[3], c[4], c[5], c[6]
		t.expect_near(core.timing_u(L, T, va, vb, tau), u, 1e-8, string.format("L %g T %g va %g vb %g tau %g", L, T, va,
			vb, tau))
	end
end)
