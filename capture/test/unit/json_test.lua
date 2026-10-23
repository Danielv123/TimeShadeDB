-- Unit tests of capture/lua/json.lua, the encoder the outbox uses. The game tests parse every line it wrote with a
-- real JSON parser (run_game.mjs).
local t = require("testlib")
local json = require("json")

t.test("json: scalars, escapes and arrays", function()
	t.expect_eq(json.encode('a"b\\c\n\1'), '"a\\"b\\\\c\\n\\u0001"')
	t.expect_eq(json.encode('a"b\\c\n\1'), '"a\\"b\\\\c\\n\\u0001"', "memoised: the same again")
	t.expect_eq(json.encode(true), "true")
	t.expect_eq(json.encode({ 1, 2, 3 }), "[1,2,3]")
	t.expect_eq(json.encode({ { 1.5, -2.25, 0.125 } }), "[[1.5,-2.25,0.125]]")
	t.expect_eq(json.encode({}), "{}")
	t.expect_eq(json.encode({ a = { b = "c" } }), '{"a":{"b":"c"}}')
end)

t.test("json: numbers: integers, 1/256-tile positions and rounded orientations are exact", function()
	t.expect_eq(json.number(5000000), "5000000")
	t.expect_eq(json.number(-12), "-12")
	t.expect_eq(json.number(40.47265625), "40.47265625")
	t.expect_eq(json.number(-999999.99609375), "-999999.99609375", "the map limit, on the 1/256 grid")
	t.expect_eq(json.number(0.123), "0.123")
	t.expect_eq(tonumber(json.number(0.1 + 0.2)), 0.3, "other numbers: 14 significant digits")
	t.expect_eq(json.number(0 / 0), "null")
	t.expect_eq(json.number(math.huge), "null")
end)

t.test("json: the move fast path gives what the generic encoder gives", function()
	local base = { j = "4242.600.7", k = "t", f = "player", name = "locomotive", x = -100.25, y = 12.00390625, h = 0.125,
		n = 3, o = "LCC", c = "eb1200" }
	local variants = {
		base,
		{ j = "1.2.3", k = "t", f = "player", name = "cargo-wagon", x = 1, y = 2, h = 0.75, n = 3, o = "ccl", c = "eb1200",
			tick = 5000000, stop = true, es = true },
		{ j = "1.2.5", k = "t", f = "player", name = "locomotive", x = 1, y = 2, h = 0.5, n = 3, o = "LCC", c = "eb1200",
			v = 0.4375, ru = true, rp = { { -1067, -764, 0, 1 }, { 12, 300, 15, 4 } } },
		{ j = "p:alice", k = "p", f = "player", name = "alice", x = 3.5, y = 4 },
		{ j = "1.2.3", gone = true },
		{ j = "1.2.4", k = "t", f = "player", name = "locomotive", x = 1, y = 2, h = 0.5, n = 1, o = "L", c = "ffffff",
			car = { { 1, 2, 0.5 } } },
	}
	local msg = { type = "move", s = 'surface "with" quotes', m = variants }
	local fast = json.encode_move(msg)
	local generic = json.encode(msg)
	t.expect_eq(#fast, #generic, "the same length")
	-- the same fields: compare each sample's sorted "key":value pieces
	local function pieces(text)
		local out = {}
		for piece in text:gmatch('"[%w_]+":[^,{}%[%]]+') do
			out[#out + 1] = piece
		end
		table.sort(out)
		return table.concat(out, " ")
	end
	t.expect_eq(pieces(fast), pieces(generic))
end)

t.test("json: a move message carries every field", function()
	local s = json.encode({ type = "move", s = "nauvis", m = { { j = "0.1.2", k = "t", x = 1.5, y = -2, stop = true,
		car = { { 1.5, -2, 0.25 } } } } })
	for _, part in ipairs({ '"type":"move"', '"s":"nauvis"', '"j":"0.1.2"', '"x":1.5', '"y":-2', '"stop":true',
		'"car":[[1.5,-2,0.25]]' }) do
		t.expect_true(s:find(part, 1, true), "has " .. part .. " in " .. s)
	end
end)

t.test("json: the chunk fast path gives what the generic encoder gives", function()
	local d = "eJzt0TENAAAIA7Dh/aMRsBQ+E2j1Vg+GAAAAAAAAAAAA+w5Z3gAB"
	for _, msg in ipairs({
		{ type = "chunk", s = "nauvis", f = "player", x = -3, y = 31250, d = d },
		{ type = "chunk", s = 'a "quoted" surface', f = "enemy", x = 0, y = -1 },
		{ type = "chunk", s = "nauvis", f = "player", x = 5, y = 6, d = d, tick = 123456 },
	}) do
		local fast, generic = json.encode_chunk(msg), json.encode(msg)
		t.expect_eq(#fast, #generic, "the same length: " .. fast)
		local function fields(text)
			local out = {}
			for k, v in text:gmatch('"([%w_]+)":([^,{}]+)') do
				out[#out + 1] = k .. "=" .. v
			end
			table.sort(out)
			return table.concat(out, " ")
		end
		t.expect_eq(fields(fast), fields(generic), "the same fields")
	end
end)
