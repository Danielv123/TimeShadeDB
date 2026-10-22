-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap module/terrain.lua (Blue Oak Model License 1.0.0, see capture/README.md).
--
-- Terrain (docs/v3/PROTOCOL.md section 6): on_chunk_charted queues a chunk through the debounce; each tick reads
-- core.CHARTS queued chunks with LuaForce.get_chunk_chart and sends those whose chart differs from the last one sent.
-- The forces with players, the surfaces message and the resync's walk of charted chunks live here too.
--
-- The debounce, the queue, the last-sent cache and the resync walk are file-locals: they only decide what is printed,
-- and printing changes no game state. A joining client starts empty, which only changes what that client would print;
-- every boot starts with a resync that refills the cache. Keeping them out of storage keeps them out of the save.
local name = ...
local P = type(name) == "string" and name:match("^(.*[./])[^./]*$") or ""
local core = require(P .. "core")
local outbox = require(P .. "outbox")
local overlays = require(P .. "overlays")

local terrain = {}

local deb = core.debounce()
local cache = core.cache(core.CACHE)
local walk = nil -- the running resync's walk, see terrain.resync
local walks = 0 -- resyncs started since load: each walk and its sentinel carry their own number, not the trace
local reads, sent, skipped = 0, 0, 0

--- seam(force, surface, position) -> chart or nil, or nil for the engine. Headless servers never chart, so the game
--- tests stand it in for get_chunk_chart and is_chunk_charted.
terrain.seam = nil

local function chart_of(force, surface, pos)
	if terrain.seam then
		return terrain.seam(force, surface, pos)
	end
	return force.get_chunk_chart(surface, pos)
end

local function is_charted(force, surface, pos)
	if terrain.seam then
		return terrain.seam(force, surface, pos) ~= nil
	end
	return force.is_chunk_charted(surface, pos)
end

--- Size the last-sent cache (entries). A new size drops what it held; the same size keeps it.
function terrain.configure(cfg)
	if cfg.cache and core.cache(cfg.cache).half ~= cache.half then
		cache = core.cache(cfg.cache)
	end
end

--- The file-local state, for tests and the stats command.
function terrain.state()
	return { debounce = deb, cache = cache, walk = walk, reads = reads, sent = sent, skipped = skipped }
end

--- Only forces with a player are captured (plus any the host names: the standalone mod's setting, the test seam). Read
--- at install, at every config and at every player creation, force change, removal and merge, all of which run on
--- every peer.
function terrain.refresh_forces(st)
	if not st then
		return
	end
	local forces = {}
	for _, p in pairs(game.players) do
		forces[p.force.index] = true
	end
	for fname in pairs(st.extra_forces or {}) do
		local f = game.forces[fname]
		if f then
			forces[f.index] = true
		end
	end
	st.forces = forces
end

-- Surface names and planets by index, for the clip (only read with a gridworld tile). Cleared whenever a surface is
-- created, deleted, renamed or imported.
local surface_info = {}

function terrain.forget_surfaces()
	surface_info = {}
end

--- A deleted surface's index can be reused, so the next surface must not inherit its last-sent charts.
function terrain.on_surface_deleted(e)
	surface_info = {}
	local si = e.surface_index
	for _, gen in ipairs({ cache.young, cache.old }) do
		local keys = {}
		for key in pairs(gen) do
			if core.key_surface(key) == si then
				keys[#keys + 1] = key
			end
		end
		for _, key in ipairs(keys) do
			core.cache_del(cache, key)
		end
	end
end

--- on_chunk_charted: a captured chunk of a captured force joins the queue through the debounce. A forced push (a
--- deletion, a resync) skips the debounce and the cap. This runs for every chart refresh of every chunk, so it touches
--- the game as little as it can: the force index, and the surface only when a gridworld tile clips.
function terrain.on_chunk_charted(st, e, forced)
	if not (st and st.configured) then
		return
	end
	local fi = e.force.index
	if not st.forces[fi] then
		return
	end
	local p, si = e.position, e.surface_index
	if st.tile then
		local info = surface_info[si]
		if not info then
			local s = game.get_surface(si)
			if not s then
				return
			end
			info = { name = s.name, planet = core.planet(s) }
			surface_info[si] = info
		end
		if not core.captured(st.tile, st.bounds, info.name, p.x, p.y, info.planet) then
			return
		end
	end
	local key = core.key(si, fi, p.x, p.y)
	if not forced then
		-- what core.charted would do, inlined: most events of a busy map end here
		if deb.pending[key] or deb.dirty[key] then
			return
		end
	end
	core.charted(deb, key, forced)
end

--- A deleted chunk is read again for each captured force, and its nil chart sends a chunk message without d. Forced:
--- the cache may not know the chunk, and nil == nil would swallow a deletion the server needs.
function terrain.on_chunk_deleted(st, e)
	if not (st and st.configured) then
		return
	end
	for _, p in pairs(e.positions) do
		for fi in pairs(st.forces) do
			terrain.on_chunk_charted(st, { surface_index = e.surface_index, force = game.forces[fi], position = p }, true)
		end
	end
end

--- Read one queued chunk. A forced read is sent though unchanged.
local function read(st, key, forced, r)
	local si, fi, x, y = core.unkey(key)
	local force, surface = game.forces[fi], game.get_surface(si)
	if not (force and surface) then
		return -- a deleted surface's chunks are skipped
	end
	local pos = { x = x, y = y }
	local chart = chart_of(force, surface, pos)
	reads = reads + 1
	if forced then
		deb.raw[key] = nil
		if r then
			overlays.chunk_resync(st, surface, force, pos, r)
		end
	else
		-- the same raw chart as this chunk's last event read: unchanged, nothing to encode
		local raw = deb.raw[key]
		deb.raw[key] = chart
		if raw ~= nil and raw == chart then
			skipped = skipped + 1
			return
		end
	end
	local d = chart and helpers.encode_string(chart) or false
	if not forced and core.cache_get(cache, key) == d then
		return
	end
	core.cache_set(cache, key, d)
	sent = sent + 1
	outbox.send{ type = "chunk", s = surface.name, f = force.name, x = x, y = y, d = d or nil }
end

--- The per-tick step: end due cool-downs, then read up to core.CHARTS queued chunks (core.RESYNC_CHARTS while a resync
--- runs), forced ones first. While a resync runs the event queue keeps one of those reads whenever it is not empty, so
--- neither starves the other. The resync's sentinel marks its chunks done. Returns the number of reads.
function terrain.step(st, tick, r)
	core.cooled(deb, tick)
	local n = 0
	local limit = r and math.max(core.CHARTS, core.RESYNC_CHARTS) or core.CHARTS
	local forced = limit
	if r and limit > 1 and core.size(deb.queue) > 0 then
		forced = limit - 1
	end
	while n < forced do
		local key = core.take(deb, tick, "forced")
		if not key then
			break
		end
		if type(key) == "table" then
			if r and r.walk == key.resynced then
				r.chunks_done = true
			end
		else
			read(st, key, true, r)
			n = n + 1
		end
	end
	while n < limit do
		local key = core.take(deb, tick, "events")
		if not key then
			break
		end
		read(st, key, false, r)
		n = n + 1
	end
	return n
end

--- Each surface's name and whether it is one of the tile's, at most core.SURFACES_MAX a message.
function terrain.surfaces(st)
	local list = {}
	for _, s in pairs(game.surfaces) do
		if #list == core.SURFACES_MAX then
			outbox.send{ type = "surfaces", list = list }
			list = {}
		end
		list[#list + 1] = { name = s.name, tile = core.tiled(st.tile, s.name, core.planet(s)) }
	end
	outbox.send{ type = "surfaces", list = list }
end

--- A surface made mid-game is listed before its first chunk, in a surfaces message of its own flagged created.
function terrain.on_surface_created(st, e)
	surface_info = {}
	local s = game.get_surface(e.surface_index)
	if st and st.configured and s then
		outbox.send{ type = "surfaces", list = { { name = s.name, tile = core.tiled(st.tile, s.name, core.planet(s)) } },
			created = true }
	end
end

--- Start the resync's walk of every charted captured chunk, pushed forced and past the cap, then a sentinel. A newer
--- resync replaces one still running; it gets a new walk number (r.walk), so an older walk's sentinel still queued
--- cannot end it, even when both resyncs share a trace. It writes no storage, so it can start on one peer only.
function terrain.resync(st, r)
	local list = {}
	for _, s in pairs(game.surfaces) do
		list[#list + 1] = s.index
	end
	walks = walks + 1
	r.walk = walks
	walk = { list = list, k = 0, id = walks }
end

--- Advance the walk: at most core.RESYNC_READS chunk positions a tick, stopping once core.RESYNC_CHUNKS charted chunks
--- are queued, and not at all while core.RESYNC_QUEUE reads wait. Each surface is walked with its own get_chunks()
--- iterator, kept across ticks.
function terrain.walk(st, r)
	local w = walk
	if not (w and r and w.id == r.walk) then
		return
	end
	if core.size(deb.forced) >= core.RESYNC_QUEUE then
		return -- the reads catch up first: a walk ahead of them would only grow the forced queue
	end
	local positions, pushed = 0, 0
	while positions < core.RESYNC_READS and pushed < core.RESYNC_CHUNKS do
		local s = w.si and game.get_surface(w.si)
		local c
		if s and w.it then
			local ok, v = pcall(w.it)
			if ok then
				c = v
			else
				outbox.send{ type = "trace", op = "timeshade.resync", phase = "iterator", level = "warn",
					error = tostring(v), surface = s.name }
			end
		end
		if c == nil then
			w.k = w.k + 1
			if w.k > #w.list then
				core.charted(deb, { resynced = w.id }, true) -- the sentinel
				walk = nil
				return
			end
			w.si = w.list[w.k]
			s = game.get_surface(w.si)
			w.it = s and s.get_chunks() or nil
		else
			positions = positions + 1
			if core.captured(st.tile, st.bounds, s.name, c.x, c.y, core.planet(s)) then
				local pos = { x = c.x, y = c.y }
				for fi in pairs(st.forces) do
					local force = game.forces[fi]
					if force and is_charted(force, s, pos) then
						core.charted(deb, core.key(s.index, fi, c.x, c.y), true)
						r.chunks, pushed = r.chunks + 1, pushed + 1
					end
				end
			end
		end
	end
end

--- Forget the file-local state (tests).
function terrain.reset()
	deb, cache, walk, reads, sent, skipped = core.debounce(), core.cache(core.CACHE), nil, 0, 0, 0
end

return terrain
