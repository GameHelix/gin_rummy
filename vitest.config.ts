import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// Resolve the same `@/*` → `src/*` alias the app uses, so tests import the game
// logic exactly as the components do. The core logic is framework-free, so a
// plain Node environment is all the suite needs.
export default defineConfig({
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  test: {
    environment: "node",
    include: ["tests/**/*.test.ts"],
  },
});
