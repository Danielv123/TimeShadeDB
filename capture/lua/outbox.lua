-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap module/tick.lua (Blue Oak Model License 1.0.0, see capture/README.md).
--
-- The outbox: handlers append messages during a tick, and the on_tick step flushes them as envelope lines
-- ({"v":1,"tick":T,"msgs":[...]}, docs/v3/PROTOCOL.md section 1) under core.LIMIT bytes each. Where a line goes is the
-- host's choice (outbox.sink): the Clusterio module prints it in Clusterio's IPC frame, the standalone mod appends it to
-- a script-output file.
--
-- Everything here is file-local and only feeds printed lines, never game state: a joining client's outbox differs only
-- in what that client would print, which is nothing that touches the simulation.
local name = ...
local P = type(name) == "string" and name:match("^(.*[./])[^./]*$") or ""
local core = require(P .. "core")
local json = require(P .. "json")

local outbox = {}

local queue, moves, oversize, lines_out, bytes_out = {}, {}, 0, 0, 0

--- sink(line, tick): where each line goes. The host sets it before the first flush.
outbox.sink = function(_) end

--- encode(table) -> JSON text: json.lua, with its fast path for move messages (helpers.table_to_json does the same
--- several times slower).
outbox.encode = function(msg)
	if msg.type == "move" then
		return json.encode_move(msg)
	end
	return json.encode(msg)
end

--- Queue one message for this tick's flush. Samples queued after it start a new move message, so every message keeps
--- its place in the order.
function outbox.send(msg)
	queue[#queue + 1] = msg
	moves = {}
end

--- Add a sample, or { j, gone = true }, to this tick's move message for the surface named s: at most core.MOVE_MAX
--- samples a message, and a sample with `car` always in a message of its own, so every message fits a line.
function outbox.move(s, sample)
	if sample.car then
		queue[#queue + 1] = { type = "move", s = s, m = { sample } }
		return
	end
	local m = moves[s]
	if not m or #m.m >= core.MOVE_MAX then
		m = { type = "move", s = s, m = {} }
		moves[s] = m
		queue[#queue + 1] = m
	end
	m.m[#m.m + 1] = sample
end

--- Messages dropped since load for being too big for a line.
function outbox.oversize()
	return oversize
end

--- Lines and bytes handed to the sink since load.
function outbox.stats()
	return lines_out, bytes_out
end

--- Messages waiting for the flush.
function outbox.pending()
	return #queue
end

--- Encode each message once, cut the batch byte-exactly, hand the lines to the sink.
function outbox.flush(tick, speed)
	if #queue == 0 then
		return
	end
	local parts = {}
	for i, msg in ipairs(queue) do
		parts[i] = outbox.encode(msg)
	end
	queue, moves = {}, {}
	local lines, dropped = core.cut(tick, parts, core.LIMIT, speed)
	oversize = oversize + dropped
	for _, line in ipairs(lines) do
		lines_out, bytes_out = lines_out + 1, bytes_out + #line
		outbox.sink(line, tick)
	end
end

--- Drop the queued messages, keeping the counters: the capture is not configured, so nothing may be sent.
function outbox.clear()
	if #queue > 0 then
		queue, moves = {}, {}
	end
end

--- Drop everything queued (tests).
function outbox.reset()
	queue, moves, oversize, lines_out, bytes_out = {}, {}, 0, 0, 0
end

return outbox
