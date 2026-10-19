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

t.test("the ring: order kept across the wrap; full at cap; its slots stay within 1..cap", function()
	local q = core.queue(3)
	t.expect_eq(core.pop(q), nil, "empty")
	t.expect_eq(core.peek(q), nil, "nothing to peek")
	local want, next_in = {}, 1
	for round = 1, 10 do
		while core.push(q, next_in) do
			want[#want + 1] = next_in
			next_in = next_in + 1
		end
		t.expect_eq(core.size(q), 3, "full at cap (round " .. round .. ")")
		for _ = 1, round % 3 + 1 do
			t.expect_eq(core.peek(q), want[1])
			t.expect_eq(core.pop(q), table.remove(want, 1))
		end
	end
	for k in pairs(q) do
		t.expect_true(type(k) == "string" or (k >= 1 and k <= 3), "slot " .. tostring(k))
	end
	while core.pop(q) do
	end
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

t.test("debounce: forced pushes skip the cool-down and the cap; the cap drops and counts event pushes", function()
	local cap = core.CAP
	core.CAP = 2
	local d = core.debounce()
	core.CAP = cap
	t.expect_eq(core.charted(d, "a"), "queued")
	t.expect_eq(core.charted(d, "b"), "queued")
	t.expect_eq(core.charted(d, "c"), "dropped")
	t.expect_eq(d.drops, 1)
	t.expect_eq(core.charted(d, "c", true), "queued", "forced past the cap")
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
	core.train_read(st.movers.x, { x = 0, y = 0, t = 0, h = 0.25, si = 1, moving = true, fwd = true })
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

t.test("migrations: a new table becomes schema 1; it is never wiped; a newer schema is refused", function()
	local s = core.migrate({})
	t.expect_eq(s.schema, 1)
	t.expect_true(s.configured == false and s.seeded == false and s.wi == 1 and s.next_scan == 0 and s.file_seq == 0)
	for _, k in ipairs({ "forces", "movers", "by_train", "by_unit", "walk", "reg", "wheel", "fast" }) do
		t.expect_same(s[k], {}, k)
	end
	s.marker = "kept"
	core.migrate(s)
	t.expect_eq(s.marker, "kept")
	local newer = { schema = 2, marker = "untouched" }
	t.expect_error(function() core.migrate(newer) end, "schema 2 is newer")
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

t.test("next_read: the ticks a train takes to move STEP tiles, between 1 and SLOW", function()
	t.expect_eq(core.next_read(1.2), 4)
	t.expect_eq(core.next_read(-1), 4)
	t.expect_eq(core.next_read(4), 1)
	t.expect_eq(core.next_read(0.001), 120)
	t.expect_true(core.WHEEL > core.SLOW)
end)

local function read(x, y, tick, h, moving, fwd)
	return { x = x, y = y, t = tick, h = h or 0.25, si = 1, moving = moving ~= false, fwd = fwd ~= false }
end
local function whys(out)
	local w = {}
	for i, e in ipairs(out) do
		w[i] = e.why
	end
	return table.concat(w, ",")
end

t.test("train emits: first, then on a straight at constant speed only when the read buffer fills or the heartbeat",
	function()
		local m = {}
		t.expect_eq(whys(core.train_read(m, read(0, 0, 0))), "first")
		local sent = {}
		for i = 1, 100 do
			local out = core.train_read(m, read(4 * i, 0, 10 * i))
			for _, e in ipairs(out) do
				sent[#sent + 1] = e.why .. "@" .. e.read.t
			end
		end
		t.expect_eq(sent[1], "full@170", "the 17th read finds 16 buffered")
		t.expect_eq(#sent, 5, "a sample every 17 reads: " .. table.concat(sent, " "))
		local slow = {}
		core.train_read(slow, read(0, 0, 0))
		local hb
		for i = 1, 15 do
			local out = core.train_read(slow, read(0.1 * i, 0, 50 * i))
			if #out > 0 then
				hb = out[1]
			end
		end
		t.expect_true(hb and hb.why == "heartbeat" and hb.read.t == 600, "a slow train's heartbeat at 600")
	end)

t.test("train emits: a read off the chord sends the previous read as a vertex with its own tick", function()
	local m = {}
	core.train_read(m, read(0, 0, 0))
	t.expect_eq(#core.train_read(m, read(4, 0, 10)), 0)
	t.expect_eq(#core.train_read(m, read(8, 0, 20)), 0)
	-- the track bends: (8, 0) lies 0.4 tiles off the chord from (0, 0) to (12, 1.2)
	local out = core.train_read(m, read(12, 1.2, 30, 0.25))
	t.expect_eq(whys(out), "chord")
	t.expect_same({ out[1].read.x, out[1].read.y, out[1].read.t }, { 8, 0, 20 })
	t.expect_eq(m.e.t, 20, "the vertex starts the next chord")
	t.expect_eq(#m.buf, 1, "the current read is buffered")
end)

t.test("train emits: synchronised distance catches a train that brakes on a straight", function()
	local m = {}
	core.train_read(m, read(0, 0, 0))
	t.expect_eq(#core.train_read(m, read(10, 0, 10)), 0)
	t.expect_eq(#core.train_read(m, read(20, 0, 20)), 0)
	-- then it nearly stops: at t = 20 a constant speed from 0 to 21 at t = 100 would put it at 4.2, not 20
	local out = core.train_read(m, read(21, 0, 100))
	t.expect_eq(whys(out), "chord")
	t.expect_eq(out[1].read.x, 20)
end)

t.test("train emits: a heading change over 2 degrees sends; stop, start and an end switch send, the switch after the "
	.. "old end's last read", function()
	local m = {}
	core.train_read(m, read(0, 0, 0, 0.25))
	t.expect_eq(whys(core.train_read(m, read(4, 0, 10, 0.25 + 1 / 360))), "", "1 degree: nothing")
	t.expect_eq(whys(core.train_read(m, read(8, 0, 20, 0.25 + 3 / 360))), "heading", "3 degrees: sent")
	t.expect_eq(whys(core.train_read(m, read(12, 0, 30, 0.26))), "")
	-- it reached x = 12 at t = 30 and stood: that read is off the time-interpolated chord, so it is a vertex
	t.expect_eq(whys(core.train_read(m, read(12, 0, 40, 0.26, false))), "chord,stop", "stopped")
	t.expect_eq(whys(core.train_read(m, read(12, 0, 50, 0.26, false))), "", "still at rest")
	t.expect_eq(whys(core.train_read(m, read(12, 0, 60, 0.26, true))), "start", "moving again")
	t.expect_eq(whys(core.train_read(m, read(16, 0, 70, 0.26, true))), "")
	local out = core.train_read(m, read(-5, 0, 80, 0.76, true, false))
	t.expect_eq(whys(out), "chord,es", "the old end's last read, then the switch")
	t.expect_true(core.with_car("es") and core.with_car("stop") and core.with_car("start") and core.with_car("first"))
	t.expect_true(not core.with_car("chord") and not core.with_car("heading"))
	t.expect_eq(whys(core.train_read(m, read(-5, 0, 700, 0.76, false))), "stop")
	t.expect_eq(whys(core.train_read(m, read(-5, 0, 1300, 0.76, false))), "heartbeat", "a parked train's heartbeat")
end)

t.test("train emits: the read buffer never holds more than BUF_MAX reads", function()
	local m = {}
	core.train_read(m, read(0, 0, 0))
	for i = 1, 1000 do
		core.train_read(m, read(i * 4, math.sin(i) * 0.1, i * 5))
		t.expect_true(#(m.buf or {}) <= core.BUF_MAX)
	end
end)

t.test("the polyline of train samples stays within CHORD_EPS of every read on a curve", function()
	local R, m, samples, reads = 12, {}, {}, {}
	for i = 0, 60 do
		local th = i * 4 / R -- a 4-tile step along a circle of radius 12
		local r = read(R * math.sin(th), R - R * math.cos(th), i * 6, (0.25 + th / (2 * math.pi)) % 1)
		reads[#reads + 1] = r
		for _, e in ipairs(core.train_read(m, r)) do
			samples[#samples + 1] = e.read
		end
	end
	local function seg_dist(p, a, b)
		local dx, dy = b.x - a.x, b.y - a.y
		local l2 = dx * dx + dy * dy
		local f = l2 > 0 and math.max(0, math.min(1, ((p.x - a.x) * dx + (p.y - a.y) * dy) / l2)) or 0
		local ex, ey = p.x - a.x - dx * f, p.y - a.y - dy * f
		return math.sqrt(ex * ex + ey * ey)
	end
	-- the last reads after the last sample are not covered yet; the next sample would cover them
	local last = samples[#samples].t
	local worst = 0
	for _, r in ipairs(reads) do
		if r.t <= last then
			local best = math.huge
			for i = 1, #samples - 1 do
				best = math.min(best, seg_dist(r, samples[i], samples[i + 1]))
			end
			worst = math.max(worst, best)
		end
	end
	t.expect_true(worst <= core.CHORD_EPS + 1e-9, "worst " .. worst)
	-- 19 degrees a read: the heading rule sends every read on a curve this tight
	t.expect_eq(#samples, #reads, "a sample a read")
end)
