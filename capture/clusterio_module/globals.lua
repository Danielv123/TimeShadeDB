-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap module/globals.lua (Blue Oak Model License 1.0.0, see capture/README.md).
--
-- The capture's RCON entry point, the one global this module defines. The instance plugin (clusterio/instance.ts) calls, at every start,
-- on a gridworld.tile change and on reconnect:
--   /sc ts_rt.config('{"tile":{...},"rails":false}')  then  /sc ts_rt.resync("<trace>")
-- Headless servers never chart, so the game tests and end-to-end tests stand charts in through ts_rt.seam.
local capture = require("modules/timeshade/capture")

ts_rt = {
	--- ts_rt.config(json): { tile?, rails?, forces?, debounce?, charts?, cache? } (capture.config). Raises on a bad field,
	--- and RCON returns the error.
	config = function(json)
		local c = helpers.json_to_table(json)
		assert(type(c) == "table", "timeshade config: not a JSON object: " .. tostring(json))
		capture.config(c)
	end,
	--- ts_rt.resync(trace): resync now; a resynced message with the trace follows the last chunk.
	resync = function(trace)
		capture.resync(trace)
	end,
	--- ts_rt.stats(): the capture's counters as JSON, for RCON.
	stats = function()
		rcon.print(helpers.table_to_json(capture.stats()))
	end,
	seam = {
		--- ts_rt.seam.chart(fn): fn(force, surface, position) -> a 2,048-byte chart or nil stands in for get_chunk_chart
		--- and is_chunk_charted. nil restores the engine.
		chart = function(fn)
			capture.terrain.seam = fn
		end,
		--- ts_rt.seam.charted("<surface>", "<force>", cx, cy): on_chunk_charted for that chunk.
		charted = function(surface, force, cx, cy)
			capture.terrain.on_chunk_charted(storage.timeshade, { surface_index = game.get_surface(surface).index,
				force = game.forces[force], position = { x = cx, y = cy } })
		end,
	},
}
