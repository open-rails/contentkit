import path from "node:path";
import { defineConfig } from "vitest/config";

const alias = { "#ckui": path.resolve(import.meta.dirname, "src") };

export default defineConfig({
  test: {
    projects: [
      {
        resolve: { alias },
        test: {
          name: "unit",
          include: ["src/**/*.test.{ts,tsx}", "tooling/**/*.test.ts"],
          environment: "node",
          maxWorkers: 2,
        },
      },
      {
        // Against the real ContentKit (e2e/server and its compose services).
        resolve: { alias },
        test: {
          name: "integration",
          include: ["e2e/integration/**/*.test.ts"],
          globalSetup: ["e2e/support/vitest-setup.ts"],
          environment: "node",
          testTimeout: 300_000,
          hookTimeout: 900_000,
          maxWorkers: 2,
        },
      },
      {
        // The jsdom files (hooks and components; each says so), as their own
        // run: with jsdom as the project's environment, or mixed with
        // node-environment files, vite externalized Node built-ins for them.
        resolve: { alias },
        test: {
          name: "integration-dom",
          include: ["e2e/integration/**/*.test.tsx"],
          globalSetup: ["e2e/support/vitest-setup.ts"],
          environment: "node",
          testTimeout: 300_000,
          hookTimeout: 900_000,
          maxWorkers: 2,
        },
      },
    ],
  },
});
