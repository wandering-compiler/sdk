import { defineConfig } from "vitest/config";

// Test config for the hand-written admin-runtime logic tier.
//
// `jsdom` gives auth.ts a real `window.localStorage`. Coverage is the
// JS mirror of srcgo/coverage-gate.sh: v8 provider, thresholds gate the
// suite (per docs/test-coverage.md — FLOOR 70 / TARGET 90).
//
// SCOPE: coverage.include is the pure-logic surface plus the components
// that now HAVE render tests (@testing-library/react, `.test.tsx`).
// Components are a render seam, not a unit-depth target — the tests
// assert the decisions the component makes (does the Add button appear?
// does the create form POST the declared fields?), not its markup. Add
// a component's path here once its BEHAVIOUR is covered — not merely
// once it has a render test. ListPage/App have targeted tests (the Add
// button gate, the create route guard) but are not covered end to end,
// so gating them at the threshold would be a false signal.
export default defineConfig({
  test: {
    environment: "jsdom",
    setupFiles: ["./vitest.setup.ts"],
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    coverage: {
      provider: "v8",
      include: ["src/api.ts", "src/auth.ts", "src/types.ts", "src/CreatePage.tsx"],
      reporter: ["text", "text-summary"],
      thresholds: { lines: 90, functions: 90, statements: 90, branches: 85 },
    },
  },
});
