-- A JSON encoder for capture messages. helpers.table_to_json costs about 25 microseconds for one move message of three
-- samples in Factorio 2.1 (measured by capture/test/run_game.mjs --micro); this does the same work in a fraction of that,
-- and it is exact for what the capture sends.
--
-- Rules: a table with t[1] ~= nil is an array (1..#t), any other table an object (string keys; the order is pairs'
-- order); an empty table is {}. Numbers print as Lua prints them (json.number).
-- NaN and infinities have no JSON form and print as null. Strings escape ", \ and control characters.
--
-- Most of the cost of encoding in Factorio's Lua is making new strings (each is interned), so short strings and keys
-- are memoised, and numbers are the only new strings per field.
local json = {}

local format, concat, find, gsub, byte = string.format, table.concat, string.find, string.gsub, string.byte
local tostring_, type_, pairs_ = tostring, type, pairs

local ESC = { ['"'] = '\\"', ["\\"] = "\\\\", ["\n"] = "\\n", ["\r"] = "\\r", ["\t"] = "\\t", ["\b"] = "\\b",
	["\f"] = "\\f" }
local function esc_char(c)
	return ESC[c] or format("\\u%04x", byte(c))
end

local function quote(s)
	if find(s, '[%c"\\]') then
		s = gsub(s, '[%c"\\]', esc_char)
	end
	return '"' .. s .. '"'
end

-- Quoted strings and object keys are memoised: names, ids, kinds and colours repeat in almost every message, and a
-- lookup by an existing string makes no new one. Long strings (charts) are quoted each time. At most MEMO entries each.
local MEMO = 4096
local quoted, keys, nq, nk = {}, {}, 0, 0

local function str(s)
	local q = quoted[s]
	if q then
		return q
	end
	q = quote(s)
	if #s <= 64 then
		if nq >= MEMO then
			quoted, nq = {}, 0
		end
		quoted[s], nq = q, nq + 1
	end
	return q
end

local function key(k)
	local q = keys[k]
	if q then
		return q
	end
	q = quote(tostring_(k)) .. ":"
	if nk >= MEMO then
		keys, nk = {}, 0
	end
	keys[k], nk = q, nk + 1
	return q
end

--- Lua's own number format ("%.14g"): exact for integers below 10^14 and for positions on the 1/256-tile grid within
--- the 1,000,000-tile map limit, and for the rounded orientations; other numbers get 14 significant digits. It is much
--- cheaper than a string.format call (about 0.8 microseconds each in Factorio 2.1).
local function num(v)
	if v ~= v or v == math.huge or v == -math.huge then
		return "null"
	end
	return tostring_(v)
end
json.number = num
json.string = str

local encode
local function value(v, out, n)
	local t = type_(v)
	if t == "string" then
		n = n + 1
		out[n] = str(v)
	elseif t == "number" then
		n = n + 1
		out[n] = num(v)
	elseif t == "boolean" then
		n = n + 1
		out[n] = v and "true" or "false"
	elseif t == "table" then
		n = encode(v, out, n)
	else
		n = n + 1
		out[n] = "null"
	end
	return n
end

encode = function(t, out, n)
	if t[1] ~= nil then
		n = n + 1
		out[n] = "["
		for i = 1, #t do
			if i > 1 then
				n = n + 1
				out[n] = ","
			end
			n = value(t[i], out, n)
		end
		n = n + 1
		out[n] = "]"
		return n
	end
	n = n + 1
	out[n] = "{"
	local first = true
	for k, v in pairs_(t) do
		if first then
			first = false
		else
			n = n + 1
			out[n] = ","
		end
		n = n + 1
		out[n] = key(k)
		n = value(v, out, n)
	end
	n = n + 1
	out[n] = "}"
	return n
end

--- The JSON text of v (a table, string, number or boolean).
function json.encode(v)
	local out = {}
	value(v, out, 0)
	return concat(out)
end

-- A train sample with every usual field, in one string.format call (the generic path makes one string per field), with
-- or without its own tick.
local SAMPLE = '{"j":%s,"k":"t","f":%s,"name":%s,"x":%.14g,"y":%.14g,"h":%.14g,"n":%d,"o":%s,"c":%s'
local SAMPLE_TICK = SAMPLE .. ',"tick":%d'

--- The JSON text of a move message: train samples without car through SAMPLE, anything else through the generic
--- encoder, all into one buffer joined once. The same text as json.encode would give, up to key order.
function json.encode_move(msg)
	local out, n = { '{"type":"move","s":', str(msg.s), ',"m":[' }, 3
	for i, e in ipairs(msg.m) do
		if i > 1 then
			n = n + 1
			out[n] = ","
		end
		if e.k == "t" and e.h and e.n and e.o and e.c and e.f and e.name and not e.car then
			n = n + 1
			if e.tick then
				out[n] = format(SAMPLE_TICK, str(e.j), str(e.f), str(e.name), e.x, e.y, e.h, e.n, str(e.o), str(e.c), e.tick)
			else
				out[n] = format(SAMPLE, str(e.j), str(e.f), str(e.name), e.x, e.y, e.h, e.n, str(e.o), str(e.c))
			end
			if e.stop then
				n = n + 1
				out[n] = ',"stop":true'
			end
			if e.es then
				n = n + 1
				out[n] = ',"es":true'
			end
			if e.v then
				n = n + 1
				out[n] = ',"v":' .. num(e.v)
			end
			if e.ru then
				n = n + 1
				out[n] = ',"ru":true'
			end
			local rp = e.rp
			if rp then
				-- rail pieces: [x, y, direction, kind], whole numbers
				n = n + 1
				out[n] = ',"rp":['
				for k = 1, #rp do
					local q = rp[k]
					n = n + 1
					out[n] = format(k > 1 and ',[%d,%d,%d,%d]' or '[%d,%d,%d,%d]', q[1], q[2], q[3], q[4])
				end
				n = n + 1
				out[n] = "]"
			end
			n = n + 1
			out[n] = "}"
		else
			n = value(e, out, n)
		end
	end
	n = n + 1
	out[n] = "]}"
	return concat(out)
end

local CHUNK = '{"type":"chunk","s":%s,"f":%s,"x":%d,"y":%d'

--- The JSON text of a chunk message. d (helpers.encode_string's base64) needs no escaping, so it is not scanned for
--- characters to escape as json.encode would; a chunk without d is a deletion.
function json.encode_chunk(msg)
	local s = format(CHUNK, str(msg.s), str(msg.f), msg.x, msg.y)
	if msg.tick then
		s = s .. ',"tick":' .. tostring_(msg.tick)
	end
	if msg.d then
		return s .. ',"d":"' .. msg.d .. '"}'
	end
	return s .. "}"
end

return json
