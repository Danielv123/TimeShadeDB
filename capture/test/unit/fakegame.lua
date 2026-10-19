-- A fake of the parts of the Factorio runtime API the capture reads, for unit tests in plain Lua. It installs game,
-- script, defines, helpers, prototypes and storage as globals, and builds surfaces, forces, charts, trains, vehicles
-- and players that tests move by hand. Names and shapes follow the Factorio 2.0 API; the headless game tests
-- (capture/test/game) check the real thing.
local json = require("json")

local fake = {}

local EVENTS = { "on_tick", "on_chunk_charted", "on_chunk_deleted", "on_chunk_generated", "on_surface_created",
	"on_surface_deleted", "on_player_created", "on_player_changed_force", "on_player_removed", "on_forces_merged",
	"on_train_created", "on_object_destroyed", "on_player_left_game", "on_chart_tag_added", "on_chart_tag_modified",
	"on_chart_tag_removed", "on_built_entity", "on_robot_built_entity", "on_space_platform_built_entity",
	"script_raised_built", "script_raised_revive", "on_entity_settings_pasted", "on_gui_closed",
	"on_player_mined_entity", "on_robot_mined_entity", "on_space_platform_mined_entity", "on_entity_died",
	"script_raised_destroy", "on_runtime_mod_setting_changed" }

local next_id = 0
local function object(kind, t)
	next_id = next_id + 1
	t.object_name, t.valid, t.__id = kind, true, next_id
	return t
end

--- Install the globals. Returns the world: { surfaces, forces, trains, vehicles, players, charts, tags, machines }.
function fake.install()
	local ev = {}
	for i, n in ipairs(EVENTS) do
		ev[n] = i
	end
	defines = { events = ev }
	local world = { surfaces = {}, by_index = {}, forces = {}, trains = {}, vehicles = {}, players = {}, charts = {},
		tags = {}, entities = {}, regs = 0, encodes = 0 }
	fake.world = world
	storage = {}
	helpers = {
		-- deterministic, equality-preserving and printable, like deflate + base64: printable text as it is, anything else
		-- run-length coded
		encode_string = function(s)
			world.encodes = world.encodes + 1
			if not s:find("[^%w ]") then
				return "E" .. s
			end
			local out, i = {}, 1
			while i <= #s do
				local b = s:byte(i)
				local j = i
				while j < #s and s:byte(j + 1) == b do
					j = j + 1
				end
				out[#out + 1] = string.format("%02x*%d;", b, j - i + 1)
				i = j + 1
			end
			return "R" .. table.concat(out)
		end,
		table_to_json = json.encode,
	}
	prototypes = {
		entity = { ["straight-rail"] = { type = "straight-rail" }, ["curved-rail-a"] = { type = "curved-rail-a" },
			car = { type = "car" }, spidertron = { type = "spider-vehicle" }, locomotive = { type = "locomotive" } },
		get_entity_filtered = function()
			return { car = prototypes.entity.car, spidertron = prototypes.entity.spidertron }
		end,
	}
	script = {
		register_on_object_destroyed = function(ref)
			if not ref.__reg then
				world.regs = world.regs + 1
				ref.__reg = world.regs
			end
			return ref.__reg, ref.__id, 1
		end,
	}
	local forces = {}
	game = {
		tick = 0,
		speed = 1,
		surfaces = world.surfaces,
		forces = forces,
		players = world.players,
		connected_players = world.players,
		get_surface = function(i)
			return world.by_index[i] or world.surfaces[i]
		end,
		get_player = function(i)
			return world.players[i]
		end,
		train_manager = {
			get_trains = function(f)
				local out = {}
				for _, tr in ipairs(world.trains) do
					if tr.valid and (f.is_moving == nil or (tr.speed ~= 0) == f.is_moving) then
						out[#out + 1] = tr
					end
				end
				return out
			end,
		},
		get_vehicles = function(f)
			local out = {}
			for _, v in ipairs(world.vehicles) do
				if v.valid and (f.is_moving == nil or (v.speed ~= 0) == f.is_moving) then
					out[#out + 1] = v
				end
			end
			return out
		end,
	}
	fake.force("player")
	fake.surface("nauvis")
	return world
end

function fake.force(name)
	local world = fake.world
	local f
	f = object("LuaForce", {
		name = name,
		get_chunk_chart = function(surface, pos)
			return world.charts[surface.index .. ":" .. f.index .. ":" .. pos.x .. "," .. pos.y]
		end,
		is_chunk_charted = function(surface, pos)
			return world.charts[surface.index .. ":" .. f.index .. ":" .. pos.x .. "," .. pos.y] ~= nil
		end,
		find_chart_tags = function(surface)
			local out = {}
			for _, tag in ipairs(world.tags) do
				if tag.surface == surface and tag.force == f then
					out[#out + 1] = tag
				end
			end
			return out
		end,
	})
	local n = 0
	for _ in pairs(game.forces) do
		n = n + 1
	end
	f.index = n / 2 + 1
	game.forces[name], game.forces[f.index] = f, f
	return f
end

function fake.surface(name, planet)
	local world = fake.world
	local index = 0
	for _ in pairs(world.by_index) do
		index = index + 1
	end
	index = index + 1
	local s
	s = object("LuaSurface", {
		name = name,
		index = index,
		planet = planet and { name = planet } or nil,
		chunks = {},
		get_chunks = function()
			local list = {}
			for _, c in ipairs(s.chunks) do
				list[#list + 1] = c
			end
			local i = 0
			return function()
				i = i + 1
				return list[i]
			end
		end,
		find_entities_filtered = function(f)
			local out = {}
			local types = {}
			for _, t in ipairs(f.type or {}) do
				types[t] = true
			end
			local a = f.area
			for _, e in ipairs(world.entities) do
				local p = e.position
				if e.valid and e.surface == s and types[e.type] and p.x >= a[1][1] and p.x < a[2][1] and p.y >= a[1][2]
					and p.y < a[2][2] then
					out[#out + 1] = e
				end
			end
			return out
		end,
	})
	world.surfaces[name], world.by_index[index] = s, s
	return s
end

--- Mark chunk (x, y) of surface s generated, and charted for force f with chart (a string; nil uncharts it).
function fake.chart(s, f, x, y, chart)
	local world = fake.world
	local key = s.index .. ":" .. f.index .. ":" .. x .. "," .. y
	if not s.generated then
		s.generated = {}
	end
	if not s.generated[x .. "," .. y] then
		s.generated[x .. "," .. y] = true
		s.chunks[#s.chunks + 1] = { x = x, y = y }
	end
	world.charts[key] = chart
end

--- A path: straight east from (0, 0) for L tiles, a quarter circle of radius R turning south, then straight south.
--- Returns f(s) -> x, y, orientation for arc length s (negative s continues west).
function fake.path_lc(L, R)
	local quarter = math.pi * R / 2
	return function(s)
		if s <= L then
			return s, 0, 0.25
		end
		local a = s - L
		if a <= quarter then
			local th = a / R -- 0 .. pi/2, centre at (L, R)
			return L + R * math.sin(th), R - R * math.cos(th), 0.25 + th / (2 * math.pi)
		end
		local b = a - quarter
		return L + R, R + b, 0.5
	end
end

--- A train of `kinds` (e.g. "LCC") on surface s along path, its front carriage at arc length pos; carriages 7 tiles
--- apart, all facing the front.
function fake.train(s, kinds, path, pos)
	local world = fake.world
	local types = { L = "locomotive", C = "cargo-wagon", F = "fluid-wagon", A = "artillery-wagon" }
	local tr = object("LuaTrain", { speed = 0, carriages = {}, path_fn = path, pos = pos })
	world.trains[#world.trains + 1] = tr
	tr.id = #world.trains + 100
	for i = 1, #kinds do
		local k = kinds:sub(i, i)
		local ty = types[k:upper()]
		tr.carriages[i] = object("LuaEntity", { type = ty, name = ty, force = game.forces.player, surface_index = s.index,
			surface = s, is_headed_to_trains_front = k == k:upper(), color = ty == "locomotive" and { r = 1, g = 0, b = 0 }
			or nil, prototype = {}, position = { x = 0, y = 0 }, orientation = 0, train = tr })
	end
	tr.front_stock, tr.back_stock = tr.carriages[1], tr.carriages[#tr.carriages]
	fake.place(tr, pos)
	return tr
end

--- Put train tr's front carriage at arc length pos.
function fake.place(tr, pos)
	tr.pos = pos
	for i, c in ipairs(tr.carriages) do
		local x, y, o = tr.path_fn(pos - (i - 1) * 7)
		c.position = { x = x, y = y }
		c.orientation = o
	end
end

function fake.vehicle(s, kind, x, y, orientation)
	local world = fake.world
	local v = object("LuaEntity", { type = kind == "S" and "spider-vehicle" or "car", name = kind == "S" and "spidertron"
		or "car", force = game.forces.player, surface_index = s.index, surface = s, position = { x = x, y = y },
		orientation = orientation or 0, speed = 0, prototype = {} })
	world.vehicles[#world.vehicles + 1] = v
	v.unit_number = 500 + #world.vehicles
	return v
end

function fake.player(name, s, x, y)
	local world = fake.world
	local p = object("LuaPlayer", { name = name, force = game.forces.player, connected = true,
		physical_position = { x = x, y = y }, physical_surface_index = s.index })
	world.players[#world.players + 1] = p
	p.index = #world.players
	return p
end

--- A string of v for comparing storage: fake game objects (tables with an object_name) by identity.
function fake.snapshot(v, depth)
	depth = depth or 0
	if type(v) ~= "table" then
		return type(v) .. ":" .. tostring(v)
	end
	if v.object_name then
		return "obj#" .. tostring(v.__id)
	end
	if depth > 20 then
		return "deep"
	end
	local keys = {}
	for k in pairs(v) do
		keys[#keys + 1] = k
	end
	table.sort(keys, function(a, b)
		return tostring(a) < tostring(b)
	end)
	local parts = {}
	for _, k in ipairs(keys) do
		parts[#parts + 1] = tostring(k) .. "=" .. fake.snapshot(v[k], depth + 1)
	end
	return "{" .. table.concat(parts, ",") .. "}"
end

return fake
