import { defineConfig } from "vitest/config";

// `npm run fixture` only: writes the cross-language fixture the Go verifier
// checks (go/lib/dpop/testdata/ts_proofs.json). `npm test` never runs it.
export default defineConfig({
  test: {
    environment: "node",
    include: ["scripts/gen-fixture.ts"],
  },
});
