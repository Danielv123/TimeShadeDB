-- The game-test driver for the standalone mod: runs scenario.lua against timeshadedb_exporter's test hooks (a remote
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
	if cfg.profile and remote.interfaces["timeshade_capture_test"] then
		if e.tick == (cfg.profile_start or 1) then
			remote.call("timeshade_capture_test", "profile")
		elseif e.tick == cfg.profile then
			remote.call("timeshade_capture_test", "profile_report")
		end
	end
	if cfg.micro and e.tick == cfg.micro and remote.interfaces["timeshade_capture_test"] then
		remote.call("timeshade_capture_test", "micro", 10000)
		local list = {}
		for i = 1, 20 do
			list[i] = { i, i }
		end
		local prof = helpers.create_profiler()
		for _ = 1, 1000 do
			remote.call("timeshade_capture_test", "charted", "ts-lab", "player", {})
		end
		prof.stop()
		log({ "", "TSTEST micro remote.call charted (empty list) x1000 ", prof })
	end
	mode.tick(api(), cfg, e.tick)
	if cfg.read_cost and e.tick == cfg.read_cost and remote.interfaces["timeshade_capture_test"] then
		remote.call("timeshade_capture_test", "read_cost", 1000)
	end
end)
