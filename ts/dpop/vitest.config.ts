import { defineConfig } from "vitest/config";

// The suite runs in Node (>= 20), whose globalThis.crypto is the same
// WebCrypto a browser has. IndexedDB comes from fake-indexeddb inside the one
// test that needs it, so no test sees a browser global it did not ask for.
export default defineConfig({
  test: {
    environment: "node",
    include: ["test/**/*.test.ts"],
  },
});
