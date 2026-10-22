local chunk_output_path = "timeshadedb/chunk-charted.tsv"
local chunk_size = 32

local function state()
  local s = storage.timeshadedb_exporter or {}
  s.tile_queue = s.tile_queue or {}
  s.tile_queue_head = s.tile_queue_head or 1
  s.tile_queued = s.tile_queued or {}
  s.tile_payloads = s.tile_payloads or {}
  storage.timeshadedb_exporter = s
  return s
end

local function setting(name)
  return settings.global[name].value
end

local function chunk_key(surface_index, force_name, chunk)
  return tostring(surface_index) .. ":" .. force_name .. ":" .. tostring(chunk.x) .. ":" .. tostring(chunk.y)
end

local function queue_chunk(surface, force, chunk)
  if not surface or not force or not chunk then
    return
  end

  local s = state()
  local key = chunk_key(surface.index, force.name, chunk)
  if s.tile_queued[key] then
    return
  end

  s.tile_queued[key] = true
  table.insert(s.tile_queue, {
    surface_index = surface.index,
    force = force.name,
    x = chunk.x,
    y = chunk.y
  })
end

local function is_chunk_charted(force, surface, chunk)
  if not force or not surface or not chunk or not force.is_chunk_charted then
    return false
  end
  local ok, charted = pcall(function()
    return force.is_chunk_charted(surface, chunk)
  end)
  return ok and charted
end

local function queue_chunk_for_charted_forces(surface, chunk)
  for _, force in pairs(game.forces) do
    if is_chunk_charted(force, surface, chunk) then
      queue_chunk(surface, force, chunk)
    end
  end
end

local function enqueue_charted_chunks()
  for _, surface in pairs(game.surfaces) do
    for _, force in pairs(game.forces) do
      if force.get_charted_chunks then
        local ok, chunks = pcall(function()
          return force.get_charted_chunks(surface)
        end)
        if ok and chunks then
          for _, chunk in pairs(chunks) do
            queue_chunk(surface, force, chunk)
          end
        end
      end
    end
  end
end

local function color_component(value)
  if not value then
    return 0
  end
  if value <= 1 then
    value = value * 255
  end
  value = math.floor(value + 0.5)
  if value < 0 then
    return 0
  end
  if value > 255 then
    return 255
  end
  return value
end

local function rgb565_for_tile(tile)
  local color = tile and tile.valid and tile.prototype and tile.prototype.map_color
  local r = color_component(color and color.r)
  local g = color_component(color and color.g)
  local b = color_component(color and color.b)
  return math.floor(r * 31 / 255 + 0.5) * 2048 + math.floor(g * 63 / 255 + 0.5) * 32 + math.floor(b * 31 / 255 + 0.5)
end

local function chunk_payload_hex(surface, chunk)
  local parts = {}
  local i = 1
  local origin_x = chunk.x * chunk_size
  local origin_y = chunk.y * chunk_size
  for y = 0, chunk_size - 1 do
    for x = 0, chunk_size - 1 do
      local value = rgb565_for_tile(surface.get_tile(origin_x + x, origin_y + y))
      parts[i] = string.format("%02x%02x", value % 256, math.floor(value / 256))
      i = i + 1
    end
  end
  return table.concat(parts)
end

local function export_chunk(item)
  local surface = game.get_surface(item.surface_index)
  local force = game.forces[item.force]
  if not surface or not force then
    return
  end

  local chunk = { x = item.x, y = item.y }
  if not is_chunk_charted(force, surface, chunk) then
    return
  end

  local payload = chunk_payload_hex(surface, chunk)
  local key = chunk_key(surface.index, force.name, chunk)
  local s = state()
  if s.tile_payloads[key] == payload then
    return
  end
  s.tile_payloads[key] = payload

  local line = table.concat({
    tostring(game.tick),
    surface.name,
    tostring(chunk.x),
    tostring(chunk.y),
    force.name,
    payload
  }, "\t") .. "\n"
  game.write_file(chunk_output_path, line, true)
end

local function process_tile_queue()
  local s = state()
  local limit = setting("timeshadedb-tile-export-chunks-per-tick")
  local processed = 0
  local head = s.tile_queue_head or 1
  while processed < limit and head <= #s.tile_queue do
    local item = s.tile_queue[head]
    head = head + 1
    s.tile_queued[chunk_key(item.surface_index, item.force, { x = item.x, y = item.y })] = nil
    export_chunk(item)
    processed = processed + 1
  end
  if head > #s.tile_queue then
    s.tile_queue = {}
    s.tile_queue_head = 1
  elseif head > 1024 then
    local compacted = {}
    for i = head, #s.tile_queue do
      table.insert(compacted, s.tile_queue[i])
    end
    s.tile_queue = compacted
    s.tile_queue_head = 1
  else
    s.tile_queue_head = head
  end
end

local function tile_chunk_from_position(position)
  return {
    x = math.floor(position.x / chunk_size),
    y = math.floor(position.y / chunk_size)
  }
end

local function queue_tile_event_chunks(event)
  local surface = game.get_surface(event.surface_index)
  if not surface or not event.tiles then
    return
  end
  local seen = {}
  for _, tile in pairs(event.tiles) do
    local position = tile.position or tile
    if position then
      local chunk = tile_chunk_from_position(position)
      local key = tostring(chunk.x) .. ":" .. tostring(chunk.y)
      if not seen[key] then
        seen[key] = true
        queue_chunk_for_charted_forces(surface, chunk)
      end
    end
  end
end

local function on_tick(event)
  if event.tick % setting("timeshadedb-tile-rescan-interval") == 0 then
    enqueue_charted_chunks()
  end
  process_tile_queue()
end

local function initialize()
  local s = state()
  s.tile_queue = {}
  s.tile_queue_head = 1
  s.tile_queued = {}
  s.tile_payloads = {}
  s.entities, s.entity_samples, s.entity_segments = nil, nil, nil
  enqueue_charted_chunks()
end

local function append_event(events, event_id)
  if event_id then
    table.insert(events, event_id)
  end
end

script.on_init(initialize)
script.on_configuration_changed(initialize)
script.on_event(defines.events.on_tick, on_tick)

script.on_event(defines.events.on_runtime_mod_setting_changed, function(event)
  if string.find(event.setting, "timeshadedb-", 1, true) == 1 then
    state()
  end
end)

if defines.events.on_chunk_charted then
  script.on_event(defines.events.on_chunk_charted, function(event)
    local surface = game.get_surface(event.surface_index)
    local force = event.force or (event.force_index and game.forces[event.force_index])
    if surface and force and event.position then
      queue_chunk(surface, force, event.position)
    end
  end)
end

local tile_events = {}
append_event(tile_events, defines.events.on_player_built_tile)
append_event(tile_events, defines.events.on_player_mined_tile)
append_event(tile_events, defines.events.on_robot_built_tile)
append_event(tile_events, defines.events.on_robot_mined_tile)
append_event(tile_events, defines.events.script_raised_set_tiles)

script.on_event(tile_events, queue_tile_event_chunks)
