-- Synthetic charts for the headless game tests (headless Factorio never charts). Mirrored byte for byte by
-- syntheticChart in capture/test/run_game.mjs, which checks every chunk message the capture wrote.
local synthetic = {}

local M = 2147483647 -- the Park-Miller generator: products stay below 2^53, exact in doubles (Lua 5.2) and in JS

local VARIANTS = 509 -- distinct charts; a chunk's chart is variant (x * 31 + y * 17 + p * 7) % VARIANTS
local variants = {}

--- Variant v: runs of 1 to 12 pixels in one of 6 colours of a palette, from a generator seeded by v. It deflates to
--- several hundred bytes, like a real chart.
local function build(v)
	local seed = v * 7919 % (M - 1) + 1
	local base = (v * 97 % 2048) * 32 + 7
	local palette = {}
	for i = 1, 6 do
		local c = (base + (i - 1) * 0x0841) % 65536
		palette[i] = string.char(c % 256, math.floor(c / 256))
	end
	local px, n = {}, 0
	while n < 1024 do
		seed = seed * 16807 % M
		local run = 1 + seed % 12
		seed = seed * 16807 % M
		local col = palette[1 + seed % 6]
		for _ = 1, run do
			if n < 1024 then
				n = n + 1
				px[n] = col
			end
		end
	end
	return table.concat(px)
end

--- Use these 2,048-byte charts as the variants instead of generated ones (the UPS bench's --real-charts: charts cut
--- from a real map, which deflate differently from the generated runs). Bench only: the game tests check the generated
--- bytes.
function synthetic.use(list)
	for v = 0, VARIANTS - 1 do
		variants[v] = list[v % #list + 1]
	end
end

--- The 2,048-byte RGB565 LE chart of chunk (x, y) at phase p. The variants are built once, so the seam costs about
--- what the native get_chunk_chart does (a table lookup instead of a copy).
function synthetic.chart(x, y, p)
	local v = (x * 31 + y * 17 + p * 7) % VARIANTS
	local c = variants[v]
	if not c then
		c = build(v)
		variants[v] = c
	end
	return c
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
