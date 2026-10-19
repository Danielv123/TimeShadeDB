-- A stub of Clusterio's modules/clusterio/api.lua (clusterio/packages/host/modules/clusterio/api.lua): only what the
-- timeshade module reads. Instance id 4242.
local api = {}

api.events = {
	on_instance_updated = script.generate_event_name(),
	on_server_startup = script.generate_event_name(),
}

function api.get_instance_name()
	return "harness"
end

function api.get_instance_id()
	return 4242
end

return api
