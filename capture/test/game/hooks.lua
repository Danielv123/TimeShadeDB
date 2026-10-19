-- Test hooks, added to the mod only by `node capture/build.mjs --test` (never shipped): a remote interface the game-test
-- driver mod uses. Headless Factorio never charts, so charts come from a synthetic seam (synthetic.lua) whose bytes the
-- harness recomputes.
local synthetic = require("test.synthetic")
-- test/real_charts.lua exists only in the bench's copy of the mod (run_game.mjs --real-charts)
local has_real, real_charts = pcall(require, "test.real_charts")

return function(capture)
	local spec = nil

	capture.remote_charted = {
		fn = function(surface, force, list)
			local si, f = game.get_surface(surface).index, game.forces[force]
			local handler = capture.events[defines.events.on_chunk_charted]
			for _, p in ipairs(list) do
				handler{ name = defines.events.on_chunk_charted, surface_index = si, force = f,
					position = { x = p[1], y = p[2] } }
			end
		end,
	}

	remote.add_interface("timeshade_capture_test", {
		--- Charts for chunks [x1, x2) x [y1, y2) of surface; flicker = { "x,y" = ticks per phase }.
		synthetic_charts = function(s)
			spec = s
			capture.terrain.seam = synthetic.seam(s)
			if s.real then
				-- test/real_charts.lua, written into the bench's copy of the mod by run_game.mjs --real-charts
				local list = {}
				for i, d in ipairs(has_real and real_charts or {}) do
					list[i] = helpers.decode_string(d)
				end
				synthetic.use(list)
				log("TSTEST micro real charts: " .. #list)
			end
		end,
		--- on_chunk_charted for each { x, y } in list.
		charted = function(surface, force, list)
			capture.remote_charted.fn(surface, force, list)
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
		--- Wrap the capture's per-tick steps and handlers in LuaProfilers that add up; profile_report logs the totals.
		--- Each wrapper resumes its profiler (restart does not reset) and stops it, so the totals are true sums; nested
		--- wrappers count in both (a wrapped call costs about a microsecond more).
		profile = function()
			local totals = {}
			local function wrap(tbl, name, label)
				local fn = tbl[name]
				local total = helpers.create_profiler(true)
				totals[#totals + 1] = { label = label, total = total, n = 0 }
				local entry = totals[#totals]
				tbl[name] = function(...)
					total.restart()
					local a, b, c, d = fn(...)
					total.stop()
					entry.n = entry.n + 1
					return a, b, c, d
				end
			end
			wrap(capture.terrain, "walk", "terrain.walk")
			wrap(capture.terrain, "step", "terrain.step (chart reads)")
			wrap(capture.core, "cooled", "  core.cooled")
			wrap(capture.terrain, "seam", "  terrain.seam (the synthetic chart)")
			wrap(capture.core, "off_chord", "    core.off_chord (inside core.train_read)")
			wrap(capture.overlays, "step", "overlays.step")
			wrap(capture.movers, "step", "movers.step")
			wrap(capture.core, "train_read", "  core.train_read")
			wrap(capture.outbox, "flush", "outbox.flush (JSON, cut, sink)")
			wrap(capture.outbox, "encode", "  outbox.encode")
			wrap(capture.core, "cut", "  core.cut")
			wrap(capture.outbox, "sink", "  outbox.sink (the mod's buffer)")
			wrap(capture.remote_charted, "fn", "charted remote (20 events, event tables built in Lua)")
			wrap(capture.terrain, "on_chunk_charted", "  terrain.on_chunk_charted")
			-- engine calls, through a stand-in for the global helpers (every other field passes)
			local real = helpers
			local stand = {
				write_file = function(...)
					return real.write_file(...)
				end,
				encode_string = function(...)
					return real.encode_string(...)
				end,
			}
			wrap(stand, "write_file", "helpers.write_file (the mod's file buffer)")
			wrap(stand, "encode_string", "helpers.encode_string")
			helpers = setmetatable(stand, { __index = function(_, k)
				return real[k]
			end })
			capture.profile_totals = totals
		end,
		profile_report = function()
			for _, e in ipairs(capture.profile_totals or {}) do
				log({ "", "TSTEST prof ", e.label, " calls ", e.n, " ", e.total })
			end
			-- calibration: the same accumulation over 1,000 calls of one helpers.encode_string of a chart
			local calib = { fn = function()
				return helpers.encode_string(synthetic.chart(1, 2, 3))
			end }
			local total = helpers.create_profiler(true)
			for _ = 1, 1000 do
				total.restart()
				calib.fn()
				total.stop()
			end
			local direct = helpers.create_profiler()
			for _ = 1, 1000 do
				calib.fn()
			end
			direct.stop()
			log({ "", "TSTEST prof calibration: 1000 encode_string, accumulated ", total, ", one profiler ", direct })
		end,
		--- Micro-benchmarks of the chart event path, logged.
		micro = function(n)
			local s = game.get_surface(spec and spec.surface or "nauvis")
			local f = game.forces.player
			local st = storage.timeshade
			local handler = capture.events[defines.events.on_chunk_charted]
			local function time(label, fn)
				local prof = helpers.create_profiler()
				fn()
				prof.stop()
				log({ "", "TSTEST micro ", label, " x", n, " ", prof })
			end
			time("empty loop", function()
				for _ = 1, n do
				end
			end)
			time("force.index", function()
				for _ = 1, n do
					local _ = f.index
				end
			end)
			time("core.key", function()
				for i = 1, n do
					local _ = capture.core.key(1, 1, i, -i)
				end
			end)
			local events = {}
			for i = 1, n do
				events[i] = { name = defines.events.on_chunk_charted, surface_index = s.index, force = f,
					position = { x = 5000 + i % 1000, y = 5000 + math.floor(i / 1000) } }
			end
			time("handler, new chunks", function()
				for i = 1, n do
					handler(events[i])
				end
			end)
			time("handler, queued chunks", function()
				for i = 1, n do
					handler(events[i])
				end
			end)
			time("terrain.on_chunk_charted direct, queued", function()
				for i = 1, n do
					capture.terrain.on_chunk_charted(st, events[i])
				end
			end)
			-- the train path, on the first moving train's mover
			local j, m
			for jj, mm in pairs(st.movers) do
				if mm.kind == "t" and mm.ref and mm.ref.valid and mm.ref.speed ~= 0 and mm.ef then
					j, m = jj, mm
					break
				end
			end
			if j then
				local r = m.ref
				time("train API reads (speed, position, orientation, surface_index)", function()
					for _ = 1, n do
						local _, _, _, _ = r.speed, m.ef.position, m.ef.orientation, m.ef.surface_index
					end
				end)
				time("r.carriages (4 carriages)", function()
					for _ = 1, n do
						local _ = r.carriages
					end
				end)
				local before = capture.outbox.pending()
				time("movers.poll (a read; emits per the rules)", function()
					for _ = 1, n do
						capture.movers.poll(st, j, game.tick)
					end
				end)
				log("TSTEST micro movers.poll queued " .. (capture.outbox.pending() - before) .. " messages")
				st.fast[j] = nil -- the polls above filled the wheel; the scan takes the train back
				time("outbox.flush of those messages", function()
					capture.outbox.flush(game.tick)
				end)
				local sample = { j = j, k = "t", f = "player", name = "locomotive", x = 123.45, y = -67.25, h = 0.25,
					n = 4, o = "LCCC", c = "eb1200" }
				time("helpers.table_to_json of a 3-sample move message", function()
					for _ = 1, n do
						local _ = helpers.table_to_json{ type = "move", s = "ts-lab", m = { sample, sample, sample } }
					end
				end)
				local msg = { type = "move", s = "ts-lab", m = { sample, sample, sample } }
				time("outbox.encode (move fast path) of a 3-sample move message", function()
					for _ = 1, n do
						local _ = capture.outbox.encode(msg)
					end
				end)
				time("string.format %.15g", function()
					for _ = 1, n do
						local _ = string.format("%.15g", 123.45)
					end
				end)
				time("tonumber of a short string", function()
					for _ = 1, n do
						local _ = tonumber("123.45")
					end
				end)
				time("pairs over a 10-field sample", function()
					for _ = 1, n do
						for _, _ in pairs(sample) do
						end
					end
				end)
				local pieces = {}
				for i = 1, 60 do
					pieces[i] = (i % 2 == 0) and "," or '"name":'
				end
				time("table.concat of 60 pieces", function()
					for _ = 1, n do
						local _ = table.concat(pieces)
					end
				end)
				time("a 60-element table filled", function()
					for _ = 1, n do
						local out = {}
						for i = 1, 60 do
							out[i] = ","
						end
					end
				end)
				local parts = {}
				for i = 1, 6 do
					parts[i] = capture.outbox.encode(msg)
				end
				time("core.cut of 6 parts", function()
					for _ = 1, n do
						local _ = capture.core.cut(123456, parts, 3500)
					end
				end)
				local chart = synthetic.chart(1, 2, 3)
				time("helpers.encode_string of a chart", function()
					for _ = 1, n / 10 do
						local _ = helpers.encode_string(chart)
					end
				end)
				local d = helpers.encode_string(chart)
				time("outbox.encode of a chunk message", function()
					for _ = 1, n / 10 do
						local _ = capture.outbox.encode{ type = "chunk", s = "ts-lab", f = "player", x = 1, y = 2, d = d }
					end
				end)
				time("chart_of through the seam", function()
					for i = 1, n / 10 do
						local _ = capture.terrain.seam(f, s, { x = i % 200, y = 3 })
					end
				end)
			end
			-- the chart path as the bench runs it: n / 10 ticks of one queued read, then the flush
			local st2 = storage.timeshade
			time("terrain.step (one queued read) + outbox.flush", function()
				for _ = 1, n / 10 do
					capture.terrain.step(st2, game.tick, nil)
					capture.outbox.flush(game.tick)
				end
			end)
			local side = 224
			local rng = 777
			local lists = {}
			for k = 1, n / 10 do
				local list = {}
				for i = 1, 20 do
					rng = rng * 16807 % 2147483647
					local c = rng % (side * side)
					list[i] = { c % side, math.floor(c / side) }
				end
				lists[k] = list
			end
			time("remote_charted.fn of 20 random events (event tables built here)", function()
				for k = 1, n / 10 do
					capture.remote_charted.fn(s.name, "player", lists[k])
				end
			end)
			log("TSTEST micro queue " .. capture.core.size(capture.terrain.state().debounce.queue))
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
