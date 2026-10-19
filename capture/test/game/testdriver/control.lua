-- The game-test driver for the standalone mod: runs scenario.lua against timeshade_capture's test hooks (a remote
-- interface present only in a `build.mjs --test` build). Without the capture mod (the UPS baseline) it drives the same
-- world and calls nothing.
local scenario = require("scenario")
local cfg = require("config")
local mode = assert(scenario.modes[cfg.mode], "unknown mode " .. tostring(cfg.mode))

local function api()
	if not remote.interfaces["timeshade_capture_test"] then
		return {}
	end
	local function r(name)
		return function(...)
			return remote.call("timeshade_capture_test", name, ...)
		end
	end
	return { config = r("config"), resync = r("resync"), synthetic = r("synthetic_charts"), charted = r("charted"),
		stats = r("stats"), memory = r("memory"), constants = r("constants"), read_cost = r("read_cost") }
end

script.on_init(function()
	mode.init(cfg)
end)

script.on_event(defines.events.on_tick, function(e)
	mode.tick(api(), cfg, e.tick)
	if cfg.read_cost and e.tick == cfg.read_cost and remote.interfaces["timeshade_capture_test"] then
		remote.call("timeshade_capture_test", "read_cost", 1000)
	end
end)
