-- TimeShadeDB capture, standalone mod (docs/v3/PROTOCOL.md section 2, "Standalone").
--
-- The shared capture library (capture/lua, copied into lua/ by capture/build.mjs) writes envelope lines here into
-- script-output/timeshade/<seq>-<tick>.ndjson; `timeshadedb tail --dir <script-output>/timeshade` posts complete files
-- to the server and deletes them.
--
-- Desync rule: storage changes only in events, in on_tick steps and in capture.config (on_init,
-- on_configuration_changed, a settings change), all of which run on every peer. The file name and the resync at each
-- load only decide what is written.
local capture = require("lua.capture")

local ROTATE_TICKS = 3600 -- a new file every 60 s of game time
local MAX_BYTES = 4 * 1024 * 1024 -- or once a file holds 4 MiB

local file, file_bytes, file_seq, file_tick = nil, 0, nil, nil
local booted = false

local function setting(name)
	return settings.global["timeshade-capture-" .. name].value
end

local function forces()
	local list = {}
	for name in string.gmatch(setting("forces"), "[^,%s]+") do
		list[#list + 1] = name
	end
	return list
end

local function config()
	capture.config{ forces = forces(), rails = setting("rails"), debounce = setting("debounce"),
		charts = setting("chart-reads"), cache = setting("cache") }
	local st = capture.state()
	if not setting("enabled") then
		st.configured = false
	end
	return st
end

--- Where lines go: append to the current file, opening a new one when the storage counter moved (every ROTATE_TICKS,
--- on every peer alike) or the file is full. In multiplayer only the server writes.
local function sink(line, tick)
	local st = capture.state()
	if file == nil or file_seq ~= st.file_seq or (file_bytes >= MAX_BYTES and tick ~= file_tick) then
		file_seq, file_tick, file_bytes = st.file_seq, tick, 0
		file = string.format("timeshade/%010d-%010d.ndjson", file_seq, tick)
	end
	helpers.write_file(file, line .. "\n", true, game.is_multiplayer() and 0 or nil)
	file_bytes = file_bytes + #line + 1
end

capture.setup{
	sink = sink,
	instance = function()
		return 0
	end,
	on_tick = function(st, e)
		if e.tick % ROTATE_TICKS == 0 then
			st.file_seq = st.file_seq + 1
		end
		if not booted then
			-- every load starts with a resync: the cache of sent charts is empty after a load
			booted = true
			capture.resync("load-" .. e.tick)
		end
	end,
}

local events = capture.build_events()
for id, handler in pairs(events) do
	script.on_event(id, handler)
end

script.on_init(function()
	local st = config()
	st.file_seq = st.file_seq + 1
end)

script.on_configuration_changed(function()
	local st = config()
	st.file_seq = st.file_seq + 1
end)

script.on_load(function()
	capture.on_load()
end)

script.on_event(defines.events.on_runtime_mod_setting_changed, function(e)
	if e.setting:sub(1, #"timeshade-capture-") == "timeshade-capture-" then
		local st = capture.state()
		local was = st and st.configured
		config()
		if not was and capture.state().configured then
			booted = false -- turned on again: resync on the next tick
		end
	end
end)

remote.add_interface("timeshade_capture", {
	--- Start a resync now (read-only, safe on any peer).
	resync = function(trace)
		return capture.resync(trace or ("remote-" .. game.tick))
	end,
	--- Counters: reads, sent, queued, drops, cached, lines, bytes, oversize, movers, resyncing.
	stats = function()
		return capture.stats()
	end,
})

commands.add_command("timeshade", "TimeShadeDB capture: /timeshade stats | resync", function(cmd)
	local player = cmd.player_index and game.get_player(cmd.player_index)
	local function say(msg)
		if player then
			player.print(msg)
		else
			print(msg)
		end
	end
	if cmd.parameter == "resync" then
		capture.resync("command-" .. game.tick)
		say("timeshade: resync started")
	else
		say("timeshade: " .. helpers.table_to_json(capture.stats()))
	end
end)
