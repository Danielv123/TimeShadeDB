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
	defines = { events = ev, rail_direction = { front = 0, back = 1 } }
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

-- Rails (docs/v3/RAIL-GEOMETRY.md): the shapes of curved-rail-a and curved-rail-b for the piece built facing north
-- (spec 2.4, 2.5), placed by the spec's 8-fold symmetry (2.1).
local RAIL_R, CURVE_A_LENGTH, CURVE_B_LENGTH = 13, 5.132284556, 5.077891568

local function place8(i8, x, y)
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

local function curve_offset(kind, dir, s)
	local x, y
	if kind == 1 then
		local a = s / RAIL_R
		x, y = RAIL_R * (math.cos(a) - 1), 2 - RAIL_R * math.sin(a)
	else
		local a = (CURVE_A_LENGTH + s) / RAIL_R
		local r = RAIL_R + (s / CURVE_B_LENGTH) * (9 * math.sqrt(2) - RAIL_R)
		x, y = -11 + r * math.cos(a), 7 - r * math.sin(a)
	end
	return place8(math.floor(dir / 2) % 8, x, y)
end

local STEPS = { [0] = { 0, -1 }, { 1, -2 }, { 1, -1 }, { 2, -1 }, { 1, 0 }, { 2, 1 }, { 1, 1 }, { 1, 2 }, { 0, 1 },
	{ -1, 2 }, { -1, 1 }, { -2, 1 }, { -1, 0 }, { -2, -1 }, { -1, -1 }, { -1, -2 } }

local next_unit = 10000

--- A rail entity of the fake world: type, direction, position, unit number; s0 and len are its place on a path and
--- `fwd` whether the path runs it from its front end to its back end. ends = { front, back } as { x, y, direction }.
local function rail_entity(type, dir, x, y, s0, len, fwd, ends)
	next_unit = next_unit + 1
	return object("LuaEntity", { type = type, name = type, direction = dir, position = { x = x, y = y },
		unit_number = next_unit, s0 = s0, len = len, fwd = fwd, ends = ends })
end

--- A rail path: straight east along y = 1 from x = -2000 to x = L (2-tile straight-rail pieces), a right turn of
--- curved-rail-a, curved-rail-b, curved-rail-b, curved-rail-a (a 2.0 quarter turn), then straight south. path.point(s)
--- gives the point at rail distance s (s = 0 at x = 0 on the first straight) and the unit direction of travel;
--- path.rails lists the rail entities in order, path.at(s) the one holding s. path_lc's old signature is kept (R unused).
--- With `ramp` (an even x below L - 16) a rail-ramp takes the 16 tiles of the first straight from x = ramp (in plan
--- only: its rail distance here is its plan length).
function fake.path_lc(L, _, ramp)
	local core = require("core")
	local path = { rails = {} }
	local x, y, d = -2000, 1, 4
	local s = -2000
	local function add_straight(n)
		local st = STEPS[d]
		for _ = 1, n do
			local x1, y1 = x + 2 * st[1], y + 2 * st[2]
			-- a straight-rail facing north-south (0) or east-west (4); its front end is the one to the north or east
			local ew = d == 4 or d == 12
			local front_first = d == 0 or d == 4 -- runs from its back end towards its front end? (front end ahead)
			local e = rail_entity("straight-rail", ew and 4 or 0, (x + x1) / 2, (y + y1) / 2, s, 2, not front_first, nil)
			local back_end = { x, y, (d + 8) % 16 }
			local front_end = { x1, y1, d }
			if front_first then
				e.ends = { front_end, back_end }
			else
				e.ends = { back_end, front_end }
			end
			path.rails[#path.rails + 1] = e
			x, y, s = x1, y1, s + 2
		end
	end
	local function add_curve(kind, turn)
		local want_in, want_out = (d + 8) % 16, (d + turn) % 16
		for dir = 0, 15 do
			local en = core.piece_ends(kind, dir)
			local fwd
			if en[3] == want_in and en[6] == want_out then
				fwd = true
			elseif en[6] == want_in and en[3] == want_out then
				fwd = false
			end
			if fwd ~= nil then
				local px, py = x - (fwd and en[1] or en[4]), y - (fwd and en[2] or en[5])
				local len = kind == 1 and CURVE_A_LENGTH or CURVE_B_LENGTH
				local e = rail_entity(kind == 1 and "curved-rail-a" or "curved-rail-b", dir, px, py, s, len, fwd,
					{ { px + en[1], py + en[2], en[3] }, { px + en[4], py + en[5], en[6] } })
				path.rails[#path.rails + 1] = e
				local out = fwd and e.ends[2] or e.ends[1]
				x, y, d, s = out[1], out[2], out[3], s + len
				return
			end
		end
		error("no piece")
	end
	local function add_ramp()
		for dir = 0, 12, 4 do
			local en = core.piece_ends(4, dir)
			if en[3] == (d + 8) % 16 then
				local px, py = x - en[1], y - en[2]
				local e = rail_entity("rail-ramp", dir, px, py, s, 16, true,
					{ { px + en[1], py + en[2], en[3] }, { px + en[4], py + en[5], en[6] } })
				path.rails[#path.rails + 1] = e
				x, y, s = px + en[4], py + en[5], s + 16
				return
			end
		end
		error("no ramp")
	end
	if ramp then
		add_straight(math.floor((ramp + 2000) / 2))
		add_ramp()
		add_straight(math.floor((L - ramp - 16) / 2))
	else
		add_straight(math.floor((L + 2000) / 2))
	end
	add_curve(1, 1)
	add_curve(2, 1)
	add_curve(2, 1)
	add_curve(1, 1)
	add_straight(4000)
	function path.at(u)
		local lo, hi = 1, #path.rails
		while lo < hi do
			local mid = math.floor((lo + hi + 1) / 2)
			if path.rails[mid].s0 <= u then
				lo = mid
			else
				hi = mid - 1
			end
		end
		return path.rails[lo], lo
	end
	function path.point(u)
		local r = path.at(u)
		local q = math.max(0, math.min(r.len, u - r.s0))
		if r.type == "straight-rail" or r.type == "rail-ramp" then
			local a, b = r.ends[1], r.ends[2]
			if not r.fwd then
				a, b = b, a
			end
			local f, l = q / r.len, r.len
			return a[1] + (b[1] - a[1]) * f, a[2] + (b[2] - a[2]) * f, (b[1] - a[1]) / l, (b[2] - a[2]) / l
		end
		local sq = r.fwd and q or r.len - q
		local ox, oy = curve_offset(r.type == "curved-rail-a" and 1 or 2, r.direction, sq)
		local h = 1e-4
		local ax, ay = curve_offset(r.type == "curved-rail-a" and 1 or 2, r.direction, math.max(0, sq - h))
		local bx, by = curve_offset(r.type == "curved-rail-a" and 1 or 2, r.direction, math.min(r.len, sq + h))
		local tx, ty = bx - ax, by - ay
		local l = math.sqrt(tx * tx + ty * ty)
		if not r.fwd then
			tx, ty = -tx, -ty
		end
		return r.position.x + ox, r.position.y + oy, tx / l, ty / l
	end
	path.straight_end = math.floor((L + 2000) / 2) * 2 - 2000 -- where the turn begins
	return path
end

--- The rail end of rail i of path p ahead of travel along the path (`ahead`) or behind it, as a LuaRailEnd (rail,
--- direction, location, make_copy, flip_direction, move_natural; the path has no junctions).
local function rail_end(p, i, ahead)
	local le = {}
	local function set()
		local r = p.rails[i]
		-- the front end is ends[1]; a rail run front to back (fwd) has its back end ahead
		local back = r.fwd == ahead
		local e = back and r.ends[2] or r.ends[1]
		le.rail, le.direction = r, back and 1 or 0
		le.location = { position = { x = e[1], y = e[2] }, direction = e[3] }
	end
	le.make_copy = function()
		return rail_end(p, i, ahead)
	end
	le.flip_direction = function()
		ahead = not ahead
		set()
	end
	le.move_natural = function()
		local j = i + (ahead and 1 or -1)
		if not p.rails[j] then
			return false
		end
		i = j
		set()
		return true
	end
	set()
	return le
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

--- Put train tr's leading joint at rail distance pos of its path: every carriage's joints 4 apart, carriages 7 apart
--- (RAIL-GEOMETRY.md 3); its rail ends and get_rails follow.
function fake.place(tr, pos)
	tr.pos = pos
	local path = tr.path_fn
	local n = #tr.carriages
	for i, c in ipairs(tr.carriages) do
		local uf = pos - (i - 1) * 7
		local fx, fy = path.point(uf)
		local bx, by = path.point(uf - 4)
		c.position = { x = (fx + bx) / 2, y = (fy + by) / 2 }
		local o = (math.atan(fx - bx, by - fy) / (2 * math.pi)) % 1
		if not c.is_headed_to_trains_front then
			o = (o + 0.5) % 1
		end
		c.orientation = o
	end
	local back = pos - 7 * (n - 1) - 4
	tr.front_end = rail_end(path, select(2, path.at(pos)), true)
	tr.back_end = rail_end(path, select(2, path.at(back)), false)
	tr.get_rails = function()
		local out = {}
		local _, i0 = path.at(pos)
		local _, i1 = path.at(back)
		for i = i0, i1, -1 do
			out[#out + 1] = path.rails[i]
		end
		return out
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
