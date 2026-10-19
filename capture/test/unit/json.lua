-- A small JSON encoder standing in for helpers.table_to_json in the unit tests: a table whose keys are 1..n is an
-- array, any other table an object (keys sorted, so output is stable); an empty table is an object.
local json = {}

local ESC = { ['"'] = '\\"', ["\\"] = "\\\\", ["\n"] = "\\n", ["\r"] = "\\r", ["\t"] = "\\t" }

local function esc(s)
	return '"' .. s:gsub('[%c"\\]', function(c)
		return ESC[c] or string.format("\\u%04x", c:byte())
	end) .. '"'
end

local function num(v)
	if v == math.floor(v) and math.abs(v) < 1e15 then
		return string.format("%d", v)
	end
	return string.format("%.17g", v)
end

function json.encode(v)
	local t = type(v)
	if t == "nil" then
		return "null"
	elseif t == "boolean" then
		return tostring(v)
	elseif t == "number" then
		return num(v)
	elseif t == "string" then
		return esc(v)
	elseif t == "table" then
		local n = #v
		local count = 0
		for _ in pairs(v) do
			count = count + 1
		end
		if n > 0 and count == n then
			local parts = {}
			for i = 1, n do
				parts[i] = json.encode(v[i])
			end
			return "[" .. table.concat(parts, ",") .. "]"
		end
		local keys = {}
		for k in pairs(v) do
			keys[#keys + 1] = tostring(k)
		end
		table.sort(keys)
		local parts = {}
		for _, k in ipairs(keys) do
			local x = v[k]
			if x == nil then
				x = v[tonumber(k)]
			end
			parts[#parts + 1] = esc(k) .. ":" .. json.encode(x)
		end
		return "{" .. table.concat(parts, ",") .. "}"
	end
	error("json: cannot encode " .. t)
end

return json
