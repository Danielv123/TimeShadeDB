-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap module/overlays.lua (Blue Oak Model License 1.0.0, see capture/README.md).
--
-- Overlays (docs/PROTOCOL.md section 3): chart tags, display panels shown on the map, machine recipes and, when
-- enabled, rail entities. Each change sends the new state; the server closes the old one. Removal is seen through the
-- mined and died events, so nothing is registered with script.register_on_object_destroyed and the resync can stay
-- read-only.
--
-- The resync's queue of machines, rails and panels, and the display panels read in turn, are file-locals: they only
-- feed printed lines.
local name = ...
local P = type(name) == "string" and name:match("^(.*[./])[^./]*$") or ""
local core = require(P .. "core")
local outbox = require(P .. "outbox")

local overlays = {}

local MACHINE = { ["assembling-machine"] = true, furnace = true }
local MACHINE_TYPES = { "assembling-machine", "furnace" }

--- Rail entity types of Factorio 2.0 (checked in the headless game test, capture/test/game). Legacy rails are what a
--- 1.1 save's rails became; elevated ones come with the elevated-rails mod. Ramps and supports are not track.
overlays.RAIL = {
	["straight-rail"] = true,
	["half-diagonal-rail"] = true,
	["curved-rail-a"] = true,
	["curved-rail-b"] = true,
	["elevated-straight-rail"] = true,
	["elevated-half-diagonal-rail"] = true,
	["elevated-curved-rail-a"] = true,
	["elevated-curved-rail-b"] = true,
	["legacy-straight-rail"] = true,
	["legacy-curved-rail"] = true,
	["rail-ramp"] = true,
}

overlays.PANEL = "display-panel"

local present
--- The entity types of `set` this game has, sorted (a type missing from the prototypes would make
--- find_entities_filtered fail).
local function types_of(set)
	if not present then
		present = {}
		for _, p in pairs(prototypes.entity) do
			present[p.type] = true
		end
	end
	local list = {}
	for t in pairs(set) do
		if present[t] then
			list[#list + 1] = t
		end
	end
	table.sort(list)
	return list
end

local rail_types, panel_types
--- The rail types this game has.
local function rail_list()
	if not rail_types then
		rail_types = types_of(overlays.RAIL)
	end
	return rail_types
end

--- The display panel type, if this game has it: { "display-panel" } or {}.
local function panel_list()
	if not panel_types then
		panel_types = types_of{ [overlays.PANEL] = true }
	end
	return panel_types
end

local queue = core.queue()

--- Whether the chunk under position is captured.
local function captured(st, surface, position)
	return core.captured(st.tile, st.bounds, surface.name, math.floor(position.x / 32), math.floor(position.y / 32),
		core.planet(surface))
end

--- A tag's new state, or { s, f, n } on removal, for captured forces and chunks. Returns true when it sent one. icon is
--- "<signal type>/<name>" (a SignalID reads type nil for an item), or "" without one.
function overlays.on_tag(st, e)
	if not (st and st.configured and st.forces[e.force.index]) then
		return false
	end
	local tag = e.tag
	local surface, p = tag.surface, tag.position
	if not captured(st, surface, p) then
		return false
	end
	local msg = { type = "tag", s = surface.name, f = e.force.name, n = tag.tag_number }
	if e.name ~= defines.events.on_chart_tag_removed then
		local icon = tag.icon
		msg.x, msg.y, msg.text = p.x, p.y, tag.text
		msg.icon = icon and icon.name and ((icon.type or "item") .. "/" .. icon.name) or ""
	end
	outbox.send(msg)
	return true
end

--- A force's tags on a surface. The game tests replace it: headless has no charted chunk to hold a tag.
function overlays.find_tags(force, surface)
	return force.find_chart_tags(surface)
end

--- Every tag of every captured force on a captured chunk. Returns the count.
function overlays.resync_tags(st)
	local n = 0
	for fi in pairs(st.forces) do
		local force = game.forces[fi]
		for _, surface in pairs(game.surfaces) do
			for _, tag in pairs(overlays.find_tags(force, surface)) do
				if overlays.on_tag(st, { name = defines.events.on_chart_tag_added, force = force, tag = tag }) then
					n = n + 1
				end
			end
		end
	end
	return n
end

--- A prototype's name, from a RecipeID or QualityID read (a name, or an object with one).
local function id(v)
	return type(v) == "string" and v or v.name
end

--- A machine's recipe and quality, from get_recipe() or, idle, a furnace's previous_recipe. Without either it sends
--- { s, f, u }, which closes the open version. No dedupe here: the server drops repeats.
function overlays.send_machine(e)
	local msg = { type = "recipe", s = e.surface.name, f = e.force.name, u = e.unit_number }
	local recipe, quality = e.get_recipe()
	if recipe then
		msg.r, msg.q = recipe.name, quality and quality.name or "normal"
	elseif e.type == "furnace" and e.previous_recipe then
		msg.r, msg.q = id(e.previous_recipe.name), id(e.previous_recipe.quality)
	end
	if msg.r then
		msg.x, msg.y = e.position.x, e.position.y
	end
	outbox.send(msg)
end

--- A rail's state: name, position and direction.
function overlays.send_rail(e)
	outbox.send{ type = "rail", s = e.surface.name, f = e.force.name, u = e.unit_number, name = e.name,
		x = e.position.x, y = e.position.y, dir = e.direction }
end

local function wanted(st, entity)
	return st and st.configured and st.forces[entity.force.index] and captured(st, entity.surface, entity.position)
end

-- Display panels with "show in chart" put their icon and text on the map, as a chart tag does. Nothing tells when that
-- changes (a circuit network picks the message), so the known panels are read in turn: each at most every
-- core.PANEL_POLL ticks, core.PANELS a tick. A panel sends its state at most every core.PANEL_GAP ticks, and all panels
-- together at most core.PANEL_RATE a minute, so a panel driven by a fast circuit cannot flood the database. A change
-- held back goes out at a later read, so the last state sent is the one the panel ends up showing.
--
-- A panel is known from its build (or a closed GUI, a paste) and from the resync's chunks, which run at every load.
-- panels: unit number -> { e, u, s, f, x, y, icon, text, next, due, gone }, where icon and text are what was sent last
-- (icon nil: nothing open, or nothing sent), next the tick the next send may go and due the tick of the next read. ring
-- holds them in order of due (every push is due core.PANEL_POLL ticks later); gone ones are dropped as they come up.
local panels, panel_count, ring, bucket = {}, 0, core.queue(64), {}

--- What panel e shows on the map: its icon ("<signal type>/<name>", or "" without one) and text, or nil when it shows
--- nothing there (chart display off, or no icon and no text, as a circuit panel whose conditions all fail).
local function panel_state(e)
	if not e.display_panel_show_in_chart then
		return nil
	end
	local icon, text = e.display_panel_icon, e.display_panel_text or ""
	icon = icon and icon.name and ((icon.type or "item") .. "/" .. icon.name) or ""
	if icon == "" and text == "" then
		return nil
	end
	return icon, text
end

--- Send panel record r's state: icon and text, or (icon nil) a removal.
local function send_panel(r, icon, text)
	local msg = { type = "panel", s = r.s, f = r.f, u = r.u }
	if icon then
		msg.x, msg.y, msg.icon, msg.text = r.x, r.y, icon, text
	end
	outbox.send(msg)
	r.icon, r.text = icon, text
end

--- Stop reading panel record r. With `close` (it was removed) its state closes unless nothing was sent for it.
local function forget(r, close)
	if r.gone then
		return
	end
	r.gone = true
	panels[r.u] = nil
	panel_count = panel_count - 1
	if close and r.icon then
		send_panel(r, nil)
	end
end

--- The record of panel e, made (and queued for reads) when new.
local function track(e, tick)
	local u = e.unit_number
	local r = panels[u]
	if r then
		r.e = e
		return r
	end
	local p = e.position
	r = { e = e, u = u, s = e.surface.name, f = e.force.name, x = p.x, y = p.y, next = 0, due = tick + core.PANEL_POLL }
	panels[u] = r
	panel_count = panel_count + 1
	core.push(ring, r)
	return r
end

--- Read panel record r; send its state when it changed and both limits allow.
local function check(r, tick)
	local e = r.e
	if not e.valid then
		forget(r, true) -- gone without a mined event (its surface deleted, destroyed by a script without raising)
		return
	end
	local icon, text = panel_state(e)
	if icon == r.icon and text == r.text then
		return
	end
	if tick < r.next or not core.token(bucket, tick, core.PANEL_RATE) then
		return -- the next read sends it, if it still differs
	end
	send_panel(r, icon, text)
	r.next = tick + core.PANEL_GAP
end

--- A resync's panel: its state now, past both limits (a removal too, so the server closes what it no longer shows).
local function resend_panel(e, tick)
	local r = track(e, tick)
	local icon, text = panel_state(e)
	send_panel(r, icon, text)
end

--- The per-tick read: the panels that are due, at most core.PANELS.
function overlays.poll(tick)
	for _ = 1, core.PANELS do
		local r = core.peek(ring)
		if not r or r.due > tick then
			return
		end
		core.pop(ring)
		if not r.gone then
			check(r, tick)
			if not r.gone then
				r.due = tick + core.PANEL_POLL
				core.push(ring, r)
			end
		end
	end
end

--- Display panels being read.
function overlays.panels()
	return panel_count
end

--- A build's entity, a paste's destination, a closed GUI's entity: machines send their recipe, rails (when enabled)
--- their state, display panels theirs (within the limits) and are read from then on.
function overlays.on_entity(st, e)
	local entity = e.entity or e.destination
	if not (entity and entity.valid) then
		return
	end
	if entity.type == overlays.PANEL then
		if wanted(st, entity) then
			check(track(entity, game.tick), game.tick)
		end
		return
	end
	local machine = MACHINE[entity.type]
	local rail = st and st.rails and overlays.RAIL[entity.type] and e.name ~= defines.events.on_gui_closed
		and e.name ~= defines.events.on_entity_settings_pasted
	if not ((machine or rail) and wanted(st, entity)) then
		return
	end
	if machine then
		overlays.send_machine(entity)
	else
		overlays.send_rail(entity)
	end
end

--- A mined or destroyed entity: a machine's recipe, a rail or a display panel's map tag closes (a panel's at once,
--- whatever the limits: it happens once).
function overlays.on_removed(st, e)
	local entity = e.entity
	if not (entity and entity.valid) then
		return
	end
	if entity.type == overlays.PANEL then
		local r = panels[entity.unit_number]
		if r then
			forget(r, true)
		elseif wanted(st, entity) then
			-- not read yet (built before a load whose resync has not reached it): whatever the server holds closes
			outbox.send{ type = "panel", s = entity.surface.name, f = entity.force.name, u = entity.unit_number }
		end
		return
	end
	local machine = MACHINE[entity.type]
	local rail = st and st.rails and overlays.RAIL[entity.type]
	if not ((machine or rail) and wanted(st, entity)) then
		return
	end
	outbox.send{ type = machine and "recipe" or "rail", s = entity.surface.name, f = entity.force.name,
		u = entity.unit_number }
end

--- A resync's forced chunk: its machines, display panels (and rails) join the resync's queue. An entity across two
--- chunks is sent twice; the server drops the repeat.
function overlays.chunk_resync(st, surface, force, pos, r)
	local x, y = pos.x * 32, pos.y * 32
	local area = { { x, y }, { x + 32, y + 32 } }
	for _, e in pairs(surface.find_entities_filtered{ area = area, type = MACHINE_TYPES, force = force }) do
		core.push(queue, e)
		r.machines = r.machines + 1
	end
	if #panel_list() > 0 then
		for _, e in pairs(surface.find_entities_filtered{ area = area, type = panel_list(), force = force }) do
			core.push(queue, e)
			r.panels = (r.panels or 0) + 1
		end
	end
	if st.rails then
		for _, e in pairs(surface.find_entities_filtered{ area = area, type = rail_list(), force = force }) do
			core.push(queue, e)
			r.rails = (r.rails or 0) + 1
		end
	end
end

--- The per-tick step: at most core.MACHINES of the resync's queue.
function overlays.step(tick)
	tick = tick or game.tick
	for _ = 1, core.MACHINES do
		local e = core.pop(queue)
		if not e then
			return
		end
		if e.valid then
			if MACHINE[e.type] then
				overlays.send_machine(e)
			elseif e.type == overlays.PANEL then
				resend_panel(e, tick)
			else
				overlays.send_rail(e)
			end
		end
	end
end

--- Entities waiting in the resync's queue.
function overlays.waiting()
	return core.size(queue)
end

function overlays.reset()
	queue = core.queue()
	panels, panel_count, ring, bucket = {}, 0, core.queue(64), {}
end

return overlays
