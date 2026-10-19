-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap module/overlays.lua (Blue Oak Model License 1.0.0, see capture/README.md).
--
-- Overlays (docs/v3/PROTOCOL.md section 3): chart tags, machine recipes and, when enabled, rail entities. Each change
-- sends the new state; the server closes the old one. Removal is seen through the mined and died events, so nothing is
-- registered with script.register_on_object_destroyed and the resync can stay read-only.
--
-- The resync's queue of machines and rails is a file-local: it only feeds printed lines.
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

local rail_types
--- The rail types this game has (a type missing from the prototypes would make find_entities_filtered fail).
local function rail_list()
	if not rail_types then
		rail_types = {}
		local present = {}
		for _, p in pairs(prototypes.entity) do
			present[p.type] = true
		end
		for t in pairs(overlays.RAIL) do
			if present[t] then
				rail_types[#rail_types + 1] = t
			end
		end
		table.sort(rail_types)
	end
	return rail_types
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

--- A build's entity, a paste's destination, a closed GUI's entity: machines send their recipe, rails (when enabled)
--- their state.
function overlays.on_entity(st, e)
	local entity = e.entity or e.destination
	if not (entity and entity.valid) then
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

--- A mined or destroyed entity: a machine's recipe or a rail closes.
function overlays.on_removed(st, e)
	local entity = e.entity
	if not (entity and entity.valid) then
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

--- A resync's forced chunk: its machines (and rails) join the resync's queue. An entity across two chunks is sent twice;
--- the server drops the repeat.
function overlays.chunk_resync(st, surface, force, pos, r)
	local x, y = pos.x * 32, pos.y * 32
	local area = { { x, y }, { x + 32, y + 32 } }
	for _, e in pairs(surface.find_entities_filtered{ area = area, type = MACHINE_TYPES, force = force }) do
		core.push(queue, e)
		r.machines = r.machines + 1
	end
	if st.rails then
		for _, e in pairs(surface.find_entities_filtered{ area = area, type = rail_list(), force = force }) do
			core.push(queue, e)
			r.rails = (r.rails or 0) + 1
		end
	end
end

--- The per-tick step: at most core.MACHINES of the resync's queue.
function overlays.step()
	for _ = 1, core.MACHINES do
		local e = core.pop(queue)
		if not e then
			return
		end
		if e.valid then
			if MACHINE[e.type] then
				overlays.send_machine(e)
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
end

return overlays
