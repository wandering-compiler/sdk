import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// Unit tests run against src/ in Node (>= 24); the conformance suite in
// tests/conformance/ is driven by its Go harness instead (make conformance-ts).
// The conformance service imports "@w17/business" as an author's package
// does; here that name is this checkout's sources, so the tests and the
// service share one instance of the SDK.
export default defineConfig({
  resolve: {
    alias: { "@w17/business": fileURLToPath(new URL("./src/index.ts", import.meta.url)) },
  },
  test: {
    environment: "node",
    include: ["tests/*.test.ts"],
    testTimeout: 20000,
    hookTimeout: 20000,
    pool: "forks",
  },
});
