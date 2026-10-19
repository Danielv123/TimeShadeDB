-- SPDX-License-Identifier: BlueOak-1.0.0
-- Ported from illisis/minimap test/unit/testlib.lua (Blue Oak Model License 1.0.0, see capture/README.md).
-- Minimal test library for plain Lua (unit tests of capture/lua).
local testlib = { cases = {} }

function testlib.test(name, fn)
	testlib.cases[#testlib.cases + 1] = { name = name, fn = fn }
end

local function describe(value)
	if type(value) == "string" then
		return string.format("%q", value)
	end
	return tostring(value)
end

local function same(a, b)
	if type(a) ~= type(b) then
		return false
	end
	if type(a) ~= "table" then
		return a == b
	end
	for k, v in pairs(a) do
		if not same(v, b[k]) then
			return false
		end
	end
	for k in pairs(b) do
		if a[k] == nil then
			return false
		end
	end
	return true
end
testlib.same = same

function testlib.expect_eq(actual, expected, message)
	if actual ~= expected then
		error((message or "values differ") .. ": expected " .. describe(expected) .. ", got " .. describe(actual), 2)
	end
end

function testlib.expect_true(value, message)
	if not value then
		error(message or "expected a truthy value", 2)
	end
end

function testlib.expect_near(actual, expected, eps, message)
	if type(actual) ~= "number" or math.abs(actual - expected) > eps then
		error((message or "values differ") .. ": expected " .. describe(expected) .. " +- " .. eps .. ", got "
			.. describe(actual), 2)
	end
end

function testlib.expect_same(actual, expected, message)
	if not same(actual, expected) then
		error(message or "tables differ", 2)
	end
end

function testlib.expect_error(fn, pattern)
	local ok, err = pcall(fn)
	if ok then
		error("expected an error", 2)
	end
	if pattern and not string.find(tostring(err), pattern) then
		error("error " .. describe(tostring(err)) .. " does not match " .. describe(pattern), 2)
	end
end

--- A deep copy of a plain table (no metatables, no cycles beyond shared references kept shared).
function testlib.copy(v, seen)
	if type(v) ~= "table" then
		return v
	end
	seen = seen or {}
	if seen[v] then
		return seen[v]
	end
	local out = {}
	seen[v] = out
	for k, x in pairs(v) do
		out[testlib.copy(k, seen)] = testlib.copy(x, seen)
	end
	return out
end

return testlib
