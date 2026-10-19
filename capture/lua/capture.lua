-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap module/control.lua (Blue Oak Model License 1.0.0, see capture/README.md).
--
-- The capture's entry points, shared by both hosts (docs/v3/PROTOCOL.md): the standalone mod
-- (capture/mod/timeshade_capture) and the Clusterio save-patch module (capture/clusterio_module). A host calls
-- capture.setup with where lines go and who it is, registers capture.events, and calls capture.config (on every peer:
-- an RCON command, on_init, a settings change) and capture.resync (read-only, so any peer may run it).
--
-- Desync rule: storage changes only in events, in on_tick steps and in capture.config, which run identically on every
-- peer. What only decides what is printed (the chart cache, the debounce, the outbox, the resync's progress) lives in
-- file-locals.
local name = ...
local P = type(name) == "string" and name:match("^(.*[./])[^./]*$") or ""
local core = require(P .. "core")
local outbox = require(P .. "outbox")
local terrain = require(P .. "terrain")
local movers = require(P .. "movers")
local overlays = require(P .. "overlays")

local capture = { core = core, outbox = outbox, terrain = terrain, movers = movers, overlays = overlays, events = {} }

capture.VERSION = "0.1.0"

local host = {}
local resync = nil -- the running resync: { trace, chunks, movers, tags, machines, rails?, chunks_done }

-- Tunables a config may set. Only output-side ones: the mover rules stay fixed, so every peer runs the same ones.
local TUNABLES = { debounce = "DEBOUNCE", charts = "CHARTS", cache = "CACHE" }

local function apply(cfg)
	for key, const in pairs(TUNABLES) do
		local v = cfg and cfg[key]
		if type(v) == "number" and v >= 1 and v == math.floor(v) then
			core[const] = v
		end
	end
	terrain.configure{ cache = cfg and cfg.cache }
end

--- host: { sink = function(line, tick), instance = function() -> id, on_tick = function(st, e)?, players = fn? }.
function capture.setup(h)
	host = h or {}
	outbox.sink = host.sink or outbox.sink
	if host.instance then
		movers.instance = host.instance
	end
	if host.players then
		movers.players = host.players
	end
end

--- storage.timeshade, created or brought to this code's schema; a new one reads which forces have players. Only call
--- where every peer runs it (on_init, on_configuration_changed, config).
function capture.ensure()
	local fresh = storage.timeshade == nil
	storage.timeshade = core.migrate(storage.timeshade or {})
	if fresh then
		terrain.refresh_forces(storage.timeshade)
	end
	return storage.timeshade
end

--- The capture state, or nil before install.
function capture.state()
	return storage.timeshade
end

local function int(v)
	return type(v) == "number" and v == math.floor(v)
end

local function names(v)
	if v == nil then
		return true
	end
	if type(v) ~= "table" then
		return false
	end
	for _, n in pairs(v) do
		if type(n) ~= "string" then
			return false
		end
	end
	return true
end

--- Configure the capture and start it. c: { tile?, rails?, forces?, debounce?, charts?, cache? }. tile is gridworld's
--- { world, x, y, size, surface, exclude? }; forces names forces captured without a player; rails turns on the rail
--- layer. Every field read is checked; a bad one raises. Writes storage: run it on every peer.
function capture.config(c)
	assert(type(c) == "table", "timeshade config: not a table")
	local tile = c.tile
	assert(tile == nil or (type(tile) == "table" and type(tile.world) == "string" and type(tile.surface) == "string"
		and int(tile.x) and int(tile.y) and int(tile.size) and tile.size > 0 and tile.size % 64 == 0 and names(tile.exclude)),
		"timeshade config: bad tile")
	assert(names(c.forces), "timeshade config: forces must be a list of names")
	local st = capture.ensure()
	local extra = {}
	for _, n in pairs(c.forces or {}) do
		extra[n] = true
	end
	st.extra_forces = extra
	st.cfg = { debounce = c.debounce, charts = c.charts, cache = c.cache }
	st.rails = c.rails and true or false
	apply(st.cfg)
	terrain.refresh_forces(st)
	st.tile, st.bounds, st.configured = tile, core.bounds(tile), true
	if not st.seeded then
		movers.seed(st, game.tick)
	end
	if tile then
		outbox.send{ type = "trace", op = "timeshade.integration", phase = "gridworld", level = "info", world = tile.world,
			surface = tile.surface }
	end
end

--- on_load: the tunables a config stored, so a joining client runs with the server's.
function capture.on_load()
	local st = storage.timeshade
	apply(st and st.cfg)
end

--- Resync: the surfaces, every mover with its carriages, every tag, then every charted captured chunk (and its machines
--- and rails) over the next ticks, forced past the compare and the cap; a resynced message follows the last of them.
--- A newer resync replaces one still running. It writes no storage.
function capture.resync(trace)
	assert(type(trace) == "string", "timeshade resync: the trace must be a string")
	local st = storage.timeshade
	if not (st and st.configured) then
		return false
	end
	resync = { trace = trace, chunks = 0, movers = 0, tags = 0, machines = 0 }
	terrain.surfaces(st)
	resync.movers = movers.resync(st, game.tick)
	resync.tags = overlays.resync_tags(st)
	terrain.resync(st, resync)
	return true
end

--- The running resync, or nil.
function capture.resyncing()
	return resync
end

--- Once the resync's sentinel is read and no machine waits: resynced with its counts.
local function finish()
	local r = resync
	if r and r.chunks_done and overlays.waiting() == 0 then
		local d = terrain.state().debounce
		outbox.send{ type = "resynced", trace = r.trace, counts = { chunks = r.chunks, movers = r.movers, tags = r.tags,
			machines = r.machines, rails = r.rails or 0, drops = d.drops + outbox.oversize() } }
		resync = nil
	end
end

--- The per-tick steps: nothing before the config; then the host's step, the resync walk, chart reads, the resync's
--- machines, movers, the resync's end, and the flush.
function capture.on_tick(e)
	local st = storage.timeshade
	if not (st and st.configured) then
		return
	end
	if host.on_tick then
		host.on_tick(st, e)
	end
	terrain.walk(st, resync)
	terrain.step(st, e.tick, resync)
	overlays.step()
	movers.step(st, e.tick)
	finish()
	outbox.flush(e.tick, game.speed)
end

local function on(names_, fn)
	for _, n in ipairs(names_) do
		local id = defines.events[n]
		if id then
			capture.events[id] = fn
		end
	end
end

--- Fill capture.events (event id -> handler). Called by the host at load, when defines exist.
function capture.build_events()
	local ev = {}
	capture.events = ev
	on({ "on_tick" }, capture.on_tick)
	on({ "on_chunk_charted" }, function(e)
		terrain.on_chunk_charted(storage.timeshade, e)
	end)
	on({ "on_chunk_deleted" }, function(e)
		terrain.on_chunk_deleted(storage.timeshade, e)
	end)
	on({ "on_surface_created" }, function(e)
		terrain.on_surface_created(storage.timeshade, e)
	end)
	on({ "on_surface_deleted" }, terrain.on_surface_deleted)
	on({ "on_player_created", "on_player_changed_force", "on_player_removed", "on_forces_merged" }, function()
		terrain.refresh_forces(storage.timeshade)
	end)
	on({ "on_train_created" }, function(e)
		movers.on_train_created(storage.timeshade, e)
	end)
	on({ "on_object_destroyed" }, function(e)
		local st = storage.timeshade
		local j = st and st.reg[e.registration_number]
		if type(j) == "string" then
			st.reg[e.registration_number] = nil
			movers.gone(st, j)
		end
	end)
	on({ "on_player_left_game" }, function(e)
		local st, p = storage.timeshade, game.get_player(e.player_index)
		if st and p then
			movers.gone(st, "p:" .. p.name)
		end
	end)
	on({ "on_chart_tag_added", "on_chart_tag_modified", "on_chart_tag_removed" }, function(e)
		overlays.on_tag(storage.timeshade, e)
	end)
	-- circuit-set recipes and a furnace's own switch are missed (as in illisis)
	on({ "on_built_entity", "on_robot_built_entity", "on_space_platform_built_entity", "script_raised_built",
		"script_raised_revive", "on_entity_settings_pasted", "on_gui_closed" }, function(e)
		overlays.on_entity(storage.timeshade, e)
	end)
	on({ "on_player_mined_entity", "on_robot_mined_entity", "on_space_platform_mined_entity", "on_entity_died",
		"script_raised_destroy" }, function(e)
		overlays.on_removed(storage.timeshade, e)
	end)
	return ev
end

--- Counters for the stats command.
function capture.stats()
	local t = terrain.state()
	local lines, bytes = outbox.stats()
	local st = storage.timeshade
	local nm = 0
	for _ in pairs(st and st.movers or {}) do
		nm = nm + 1
	end
	return { reads = t.reads, sent = t.sent, queued = core.size(t.debounce.queue), drops = t.debounce.drops,
		cached = core.cache_count(t.cache), lines = lines, bytes = bytes, oversize = outbox.oversize(), movers = nm,
		resyncing = resync and resync.trace or nil }
end

return capture
