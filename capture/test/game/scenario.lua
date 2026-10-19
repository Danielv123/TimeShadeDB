-- The headless game-test scenario, shared by the standalone test driver mod and the Clusterio module harness
-- (capture/test/run_game.mjs assembles both). It builds a lab surface with rails (through the Factorio 2.0 rail planner
-- API, LuaRailEnd.get_rail_extensions), puts trains, a car and an assembler on it, drives them by tick, feeds synthetic
-- charts to the capture, and writes what it saw (true train positions, API checks) to script-output/timeshade-test/.
--
-- api: { config(c), resync(trace), synthetic(spec), charted(surface, force, list), stats(), memory() }, each optional.
local scenario = {}

local SURFACE = "ts-lab"

local function out(s)
	log("TSTEST " .. s)
end

local function write(name, text, append)
	helpers.write_file("timeshade-test/" .. name, text, append)
end

local function loc_eq(l, goal)
	return l.direction == goal.direction and math.abs(l.position.x - goal.position.x) < 0.01
		and math.abs(l.position.y - goal.position.y) < 0.01
end

--- Lay rails from a first straight rail at `start` facing east, following plan: a list of { goal direction, count }.
--- Returns the first rail, whether the track closed on itself, and the counts of each rail prototype used.
function scenario.build(surface, start, plan, raise)
	local used = {}
	local first = surface.create_entity{ name = "straight-rail", position = start, direction = defines.direction.east,
		force = "player", raise_built = raise }
	assert(first, "cannot place the first rail at " .. start[1] .. "," .. start[2])
	used["straight-rail"] = 1
	local frontier = first.get_rail_end(defines.rail_direction.front)
	for _, step in ipairs(plan) do
		for _ = 1, step[2] do
			local pick
			local seen = {}
			for _, x in ipairs(frontier.get_rail_extensions("rail")) do
				seen[#seen + 1] = x.name .. ">" .. x.goal.direction
				if not pick and x.goal.direction == step[1] and x.goal.rail_layer == defines.rail_layer.ground
					and x.name ~= "rail-ramp" then
					pick = x
				end
			end
			assert(pick, "no extension to direction " .. step[1] .. " from " .. serpent.line(frontier.location)
				.. "; offered " .. table.concat(seen, " "))
			if surface.find_entity(pick.name, pick.position) then
				return first, true, used
			end
			local e = surface.create_entity{ name = pick.name, position = pick.position, direction = pick.direction,
				force = "player", raise_built = raise }
			assert(e, "cannot place " .. pick.name .. " at " .. serpent.line(pick.position))
			used[pick.name] = (used[pick.name] or 0) + 1
			local nextf
			for _, rd in ipairs({ defines.rail_direction.front, defines.rail_direction.back }) do
				local re = e.get_rail_end(rd)
				if loc_eq(re.location, pick.goal) then
					nextf = re
				end
			end
			frontier = assert(nextf, "no rail end of the new " .. pick.name .. " at its goal")
		end
	end
	return first, false, used
end

--- A rectangle of track with s straight rails a side, turning right; its first rail at (ox + 1, oy + 1).
function scenario.loop(surface, ox, oy, s, raise)
	local plan = { { 4, s - 1 } }
	for _, turn in ipairs({ { 5, 6, 7, 8 }, { 9, 10, 11, 12 }, { 13, 14, 15, 0 }, { 1, 2, 3, 4 } }) do
		for _, d in ipairs(turn) do
			plan[#plan + 1] = { d, 1 }
		end
		plan[#plan + 1] = { turn[4], turn[4] == 4 and 1000 or s }
	end
	return scenario.build(surface, { ox + 1, oy + 1 }, plan, raise)
end

--- A train of kinds ("L" locomotive, "C" cargo wagon) facing east, its front carriage centred at (x, y), fuelled, in
--- manual mode.
function scenario.train(surface, kinds, x, y)
	local names = { L = "locomotive", C = "cargo-wagon", F = "fluid-wagon" }
	local first
	for i = 1, #kinds do
		local e = surface.create_entity{ name = names[kinds:sub(i, i)], position = { x - 7 * (i - 1), y },
			direction = defines.direction.east, force = "player" }
		assert(e, "cannot place carriage " .. i .. " of " .. kinds .. " at " .. (x - 7 * (i - 1)) .. "," .. y)
		if e.type == "locomotive" then
			e.get_fuel_inventory().insert{ name = "coal", count = 50 }
		end
		first = first or e
	end
	local tr = first.train
	tr.manual_mode = true
	return tr
end

local function generate(surface, x1, y1, x2, y2)
	for cx = x1, x2 do
		for cy = y1, y2 do
			surface.request_to_generate_chunks({ cx * 32 + 16, cy * 32 + 16 }, 0)
		end
	end
	surface.force_generate_chunk_requests()
end

local function lab()
	local s = game.get_surface(SURFACE)
	if s then
		return s
	end
	s = game.create_surface(SURFACE, { width = 0, height = 0, peaceful_mode = true })
	s.generate_with_lab_tiles = true
	s.always_day = true
	return s
end

-- The functional test (mode "test").

local T = {}

function T.init(cfg)
	local s = lab()
	generate(s, -2, -4, 40, 20)
	local checks = {}
	-- rail prototypes this game has
	local types = {}
	for _, p in pairs(prototypes.entity) do
		if p.type:find("rail") and not p.type:find("signal") and not p.type:find("remnants") then
			types[p.type] = true
		end
	end
	checks.rail_types = types
	-- a straight line of 600 rails for train A: x from 1 to 1199 on y = 1
	local line_first, _, line_used = scenario.build(s, { 1, 1 }, { { 4, 599 } }, false)
	checks.line_rails = line_used
	-- a loop for train B, at (0, 100)
	generate(s, -2, 2, 6, 6)
	local loop_first, closed, used = scenario.loop(s, 0, 100, 16, false)
	checks.loop_closed, checks.loop_rails = closed, used
	local a = scenario.train(s, "LCC", 30, 1)
	local b = scenario.train(s, "LC", 25, 101)
	-- train C, under a schedule (automatic mode) on its own line at y = 201: LuaTrain.path is read while it drives
	scenario.build(s, { 1, 201 }, { { 4, 149 } }, false)
	local c = scenario.train(s, "LC", 30, 201)
	local car = s.create_entity{ name = "car", position = { 0, -40 }, direction = defines.direction.east, force = "player" }
	car.get_fuel_inventory().insert{ name = "coal", count = 10 }
	local asm = s.create_entity{ name = "assembling-machine-1", position = { 10.5, -60.5 }, force = "player",
		recipe = "iron-gear-wheel" }
	storage.t = { a = a, b = b, c = c, car = car, asm = asm, checks = checks, truth = {}, b_rails = {} }
	-- 2.0 train API checks
	checks.train = {
		front_end = a.front_end and a.front_end.object_name, back_end = a.back_end and a.back_end.object_name,
		front_end_rail = a.front_end.rail.name, front_end_location = a.front_end.location,
		path_manual = a.path == nil and "nil" or a.path.object_name, get_rails = #a.get_rails(),
		carriages = #a.carriages, front_stock = a.front_stock.name, back_stock = a.back_stock.name,
		orientation = a.front_stock.orientation, headed = a.carriages[2].is_headed_to_trains_front,
		kinds = (function()
			local k = {}
			for i, c in ipairs(a.carriages) do
				k[i] = c.type
			end
			return k
		end)(),
	}
	out("init done: loop closed " .. tostring(closed) .. ", rails " .. serpent.line(used))
end

--- The straight rail centred at (x, y) on the lab surface.
local function s_rail(x, y)
	return game.get_surface(SURFACE).find_entities_filtered{ name = "straight-rail", position = { x, y }, radius = 1 }[1]
end

local function speed(tr, v)
	if tr and tr.valid then
		tr.speed = v
	end
end

function T.tick(api, cfg, t)
	local g = storage.t
	if t == 1 then
		if api.config then
			api.config{ forces = { "player" }, rails = true }
		end
		if api.synthetic then
			api.synthetic{ surface = SURFACE, x1 = 0, y1 = 0, x2 = 20, y2 = 20, flicker = { ["3,3"] = 1, ["4,4"] = 300 } }
		end
	elseif t == 2 and api.charted then
		local list = {}
		for x = 0, 19 do
			for y = 0, 19 do
				list[#list + 1] = { x, y }
			end
		end
		api.charted(SURFACE, "player", list)
	elseif t > 5 and t <= 2000 and api.charted then
		api.charted(SURFACE, "player", { { 3, 3 }, { 4, 4 } })
	end
	-- train A on the straight line: east, stop, back west (the leading end switches), stop
	if t >= 100 and t <= 700 then
		speed(g.a, 0.5)
	elseif t > 700 and t <= 800 then
		speed(g.a, 0)
	elseif t > 800 and t <= 1000 then
		speed(g.a, -0.3)
	elseif t > 1000 then
		speed(g.a, 0)
	end
	-- train B round the loop
	if t >= 100 and t <= 1800 then
		speed(g.b, 0.4)
		if g.b.valid and t % 10 == 0 then
			for _, r in pairs(g.b.get_rails()) do
				g.b_rails[r.type] = (g.b_rails[r.type] or 0) + 1
			end
		end
	elseif t > 1800 then
		speed(g.b, 0)
	end
	if t >= 100 and t <= 400 and g.car.valid then
		g.car.speed = 0.2
	end
	-- train C: a schedule to a temporary stop on the rail at x = 251, then automatic mode; its path is read while it
	-- drives (PLAN 4.8 item 6 needs LuaTrain.path for map matching; the capture itself does not read it)
	if t == 100 and g.c.valid then
		local rail = s_rail(251, 201)
		local ok, err = pcall(function()
			g.c.schedule = { current = 1, records = { { rail = rail, temporary = true,
				wait_conditions = { { type = "time", compare_type = "or", ticks = 6000 } } } } }
		end)
		g.checks.schedule_set = ok and "LuaTrain.schedule" or tostring(err)
		g.c.manual_mode = false
	elseif (t == 160 or t == 400) and g.c.valid then
		local ok, res = pcall(function()
			local p = g.c.path
			if p == nil then
				return { path = "nil", state = g.c.state, speed = g.c.speed }
			end
			local rails = p.rails
			return { path = p.object_name, size = p.size, current = p.current, total = p.total_distance,
				travelled = p.travelled_distance, is_front = p.is_front, rail1 = rails[1] and rails[1].name,
				state = g.c.state, speed = g.c.speed }
		end)
		g.checks["path_auto_" .. t] = ok and res or { error = tostring(res) }
	end
	-- the truth: both ends of both trains every tick
	for _, name in ipairs({ "a", "b" }) do
		local tr = g[name]
		if tr and tr.valid then
			local f, b = tr.front_stock.position, tr.back_stock.position
			g.truth[#g.truth + 1] = string.format("%d\t%s\t%.6f\t%.6f\t%.6f\t%.6f\t%.6f", t, name, tr.speed, f.x, f.y, b.x, b.y)
		end
	end
	if #g.truth >= 600 then
		write("truth.tsv", table.concat(g.truth, "\n") .. "\n", true)
		g.truth = {}
	end
	local s = game.get_surface(SURFACE)
	if t == 500 then
		-- three rails built by script: the rail layer sends them
		s.create_entity{ name = "straight-rail", position = { 1, -101 }, direction = defines.direction.east,
			force = "player", raise_built = true }
		s.create_entity{ name = "straight-rail", position = { 3, -101 }, direction = defines.direction.east,
			force = "player", raise_built = true }
		local r = s.create_entity{ name = "straight-rail", position = { 5, -101 }, direction = defines.direction.east,
			force = "player", raise_built = true }
		g.removed_rail = r.unit_number
		r.destroy{ raise_destroy = true }
		local asm2 = s.create_entity{ name = "assembling-machine-1", position = { 20.5, -60.5 }, force = "player",
			recipe = "copper-cable", raise_built = true }
		g.asm2 = asm2.unit_number
	elseif t == 1900 then
		g.asm.destroy{ raise_destroy = true }
	elseif t == 1950 and api.resync then
		api.resync("final")
	elseif t == 2050 and g.b.valid then
		-- every carriage: destroying one only leaves a shorter train, which keeps the journey
		g.b_gone_id = g.b.id
		for _, c in pairs(g.b.carriages) do
			c.destroy{ raise_destroy = true }
		end
	elseif t == 2100 then
		write("truth.tsv", table.concat(g.truth, "\n") .. (#g.truth > 0 and "\n" or ""), true)
		g.truth = {}
		local res = { checks = g.checks, b_rails = g.b_rails, removed_rail = g.removed_rail, asm2 = g.asm2,
			a_id = g.a.valid and g.a.id, c_id = g.c.valid and g.c.id, c_x = g.c.valid and g.c.front_stock.position.x,
			car_unit = g.car.unit_number, asm_unit = g.asm.valid and g.asm.unit_number,
			b_gone_id = g.b_gone_id }
		if api.stats then
			res.stats = api.stats()
		end
		if api.memory then
			res.lua_kb = api.memory()
		end
		if api.constants then
			res.constants = api.constants()
		end
		res.version = script.active_mods.base
		write("results.json", helpers.table_to_json(res))
		out("results written")
	end
end

-- The UPS benchmark (mode "bench"): cfg.trains loops with a train each, driven at a constant speed, and synthetic
-- chart events over a virtual area of cfg.chunks charted chunks.

local B = {}

function B.init(cfg)
	local s = lab()
	local n = cfg.trains or 100
	local cols = math.ceil(math.sqrt(n))
	local spacing = 128
	generate(s, -1, -1, math.ceil(cols * spacing / 32) + 1, math.ceil(cols * spacing / 32) + 1)
	local trains = {}
	local closed = 0
	for i = 0, n - 1 do
		local ox, oy = (i % cols) * spacing, math.floor(i / cols) * spacing
		local _, ok = scenario.loop(s, ox, oy, cfg.side or 24, false)
		if ok then
			closed = closed + 1
		end
		trains[#trains + 1] = scenario.train(s, "LCCC", ox + 30, oy + 1)
	end
	storage.t = { trains = trains, rng = 12345 }
	out("bench init: " .. n .. " loops, " .. closed .. " closed")
end

function B.tick(api, cfg, t)
	local g = storage.t
	if t == 1 then
		if api.config then
			api.config{ forces = { "player" }, charts = cfg.chart_reads }
		end
		if api.synthetic then
			local side = math.floor(math.sqrt(cfg.chunks or 50176))
			g.side = side
			local flicker = {}
			for i = 0, 200 do
				flicker[(i * 7 % side) .. "," .. (i * 13 % side)] = 1
			end
			api.synthetic{ surface = SURFACE, x1 = 0, y1 = 0, x2 = side, y2 = side, flicker = flicker }
		end
	end
	for _, tr in ipairs(g.trains) do
		if tr.valid then
			tr.speed = cfg.speed or 0.5
		end
	end
	if api.charted and g.side then
		-- cfg.charted chart events a tick: a fixed set of 201 flickering chunks and random others over the whole area
		-- (Park-Miller, the same on every run; its products stay below 2^46, exact in doubles: an LCG with a 2^31 modulus
		-- and a 2^30 multiplier is not, and visited only about 1,000 distinct chunks)
		local list = {}
		for i = 1, (cfg.charted or 20) do
			g.rng = g.rng * 16807 % 2147483647
			local k = g.rng % (g.side * g.side)
			if i % 4 == 0 then
				local j = g.rng % 201
				list[#list + 1] = { j * 7 % g.side, j * 13 % g.side }
			else
				list[#list + 1] = { k % g.side, math.floor(k / g.side) }
			end
		end
		api.charted(SURFACE, "player", list)
	end
	if t == cfg.resync_tick and api.resync then
		api.resync("bench")
	end
	if t == (cfg.report_tick or 3000) then
		local res = { stats = api.stats and api.stats(), lua_kb = api.memory and api.memory(), trains = #g.trains }
		write("bench.json", helpers.table_to_json(res))
	end
end

scenario.modes = { test = T, bench = B }

return scenario
