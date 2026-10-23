-- railbench recorder (bench/railbench/README.md): test-only ground truth for the train benchmark, never shipped with
-- the capture. It runs in the same game as the capture and only reads the game, except for the optional refuel fix.
--
-- config.lua (written by railbench.mjs) sets:
--   mode     "survey": counts trains, rails and movement and logs one RBSURVEY line at the end;
--            "record": writes every train's every carriage on every tick to script-output/railbench/gt-*.txt
--   ticks    how long to run (ticks after the first on_tick)
--   refuel   true: every 600 ticks, top up locomotives whose fuel inventory is empty (a documented minimal fix)
--
-- Ground truth format (text, one record per line, integers; positions in 1/256 tile, Factorio's own fixed point, so
-- they are exact; orientations in 1/65536 turn; speeds in 1e-5 tiles a tick):
--   RBGT 1 <factorio version> <start tick>
--   STATE <value>=<name> ...                         defines.train_state
--   SURF <index> <name>
--   RAIL <surface> <unit> <type> <name> <x> <y> <dir> <layer> <length mm> <front end x> <y> <dir> <back end x> <y>
--        <dir> <rails connected at the front end> <at the back end>
--   TRAIN <id> <surface> <carriages> <kinds> <manual 0|1>   first sight and after any change of the train
--   GONE <id>
--   T <tick>
--   A <id> <speed> <state> <rail> <x1> <y1> <o1> <x2> ...   carriages from the train's front (LuaTrain.carriages)
--   D <id> <speed> <state> <rail> <dx1> <dy1> <do1> ...     the same as differences from this train's line one tick earlier
-- <rail> is the unit number of the rail under the end the train moves towards (front_end while speed >= 0, else
-- back_end), "-" when it is the same as in the train's previous line. A train has a line on every tick it moves, on the
-- tick it stops, and when its state changes while it stands.
local cfg = require("config")

local RAIL_TYPES = {
	["straight-rail"] = true, ["half-diagonal-rail"] = true, ["curved-rail-a"] = true, ["curved-rail-b"] = true,
	["legacy-straight-rail"] = true, ["legacy-curved-rail"] = true, ["elevated-straight-rail"] = true,
	["elevated-half-diagonal-rail"] = true, ["elevated-curved-rail-a"] = true, ["elevated-curved-rail-b"] = true,
	["rail-ramp"] = true,
}
local KINDS = { locomotive = "L", ["cargo-wagon"] = "C", ["fluid-wagon"] = "F", ["artillery-wagon"] = "A" }

local floor = math.floor
local concat = table.concat

local t0 -- first tick seen
local out, out_n, file_seq, file_bytes = {}, 0, 0, 0
local trains = {} -- id -> { tr, last tick written, speed, state, rail, cars = { x, y, o, ... } }
local survey = {}

local function q(v)
	return floor(v * 256 + 0.5)
end

local function qo(o)
	return floor(o * 65536 + 0.5) % 65536
end

local function emit(line)
	out_n = out_n + 1
	out[out_n] = line
end

local function flush(force)
	if out_n == 0 then
		return
	end
	if not force and out_n < 20000 then
		return
	end
	out_n = out_n + 1
	out[out_n] = ""
	local text = concat(out, "\n")
	helpers.write_file(string.format("railbench/gt-%04d.txt", file_seq), text, true)
	-- a new file every 128 MiB, so a reader can take one file as one string
	file_bytes = file_bytes + #text
	if file_bytes > 128 * 1024 * 1024 then
		file_seq, file_bytes = file_seq + 1, 0
	end
	out, out_n = {}, 0
end

local function rail_types()
	local list = {}
	for t in pairs(RAIL_TYPES) do
		if #prototypes.get_entity_filtered{ { filter = "type", type = t } } > 0 then
			list[#list + 1] = t
		end
	end
	return list
end

local function connections(rail, dir)
	local n = 0
	for _, c in pairs{ defines.rail_connection_direction.left, defines.rail_connection_direction.straight,
		defines.rail_connection_direction.right } do
		if rail.get_connected_rail{ rail_direction = dir, rail_connection_direction = c } then
			n = n + 1
		end
	end
	return n
end

local function dump_rails(types)
	local counts = {}
	for _, s in pairs(game.surfaces) do
		emit("SURF " .. s.index .. " " .. s.name)
		for _, r in pairs(s.find_entities_filtered{ type = types }) do
			local fe = r.get_rail_end(defines.rail_direction.front).location
			local be = r.get_rail_end(defines.rail_direction.back).location
			local layer = fe.rail_layer == defines.rail_layer.elevated and 1 or 0
			if cfg.mode == "record" then
				emit(concat({ "RAIL", s.index, r.unit_number, r.type, r.name, q(r.position.x), q(r.position.y), r.direction,
					layer, floor(r.rail_length * 1000 + 0.5), q(fe.position.x), q(fe.position.y), fe.direction,
					q(be.position.x), q(be.position.y), be.direction, connections(r, defines.rail_direction.front),
					connections(r, defines.rail_direction.back) }, " "))
			end
			local key = s.name .. "/" .. r.type
			counts[key] = (counts[key] or 0) + 1
		end
	end
	return counts
end

local function kinds(tr)
	local k = {}
	for i, c in ipairs(tr.carriages) do
		local x = KINDS[c.type] or "C"
		k[i] = c.is_headed_to_trains_front and x or x:lower()
	end
	return concat(k)
end

local function surface_of(tr)
	local c = tr.carriages[1]
	return c and c.surface_index or 0
end

local function add_train(tr)
	local m = { tr = tr, last = nil, n = #tr.carriages }
	trains[tr.id] = m
	if cfg.mode == "record" then
		emit(concat({ "TRAIN", tr.id, surface_of(tr), m.n, kinds(tr), tr.manual_mode and 1 or 0 }, " "))
	end
	return m
end

local function refuel()
	for _, tr in pairs(game.train_manager.get_trains{}) do
		for _, list in pairs(tr.locomotives) do
			for _, loco in pairs(list) do
				local inv = loco.get_fuel_inventory and loco.get_fuel_inventory()
				if inv and inv.is_empty() and loco.burner then
					-- the first fuel item the burner accepts (coal in vanilla)
					for _, fuel in pairs{ "coal", "solid-fuel", "rocket-fuel", "nuclear-fuel", "wood" } do
						if prototypes.item[fuel] and inv.can_insert{ name = fuel, count = 1 } then
							inv.insert{ name = fuel, count = 50 }
							survey.refuelled = (survey.refuelled or 0) + 1
							break
						end
					end
				end
			end
		end
	end
end

--- One tick of the recorder: a line per train that moves, stopped this tick or changed state.
local function record(tick)
	emit("T " .. tick)
	for id, m in pairs(trains) do
		local tr = m.tr
		if not tr.valid then
			emit("GONE " .. id)
			trains[id] = nil
		else
			local speed = tr.speed
			local state = tr.state
			if speed ~= 0 or (m.speed and m.speed ~= 0) or state ~= m.state then
				if #tr.carriages ~= m.n then
					m = add_train(tr)
				end
				local rend = speed >= 0 and tr.front_end or tr.back_end
				local rail = rend and rend.rail
				local ru = rail and rail.unit_number or 0
				local parts = { "", id, floor(speed * 1e5 + 0.5), state, ru == m.rail and "-" or ru }
				local delta = m.last == tick - 1 and m.cars ~= nil
				parts[1] = delta and "D" or "A"
				local cars = tr.carriages
				local prev = m.cars
				local now = {}
				local k = 5
				for i = 1, #cars do
					local c = cars[i]
					local p = c.position
					local x, y, o = q(p.x), q(p.y), qo(c.orientation)
					local j = 3 * i - 2
					now[j], now[j + 1], now[j + 2] = x, y, o
					if delta then
						local d = o - prev[j + 2]
						if d > 32768 then
							d = d - 65536
						elseif d < -32768 then
							d = d + 65536
						end
						parts[k + 1], parts[k + 2], parts[k + 3] = x - prev[j], y - prev[j + 1], d
					else
						parts[k + 1], parts[k + 2], parts[k + 3] = x, y, o
					end
					k = k + 3
				end
				emit(concat(parts, " "))
				m.cars, m.last, m.speed, m.state, m.rail = now, tick, speed, state, ru
			end
		end
	end
	flush(false)
end

--- Survey: speed integrated every 60 ticks, per train.
local function survey_step()
	for id, m in pairs(trains) do
		local tr = m.tr
		if tr.valid then
			local s = math.abs(tr.speed)
			m.dist = (m.dist or 0) + s * 60
			if s > 0 then
				m.moving_ticks = (m.moving_ticks or 0) + 60
			end
			m.states = m.states or {}
			m.states[tr.state] = (m.states[tr.state] or 0) + 1
		end
	end
end

local function survey_report(rails)
	local names = {}
	for k, v in pairs(defines.train_state) do
		names[v] = k
	end
	local res = { tick0 = t0, ticks = cfg.ticks, rails = rails, trains = 0, automatic = 0, moved = 0, moved_auto = 0,
		carriages = {}, states = {}, km = 0, refuelled = survey.refuelled or 0, no_fuel = 0, surfaces = {} }
	for _, m in pairs(trains) do
		local tr = m.tr
		if tr.valid then
			res.trains = res.trains + 1
			local auto = not tr.manual_mode
			if auto then
				res.automatic = res.automatic + 1
			end
			local n = #tr.carriages
			res.carriages[tostring(n)] = (res.carriages[tostring(n)] or 0) + 1
			if (m.dist or 0) > 100 then
				res.moved = res.moved + 1
				if auto then
					res.moved_auto = res.moved_auto + 1
				end
			end
			res.km = res.km + (m.dist or 0) / 1000
			local st = names[tr.state] or tostring(tr.state)
			res.states[st] = (res.states[st] or 0) + 1
			local sname = game.get_surface(surface_of(tr)) and game.get_surface(surface_of(tr)).name or "?"
			res.surfaces[sname] = (res.surfaces[sname] or 0) + 1
			for _, list in pairs(tr.locomotives) do
				for _, loco in pairs(list) do
					local inv = loco.get_fuel_inventory and loco.get_fuel_inventory()
					if inv and inv.is_empty() and loco.burner and not loco.burner.currently_burning then
						res.no_fuel = res.no_fuel + 1
					end
				end
			end
		end
	end
	local json = helpers.table_to_json(res)
	log("RBSURVEY " .. json)
	helpers.write_file("railbench/survey.json", json)
end

local rails_counted

local function start(tick)
	t0 = tick
	local states = {}
	for k, v in pairs(defines.train_state) do
		states[#states + 1] = v .. "=" .. k
	end
	if cfg.mode == "record" then
		emit("RBGT 1 " .. script.active_mods.base .. " " .. tick)
		emit("STATE " .. concat(states, " "))
	end
	rails_counted = dump_rails(rail_types())
	for _, tr in pairs(game.train_manager.get_trains{}) do
		add_train(tr)
	end
	flush(true)
end

script.on_event(defines.events.on_tick, function(e)
	local tick = e.tick
	if not t0 then
		start(tick)
	end
	if cfg.refuel and (tick - t0) % 600 == 0 then
		refuel()
	end
	if cfg.mode == "record" then
		if tick - t0 <= cfg.ticks then
			record(tick)
		end
	elseif (tick - t0) % 60 == 0 then
		survey_step()
	end
	if tick - t0 == cfg.ticks then
		if cfg.mode == "record" then
			flush(true)
			log("RBRECORD done at tick " .. tick)
			if remote.interfaces["timeshadedb_exporter"] then
				log("RBCAPTURE " .. helpers.table_to_json(remote.call("timeshadedb_exporter", "stats")))
			end
		else
			survey_report(rails_counted)
		end
	end
end)

script.on_event(defines.events.on_train_created, function(e)
	if t0 then
		add_train(e.train)
	end
end)
