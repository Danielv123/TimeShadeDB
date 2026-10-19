-- Loads the timeshade module the way Clusterio's save patcher writes control.lua (clusterio/packages/host/src/patch.ts:
-- event_handler.add_lib for each `load` file, then require for each `require` file), then runs the scenario through
-- the module's RCON entry point ts_rt. The instance plugin's RCON calls are made from on_tick here; both run on every
-- peer. Output is the module's IPC lines on stdout. With config no_capture (the UPS bench's baseline, run_game.mjs
-- --host clusterio) the module is not loaded and the scenario's capture calls do nothing.
local event_handler = require("event_handler")
local cfg = require("config")
local with_capture = not cfg.no_capture
if with_capture then
	event_handler.add_lib(require("modules/timeshade/control"))
	require("modules/timeshade/globals")
end

local clusterio_api = require("modules/clusterio/api")
local capture = with_capture and require("modules/timeshade/capture") or nil
local scenario = require("scenario")
local synthetic = require("synthetic")
local mode = assert(scenario.modes[cfg.mode], "unknown mode " .. tostring(cfg.mode))

local function none()
end

local api = with_capture and {
	config = function(c)
		ts_rt.config(helpers.table_to_json(c))
	end,
	resync = function(trace)
		ts_rt.resync(trace)
	end,
	synthetic = function(spec)
		ts_rt.seam.chart(synthetic.seam(spec))
	end,
	charted = function(surface, force, list)
		for _, p in ipairs(list) do
			ts_rt.seam.charted(surface, force, p[1], p[2])
		end
	end,
	stats = function()
		return capture.stats()
	end,
	memory = function()
		collectgarbage("collect")
		return collectgarbage("count")
	end,
} or { config = none, resync = none, synthetic = none, charted = none, stats = none, memory = none }

event_handler.add_lib({
	on_init = function()
		mode.init(cfg)
	end,
	events = {
		[defines.events.on_tick] = function(e)
			if e.tick == 1 then
				-- what Clusterio raises at every start, then the instance plugin's config and resync
				script.raise_event(clusterio_api.events.on_server_startup, {})
			end
			mode.tick(api, cfg, e.tick)
			if e.tick == 1 and with_capture then
				ts_rt.resync("start-1")
			end
		end,
	},
})
