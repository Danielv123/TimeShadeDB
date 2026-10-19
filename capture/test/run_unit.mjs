// Runs the capture Lua unit tests (capture/test/unit/*_test.lua) in Lua 5.4 compiled to WebAssembly (wasmoon).
// The library is written for Factorio's Lua 5.2 and kept valid on 5.4; the game tests (run_game.mjs) run it on 5.2.
// Usage: node run_unit.mjs [filter]
import { LuaFactory } from "wasmoon";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const roots = [path.join(here, "..", "lua"), path.join(here, "unit")];
const filter = process.argv[2] ?? "";

const files = fs.readdirSync(path.join(here, "unit"))
	.filter(f => f.endsWith("_test.lua") && f.includes(filter))
	.sort();

let failed = 0;
let total = 0;
for (const file of files) {
	const factory = new LuaFactory();
	const lua = await factory.createEngine({ injectObjects: false, enableProxy: false, traceAllocations: false });
	try {
		lua.global.set("__readmodule", (name) => {
			const rel = name.replace(/[./]/g, "/") + ".lua";
			for (const root of roots) {
				const p = path.join(root, rel);
				if (fs.existsSync(p)) {
					return fs.readFileSync(p, "utf8");
				}
			}
			return null;
		});
		lua.global.set("__report", (ok, name, err) => {
			total++;
			if (ok) {
				console.log(`ok ${file} :: ${name}`);
			} else {
				failed++;
				console.log(`not ok ${file} :: ${name}\n       ${err}`);
			}
		});
		await lua.doString(`
			table.insert(package.searchers, 2, function(name)
				local src = __readmodule(name)
				if not src then
					return "no module '" .. name .. "' in capture/lua or capture/test/unit"
				end
				return assert(load(src, "@" .. name)), name
			end)
		`);
		lua.global.set("__src", fs.readFileSync(path.join(here, "unit", file), "utf8"));
		await lua.doString(`
			local testlib = require("testlib")
			testlib.cases = {}
			assert(load(__src, "@${file}"))()
			for _, case in ipairs(testlib.cases) do
				local ok, err = xpcall(case.fn, debug.traceback)
				__report(ok, case.name, tostring(err))
			end
		`);
	} catch (err) {
		failed++;
		total++;
		console.log(`not ok ${file} :: (load)\n       ${err?.stack ?? err}`);
	} finally {
		lua.global.close();
	}
}
console.log(`${total - failed}/${total} passed`);
process.exit(failed === 0 && total > 0 ? 0 : 1);
