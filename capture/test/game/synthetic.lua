-- Synthetic charts for the headless game tests (headless Factorio never charts). Mirrored byte for byte by
-- syntheticChart in capture/test/run_game.mjs, which checks every chunk message the capture wrote.
local synthetic = {}

--- A 2,048-byte RGB565 LE chart for chunk (x, y) at phase p: a base colour per chunk with a stripe every 4th row whose
--- colour depends on p.
function synthetic.chart(x, y, p)
	local base = ((x * 31 + y * 17) % 2048) * 32 + 7
	local alt = (base + 0x0841 * (1 + p % 3)) % 65536
	local b1, b2 = string.char(base % 256, math.floor(base / 256)), string.char(alt % 256, math.floor(alt / 256))
	local row1 = string.rep(b1, 24) .. string.rep(b2, 8)
	local row2 = string.rep(b1, 32)
	local rows = {}
	for r = 1, 32 do
		rows[r] = (r % 4 == 0) and row1 or row2
	end
	return table.concat(rows)
end

--- spec = { surface, x1, y1, x2, y2, flicker = { ["x,y"] = ticks per phase } }: chunks [x1, x2) x [y1, y2) of the
--- surface are charted; a flickering chunk's phase is floor(tick / period), any other's 0.
function synthetic.phase(spec, x, y, tick)
	local period = spec.flicker and spec.flicker[x .. "," .. y]
	return period and math.floor(tick / period) or 0
end

--- A seam function(force, surface, position) -> chart or nil for capture.terrain.seam.
function synthetic.seam(spec)
	return function(_, surface, pos)
		if surface.name ~= spec.surface or pos.x < spec.x1 or pos.x >= spec.x2 or pos.y < spec.y1 or pos.y >= spec.y2 then
			return nil
		end
		return synthetic.chart(pos.x, pos.y, synthetic.phase(spec, pos.x, pos.y, game.tick))
	end
end

return synthetic
