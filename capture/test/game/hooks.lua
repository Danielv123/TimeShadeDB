-- Test hooks, added to the mod only by `node capture/build.mjs --test` (never shipped): a remote interface the game-test
-- driver mod uses. Headless Factorio never charts, so charts come from a synthetic seam (synthetic.lua) whose bytes the
-- harness recomputes.
local synthetic = require("test.synthetic")

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
		profile = function()
			local totals = {}
			local function wrap(tbl, name, label)
				local fn = tbl[name]
				local total = helpers.create_profiler(true)
				local p = helpers.create_profiler(true)
				totals[#totals + 1] = { label = label, total = total, n = 0 }
				local entry = totals[#totals]
				tbl[name] = function(...)
					p.restart()
					local a, b = fn(...)
					p.stop()
					total.add(p)
					entry.n = entry.n + 1
					return a, b
				end
			end
			wrap(capture.terrain, "walk", "terrain.walk")
			wrap(capture.terrain, "step", "terrain.step")
			wrap(capture.overlays, "step", "overlays.step")
			wrap(capture.movers, "step", "movers.step")
			wrap(capture.outbox, "flush", "outbox.flush (JSON, cut, file buffer)")
			wrap(capture.remote_charted, "fn", "charted remote (20 events)")
			-- the standalone mod's file writes, through a stand-in for the global helpers (every other field passes)
			local real = helpers
			local writes = { write_file = function(...)
				return real.write_file(...)
			end }
			wrap(writes, "write_file", "helpers.write_file (the mod's file buffer)")
			helpers = setmetatable(writes, { __index = function(_, k)
				return real[k]
			end })
			capture.profile_totals = totals
		end,
		profile_report = function()
			for _, e in ipairs(capture.profile_totals or {}) do
				log({ "", "TSTEST prof ", e.label, " calls ", e.n, " ", e.total })
			end
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
			end
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
