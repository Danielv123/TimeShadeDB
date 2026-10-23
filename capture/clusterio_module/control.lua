-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap module/control.lua (Blue Oak Model License 1.0.0, see capture/README.md).
--
-- TimeShadeDB capture, Clusterio save-patch module (docs/PROTOCOL.md section 2, "Clusterio"). capture/build.mjs puts
-- the shared library (capture/lua) next to this file, so the save holds modules/timeshade/{control,globals,capture,...}.
--
-- Lines are printed in Clusterio's IPC frame, "\f$ipc:timeshade:out?j" .. envelope; the instance plugin (clusterio/instance.ts) parses
-- them, adds src, map, instance, recv_ms and seq, and forwards them. Nothing is captured until the instance plugin calls
-- ts_rt.config over RCON at each start (commands run on every peer), followed by ts_rt.resync.
local clusterio_api = require("modules/clusterio/api")
local capture = require("modules/timeshade/capture")

-- Clusterio's frame, as clusterio/packages/host/modules/clusterio/api.lua prints it ("\f$ipc:" .. channel .. "?j" ..
-- data). send_json is not used: it re-encodes, and sends 4,000 bytes or more through a file the host reads later, so
-- such a payload can arrive after lines printed after it. Printed lines of any length reach the host in order (its
-- LineSplitter has no line limit), so the outbox keeps lines under 64 KiB: a 1,000-carriage train fits a sample, and
-- the plugin's 1 MiB link events and the server's 256 KiB messages take every line.
local FRAME = "\f$ipc:timeshade:out?j"

capture.core.limits{ line = 65536, car = 1000, kinds = 1000, rp_start = 1000 }

capture.setup{
	sink = function(line)
		print(FRAME .. line)
	end,
	instance = function()
		return clusterio_api.get_instance_id() or 0
	end,
}

local events = capture.build_events()

-- Universal Edges, when its module is patched in: a crossing train or vehicle carries its journey.
local ok, ue = pcall(require, "modules/universal_edges")
capture.ue = ok and type(ue) == "table" and ue.api_version == 1 and ue or nil
if capture.ue then
	capture.ue.transfers.extend{
		name = "timeshade",
		types = { train = true, vehicle = true },
		capture = function(ep, h)
			return capture.movers.capture(storage.timeshade, ep, h)
		end,
		materialized = function(ep, h, data, meta)
			capture.movers.materialized(storage.timeshade, ep, h, data, meta)
		end,
	}
end

-- A start, crash or rollback: nothing is captured until the instance plugin configures this boot. The wheel's due ticks
-- would pass unread meanwhile, so the next scan finds the trains again.
events[clusterio_api.events.on_server_startup] = function()
	local st = capture.ensure()
	st.configured = false
	st.wheel, st.fast, st.carry = {}, {}, capture.core.queue()
end

local module = { events = events }

function module.on_init()
	capture.ensure()
end

function module.on_configuration_changed()
	capture.ensure()
end

function module.on_load()
	capture.on_load()
end

return module
