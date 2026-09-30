// The two `node:fs` calls `src/vocab.test.ts` makes, declared here instead of
// pulling in `@types/node`.
//
// Not a preference. `@types/node` is GLOBAL once installed, and this package is
// typed for the DOM ("lib": ["ES2022", "DOM", "DOM.Iterable"]) — Node's own
// declarations redefine shared names (`setTimeout`'s return, `URL`, `fetch`),
// so adding them to type one filesystem read in one test file changes how every
// other file typechecks. Two signatures cost nothing and change nothing.
//
// Outside `src/` on purpose: package.json `files` publishes `src`, and an
// ambient `declare module "node:fs"` shipped to consumers would be ours leaking
// into their type space. tsconfig's `include` names this directory instead.
declare module "node:fs" {
  /** Recursive listing; entries are paths relative to `path`. */
  export function readdirSync(path: string, options: { recursive: true }): string[];
  export function readFileSync(path: string, encoding: "utf8"): string;
}
