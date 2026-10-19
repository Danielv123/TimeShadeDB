-- Simulation tests: the whole capture (capture/lua/capture.lua) against the fake Factorio world in fakegame.lua.
local t = require("testlib")
local json = require("json")
local fake = require("fakegame")

local MODULES = { "core", "outbox", "overlays", "terrain", "movers", "capture" }

--- A fresh capture over a fresh fake world. Returns capture, world and a log of what it sent: log.lines (envelope
--- lines), log.msgs (each message table with the tick it was flushed at in msg.__tick).
local function boot(cfg)
	for _, n in ipairs(MODULES) do
		package.loaded[n] = nil
	end
	local world = fake.install()
	local capture = require("capture")
	local log = { lines = {}, msgs = {} }
	capture.setup{
		sink = function(line)
			log.lines[#log.lines + 1] = line
		end,
		instance = function()
			return 7
		end,
		players = function()
			return world.players
		end,
	}
	capture.outbox.encode = function(m)
		local text = json.encode(m)
		m.__tick = game.tick
		log.msgs[#log.msgs + 1] = m
		return text
	end
	capture.build_events()
	local c = { forces = { "player" } }
	for k, v in pairs(cfg or {}) do
		c[k] = v
	end
	capture.config(c)
	return capture, world, log
end

local function run(capture, from, to, each)
	for tick = from, to do
		game.tick = tick
		if each then
			each(tick)
		end
		capture.on_tick({ tick = tick })
	end
end

local function fire(capture, name, e)
	e.name = defines.events[name]
	capture.events[defines.events[name]](e)
end

local function charted(capture, s, x, y)
	fire(capture, "on_chunk_charted", { surface_index = s.index, force = game.forces.player, position = { x = x, y = y } })
end

local function of_type(log, ty)
	local out = {}
	for _, m in ipairs(log.msgs) do
		if m.type == ty then
			out[#out + 1] = m
		end
	end
	return out
end

--- Every move entry for journey j (or all), with its tick (the entry's own, else the flush's).
local function entries(log, j)
	local out = {}
	for _, m in ipairs(of_type(log, "move")) do
		for _, e in ipairs(m.m) do
			if j == nil or e.j == j then
				e.__tick = e.tick or m.__tick
				e.__s = m.s
				out[#out + 1] = e
			end
		end
	end
	return out
end

local function chart(fill, n)
	return string.rep(string.char(fill % 256), n or 2048)
end

t.test("terrain: a charted chunk is read and sent with d; an unchanged re-chart is not; a changed one is, after the "
	.. "debounce", function()
	local capture, world, log = boot()
	local s, f = world.surfaces.nauvis, game.forces.player
	fake.chart(s, f, 0, 0, chart(1))
	fake.chart(s, f, 1, 0, chart(2))
	charted(capture, s, 0, 0)
	charted(capture, s, 1, 0)
	run(capture, 1, 1)
	local chunks = of_type(log, "chunk")
	t.expect_eq(#chunks, 2, "two chunks (CHARTS = 2 a tick)")
	t.expect_same({ chunks[1].s, chunks[1].f, chunks[1].x, chunks[1].y, chunks[1].d }, { "nauvis", "player", 0, 0,
		helpers.encode_string(chart(1)) })
	charted(capture, s, 0, 0) -- cooling since the read at 1: read again at 601, unchanged
	run(capture, 2, 1300)
	t.expect_eq(capture.terrain.state().reads, 3, "the re-chart was read when its cool-down ended")
	t.expect_eq(#of_type(log, "chunk"), 2, "unchanged: nothing sent")
	fake.chart(s, f, 0, 0, chart(3))
	charted(capture, s, 0, 0)
	run(capture, 1301, 1301)
	t.expect_eq(#of_type(log, "chunk"), 3, "changed and cooled down (601 + 600 < 1301): sent at once")
	fake.chart(s, f, 0, 0, chart(4))
	charted(capture, s, 0, 0)
	run(capture, 1302, 2000)
	local all = of_type(log, "chunk")
	t.expect_eq(#all, 4, "changed while cooling: sent when the cool-down ends")
	t.expect_eq(all[4].__tick, 1901, "at 1301 + 600")
end)

t.test("terrain: a chunk flickering every tick for 10 minutes is read at most once per 600 ticks", function()
	local capture, world, log = boot()
	local s, f = world.surfaces.nauvis, game.forces.player
	run(capture, 1, 36000, function(tick)
		fake.chart(s, f, 5, 5, chart(tick % 2))
		charted(capture, s, 5, 5)
	end)
	local st = capture.terrain.state()
	t.expect_true(st.reads <= 36000 / 600 + 1, "reads: " .. st.reads)
	t.expect_true(#of_type(log, "chunk") <= st.reads, "sent at most once a read")
	t.expect_true(capture.core.size(st.debounce.due) <= 1, "one cool-down entry")
end)

t.test("terrain: a deleted chunk is sent without d; another surface's chunk is keyed apart", function()
	local capture, world, log = boot()
	local s, f = world.surfaces.nauvis, game.forces.player
	local lab = fake.surface("lab")
	fake.chart(s, f, 0, 0, chart(1))
	fake.chart(lab, f, 0, 0, chart(1))
	charted(capture, s, 0, 0)
	charted(capture, lab, 0, 0)
	run(capture, 1, 1)
	t.expect_eq(#of_type(log, "chunk"), 2, "the same chart on two surfaces: both sent")
	fake.chart(s, f, 0, 0, nil)
	fire(capture, "on_chunk_deleted", { surface_index = s.index, positions = { { x = 0, y = 0 } } })
	run(capture, 2, 2)
	local chunks = of_type(log, "chunk")
	t.expect_eq(#chunks, 3)
	t.expect_true(chunks[3].d == nil and chunks[3].x == 0 and chunks[3].s == "nauvis", "deleted: no d")
end)

t.test("terrain: the gridworld clip keeps only the tile's chunks on tiled surfaces", function()
	local capture, world, log = boot{ tile = { world = "w", x = 0, y = 0, size = 64, surface = "nauvis" } }
	local s, f = world.surfaces.nauvis, game.forces.player
	for _, p in ipairs({ { -1, -1 }, { 0, 0 }, { 1, 0 }, { 5, 5 } }) do
		fake.chart(s, f, p[1], p[2], chart(9))
		charted(capture, s, p[1], p[2])
	end
	run(capture, 1, 5)
	local chunks = of_type(log, "chunk")
	t.expect_eq(#chunks, 2, "only (-1, -1) and (0, 0)")
end)

t.test("terrain: the last-sent cache stays bounded with 200,000 charted chunks", function()
	local capture, world, log = boot{ charts = 2000 }
	local s, f = world.surfaces.nauvis, game.forces.player
	local core = capture.core
	collectgarbage("collect")
	local before = collectgarbage("count")
	local n = 0
	run(capture, 1, 100, function()
		for _ = 1, 2000 do
			n = n + 1
			local x, y = n % 500, math.floor(n / 500)
			-- a realistic deflated size: 300 bytes, distinct per chunk
			world.charts[s.index .. ":" .. f.index .. ":" .. x .. "," .. y] = string.rep("x", 290) .. n
			charted(capture, s, x, y)
		end
		log.msgs, log.lines = {}, {}
	end)
	world.charts = {}
	run(capture, 101, 101 + capture.core.DEBOUNCE) -- let the cool-downs of the last reads end
	collectgarbage("collect")
	local grown = collectgarbage("count") - before
	local st = capture.terrain.state()
	local entries_ = core.cache_count(st.cache)
	t.expect_eq(st.reads, 200000, "every chunk read")
	t.expect_true(entries_ <= core.CACHE, "cache entries " .. entries_)
	-- what the cache itself holds: drop it and measure again
	st = nil -- it references the cache
	log.msgs = {}
	collectgarbage("collect")
	local full = collectgarbage("count")
	capture.terrain.configure{ cache = core.CACHE }
	collectgarbage("collect")
	local held = full - collectgarbage("count")
	-- an unbounded cache would hold all 200,000 charts: about 200,000 / entries_ times as much
	t.expect_true(held < 16 * 1024, string.format("the cache holds %.1f MiB", held / 1024))
	print(string.format("    cache: %d entries (cap %d) hold %.1f MiB (%.0f B/entry); Lua memory +%.1f MiB after "
		.. "200,000 chunks (Lua 5.4 in wasm)", entries_, core.CACHE, held / 1024, held * 1024 / entries_, grown / 1024))
end)

t.test("resync: storage is untouched; every charted chunk is sent forced, every mover with car; resynced last",
	function()
		local capture, world, log = boot()
		local s, f = world.surfaces.nauvis, game.forces.player
		for x = 0, 9 do
			for y = 0, 9 do
				fake.chart(s, f, x, y, chart(x + y))
			end
		end
		fake.train(s, "LCC", fake.path_lc(1000, 12), 50)
		capture.movers.seed(storage.timeshade, 0) -- already seeded by config; the train came later
		storage.timeshade.seeded = false
		capture.config({ forces = { "player" } })
		fake.vehicle(s, "S", 3, 4, 0.125)
		capture.config({ forces = { "player" } })
		local before = fake.snapshot(storage)
		t.expect_true(capture.resync("r1"))
		local r = capture.resyncing()
		for tick = 1, 200 do
			game.tick = tick
			capture.terrain.walk(storage.timeshade, r)
			capture.terrain.step(storage.timeshade, tick, r)
			capture.overlays.step()
			capture.outbox.flush(tick)
			if r.chunks_done then
				break
			end
		end
		t.expect_eq(fake.snapshot(storage), before, "the resync wrote no storage")
		t.expect_true(r.chunks_done, "the walk finished")
		t.expect_eq(r.chunks, 100)
		t.expect_eq(#of_type(log, "chunk"), 100, "every charted chunk sent")
		local moves = entries(log)
		t.expect_eq(#moves, 1, "the train; the spidertron was never seeded, so it waits for a scan")
		t.expect_eq(#moves[1].car, 3, "with every carriage")
		t.expect_true(moves[1].stop, "parked")
		run(capture, 201, 201)
		local done = of_type(log, "resynced")
		t.expect_eq(#done, 1)
		t.expect_eq(done[1].counts.chunks, 100)
		t.expect_eq(done[1].counts.movers, 1)
		t.expect_eq(of_type(log, "surfaces")[1].list[1].name, "nauvis", "surfaces first")
	end)

--- The distance from p to segment a-b.
local function seg_dist(p, a, b)
	local dx, dy = b.x - a.x, b.y - a.y
	local l2 = dx * dx + dy * dy
	local f = l2 > 0 and math.max(0, math.min(1, ((p.x - a.x) * dx + (p.y - a.y) * dy) / l2)) or 0
	local ex, ey = p.x - a.x - dx * f, p.y - a.y - dy * f
	return math.sqrt(ex * ex + ey * ey)
end

t.test("trains: samples carry the read tick; the polyline stays on the track; stop, start and end switch carry car",
	function()
		local capture, world, log = boot()
		local s = world.surfaces.nauvis
		local path = fake.path_lc(300, 12)
		local tr = fake.train(s, "LCC", path, 20)
		local truth = {}
		local speed = 0
		run(capture, 1, 1400, function(tick)
			if tick == 10 then
				speed = 0.5
			elseif tick == 1100 then
				speed = 0
			end
			tr.speed = speed
			fake.place(tr, tr.pos + speed)
			local p = tr.front_stock.position
			truth[tick] = { x = p.x, y = p.y }
		end)
		local j = storage.timeshade.by_train[tr.id]
		t.expect_true(j ~= nil and j:match("^7%.%d+%." .. tr.id .. "$"), "a journey minted by the scan: " .. tostring(j))
		local got = entries(log, j)
		t.expect_true(#got >= 5, #got .. " samples")
		local first = got[1]
		t.expect_same({ first.k, first.f, first.name, first.n, first.o, first.c }, { "t", "player", "locomotive", 3,
			"LCC", "ff0000" })
		t.expect_eq(#first.car, 3, "first sight carries every carriage")
		t.expect_eq(first.car[1][1], first.x, "car starts at the leading carriage")
		local last = got[#got]
		t.expect_true(last.stop and last.car and #last.car == 3, "the stop carries car")
		-- every sample's position is the train's position at its tick
		for _, e in ipairs(got) do
			local p = truth[e.__tick]
			t.expect_true(p and math.abs(p.x - e.x) < 1e-9 and math.abs(p.y - e.y) < 1e-9,
				"sample at its read tick " .. e.__tick)
			t.expect_true(e.h ~= nil, "a heading")
		end
		-- the polyline of samples is within a quarter tile of every tick's true position, plus the read step's sagitta
		local worst = 0
		for tick = got[1].__tick, last.__tick do
			local p = truth[tick]
			local best = math.huge
			for i = 1, #got - 1 do
				best = math.min(best, seg_dist(p, got[i], got[i + 1]))
			end
			worst = math.max(worst, best)
		end
		t.expect_true(worst <= 0.45, "worst distance to the polyline " .. worst)
		print(string.format("    trains: %d samples over %d ticks, worst %.3f tiles off the track", #got,
			last.__tick - got[1].__tick, worst))
		-- reverse: the leading end switches to the back
		run(capture, 1401, 1700, function(tick)
			tr.speed = tick < 1500 and -0.3 or 0
			fake.place(tr, tr.pos + tr.speed)
		end)
		local after = entries(log, j)
		local es
		for i = #got + 1, #after do
			if after[i].es then
				es = after[i]
			end
		end
		t.expect_true(es ~= nil, "an end switch")
		t.expect_true(es.car and #es.car == 3, "with car")
		t.expect_true(math.abs(es.x - es.car[1][1]) < 0.01 and math.abs(es.y - es.car[1][2]) < 0.01,
			"car starts at the new leading carriage")
		t.expect_eq(es.o, "ccl", "kinds from the new leading end")
		t.expect_true(after[#after].stop, "stopped again")
	end)

t.test("trains: a train destroyed sends gone; a long train's car is capped and every line stays under the limit",
	function()
		local capture, world, log = boot()
		local s = world.surfaces.nauvis
		local tr = fake.train(s, "L" .. string.rep("C", 149), fake.path_lc(5000, 12), 2000)
		run(capture, 1, 200, function()
			tr.speed = 1
			fake.place(tr, tr.pos + 1)
		end)
		local j = storage.timeshade.by_train[tr.id]
		local got = entries(log, j)
		t.expect_eq(got[1].n, 150)
		t.expect_eq(#got[1].car, capture.core.CAR_MAX, "car capped")
		t.expect_eq(#got[1].o, 150)
		for _, line in ipairs(log.lines) do
			t.expect_true(#line < 3500, "line of " .. #line)
		end
		t.expect_eq(capture.outbox.oversize(), 0, "nothing dropped")
		tr.valid = false
		fire(capture, "on_object_destroyed", { registration_number = storage.timeshade.movers[j].reg })
		run(capture, 201, 201)
		local all = entries(log, j)
		t.expect_true(all[#all].gone, "gone")
		t.expect_eq(storage.timeshade.movers[j], nil, "forgotten")
	end)

t.test("vehicles and players: dead reckoning, vehicles with heading and kind, players by name", function()
	local capture, world, log = boot()
	local s = world.surfaces.nauvis
	local car = fake.vehicle(s, "C", 0, 0, 0.25)
	local spider = fake.vehicle(s, "S", 50, 50, 0)
	local alice = fake.player("alice", s, 10, 10)
	run(capture, 1, 700, function()
		car.speed = 0.2
		car.position = { x = car.position.x + 0.2, y = 0 }
		spider.speed = 0.1
		spider.position = { x = 50, y = spider.position.y + 0.1 }
		alice.physical_position = { x = 10 + game.tick * 0.01, y = 10 }
	end)
	local cars = entries(log, storage.timeshade.by_unit[car.unit_number])
	t.expect_true(#cars >= 1, "car samples")
	t.expect_same({ cars[1].k, cars[1].o, cars[1].h, cars[1].name }, { "v", "C", 0.25, "car" })
	local spiders = entries(log, storage.timeshade.by_unit[spider.unit_number])
	t.expect_eq(spiders[1].o, "S")
	local ps = entries(log, "p:alice")
	t.expect_true(#ps >= 1, "player samples")
	t.expect_same({ ps[1].k, ps[1].name, ps[1].f }, { "p", "alice", "player" })
	fire(capture, "on_player_left_game", { player_index = alice.index })
	run(capture, 701, 701)
	ps = entries(log, "p:alice")
	t.expect_true(ps[#ps].gone, "gone when the player leaves")
end)

t.test("envelopes: v, tick and speed in every line, as JSON the server's decoder accepts", function()
	local capture, world, log = boot()
	local s, f = world.surfaces.nauvis, game.forces.player
	fake.chart(s, f, 0, 0, chart(1))
	charted(capture, s, 0, 0)
	game.speed = 2
	run(capture, 1, 1)
	t.expect_eq(#log.lines, 1)
	t.expect_eq(log.lines[1]:sub(1, 36), '{"v":1,"tick":1,"speed":2,"msgs":[{"')
	t.expect_eq(log.lines[1]:sub(-2), "]}")
end)

t.test("overlays: tags and recipes; removal through mined events; the rail layer only when enabled", function()
	local capture, world, log = boot{ rails = true }
	local s = world.surfaces.nauvis
	local tag = { tag_number = 3, surface = s, position = { x = 5, y = 6 }, icon = { type = "item", name = "iron-ore" },
		text = "ore" }
	fire(capture, "on_chart_tag_added", { force = game.forces.player, tag = tag })
	fire(capture, "on_chart_tag_removed", { force = game.forces.player, tag = tag })
	local machine = { valid = true, type = "assembling-machine", name = "assembling-machine-1", unit_number = 99,
		surface = s, force = game.forces.player, position = { x = 1.5, y = 2.5 },
		get_recipe = function() return { name = "iron-gear-wheel" }, { name = "normal" } end }
	fire(capture, "on_built_entity", { entity = machine })
	fire(capture, "on_player_mined_entity", { entity = machine })
	local rail = { valid = true, type = "straight-rail", name = "straight-rail", unit_number = 7, surface = s,
		force = game.forces.player, position = { x = 1, y = 1 }, direction = 4 }
	fire(capture, "on_built_entity", { entity = rail })
	fire(capture, "on_robot_mined_entity", { entity = rail })
	run(capture, 1, 1)
	local tags = of_type(log, "tag")
	t.expect_same({ tags[1].n, tags[1].x, tags[1].icon, tags[1].text }, { 3, 5, "item/iron-ore", "ore" })
	t.expect_true(tags[2].x == nil, "removed")
	local recipes = of_type(log, "recipe")
	t.expect_same({ recipes[1].u, recipes[1].r, recipes[1].q, recipes[1].x }, { 99, "iron-gear-wheel", "normal", 1.5 })
	t.expect_true(recipes[2].r == nil and recipes[2].u == 99, "closed when mined")
	local rails = of_type(log, "rail")
	t.expect_same({ rails[1].u, rails[1].name, rails[1].dir }, { 7, "straight-rail", 4 })
	t.expect_true(rails[2].name == nil, "removed")
	local capture2, world2, log2 = boot()
	local rail2 = { valid = true, type = "straight-rail", name = "straight-rail", unit_number = 8,
		surface = world2.surfaces.nauvis, force = game.forces.player, position = { x = 1, y = 1 }, direction = 0 }
	fire(capture2, "on_built_entity", { entity = rail2 })
	run(capture2, 1, 1)
	t.expect_eq(#of_type(log2, "rail"), 0, "rails off by default")
end)

t.test("resync: the walk keeps pace with the reads; the queue stays short with 10,000 charted chunks", function()
	local capture, world, log = boot()
	local s, f = world.surfaces.nauvis, game.forces.player
	for x = 0, 99 do
		for y = 0, 99 do
			fake.chart(s, f, x, y, chart(x * 100 + y))
		end
	end
	local core = capture.core
	capture.resync("big")
	local peak, ticks = 0, 0
	for tick = 1, 5000 do
		game.tick = tick
		capture.on_tick({ tick = tick })
		peak = math.max(peak, core.size(capture.terrain.state().debounce.queue))
		log.msgs, log.lines = {}, {}
		ticks = tick
		if not capture.resyncing() then
			break
		end
	end
	t.expect_true(capture.resyncing() == nil, "finished")
	t.expect_eq(capture.terrain.state().reads, 10000)
	t.expect_true(peak <= core.RESYNC_QUEUE + core.RESYNC_CHUNKS, "queue peak " .. peak)
	t.expect_true(ticks <= 10000 / core.RESYNC_CHARTS + 50, "ticks " .. ticks)
	print(string.format("    resync of 10,000 chunks: %d ticks, queue peak %d", ticks, peak))
end)

t.test("terrain: an unchanged re-read at the end of a cool-down skips encode_string; raw charts stay bounded", function()
	local capture, world, log = boot()
	local s, f = world.surfaces.nauvis, game.forces.player
	local core = capture.core
	-- 50 chunks charted every tick for 6,000 ticks, unchanged
	for x = 0, 49 do
		fake.chart(s, f, x, 0, chart(x))
	end
	local peak = 0
	run(capture, 1, 6000, function()
		for x = 0, 49 do
			charted(capture, s, x, 0)
		end
		local n = 0
		for _ in pairs(capture.terrain.state().debounce.raw) do
			n = n + 1
		end
		peak = math.max(peak, n)
	end)
	local st = capture.terrain.state()
	t.expect_eq(#of_type(log, "chunk"), 50, "each chunk sent once")
	t.expect_eq(world.encodes, 50, "encoded once each: the re-reads compared raw charts")
	t.expect_true(st.skipped >= 400, "re-reads skipped: " .. st.skipped)
	t.expect_true(peak <= core.CHARTS * core.DEBOUNCE, "raw charts held at most " .. peak)
	-- a change is still seen
	fake.chart(s, f, 7, 0, chart(99))
	run(capture, 6001, 6700, function()
		charted(capture, s, 7, 0)
	end)
	t.expect_eq(#of_type(log, "chunk"), 51, "the changed chunk is sent")
	-- a quiet chunk's raw chart is dropped when its cool-down ends
	run(capture, 6701, 8000)
	t.expect_eq(next(capture.terrain.state().debounce.raw), nil, "no raw chart left")
end)
