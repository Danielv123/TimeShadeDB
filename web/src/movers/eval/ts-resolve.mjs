// Lets Node (native type stripping) load the viewer's TypeScript modules, which import each
// other without extensions as Vite and tsc expect: a relative specifier without an
// extension resolves to the same path plus ".ts". Imported first by the movers tests and
// the train evaluator, before they import viewer modules dynamically.
import { registerHooks } from "node:module";

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (/^\.{1,2}\//.test(specifier) && !/\.[cm]?[jt]sx?$/.test(specifier)) {
      try {
        return nextResolve(specifier + ".ts", context);
      } catch {
        // fall through
      }
    }
    return nextResolve(specifier, context);
  },
});
