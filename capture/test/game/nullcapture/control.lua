-- The UPS benchmark's baseline (capture/test/run_game.mjs --mode bench). It stands in for timeshade_capture's test
-- interface with no-ops, so the run without the capture pays the same cost for the driver's remote calls (each tick's
-- list of chart events is copied between Lua states) and the difference between the runs is the capture's own cost.
local function none()
end

remote.add_interface("timeshade_capture_test", {
	synthetic_charts = none,
	charted = none,
	config = none,
	resync = none,
	stats = none,
	memory = none,
	constants = none,
	profile = none,
	profile_report = none,
	micro = none,
	read_cost = none,
})
