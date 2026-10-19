-- Test hooks, added to the mod only by `node capture/build.mjs --test` (never shipped): a remote interface the game-test
-- driver mod uses. Headless Factorio never charts, so charts come from a synthetic seam (synthetic.lua) whose bytes the
-- harness recomputes.
local synthetic = require("test.synthetic")

return function(capture)
	local spec = nil

	remote.add_interface("timeshade_capture_test", {
		--- Charts for chunks [x1, x2) x [y1, y2) of surface; flicker = { "x,y" = ticks per phase }.
		synthetic_charts = function(s)
			spec = s
			capture.terrain.seam = synthetic.seam(s)
		end,
		--- on_chunk_charted for each { x, y } in list.
		charted = function(surface, force, list)
			local si, f = game.get_surface(surface).index, game.forces[force]
			local handler = capture.events[defines.events.on_chunk_charted]
			for _, p in ipairs(list) do
				handler{ name = defines.events.on_chunk_charted, surface_index = si, force = f,
					position = { x = p[1], y = p[2] } }
			end
		end,
		config = function(c)
			capture.config(c)
		end,
		resync = function(trace)
			return capture.resync(trace)
		end,
		stats = function()
			local s = capture.stats()
			s.lua_kb = collectgarbage("count")
			return s
		end,
		--- Lua memory in KiB after a full collection (a test checkpoint only: it is slow).
		memory = function()
			collectgarbage("collect")
			return collectgarbage("count")
		end,
		constants = function()
			local c = {}
			for k, v in pairs(capture.core) do
				if type(v) == "number" then
					c[k] = v
				end
			end
			return c
		end,
		--- Time n synthetic chart builds plus helpers.encode_string and log it (a LuaProfiler prints to the log only).
		read_cost = function(n)
			local profiler = helpers.create_profiler()
			local total = 0
			for i = 1, n do
				total = total + #helpers.encode_string(synthetic.chart(i % 64, 0, 0))
			end
			profiler.stop()
			log("TSTEST read_cost_bytes " .. total)
			log({ "", "TSTEST read_cost ", n, " reads: ", profiler })
		end,
	})
end
