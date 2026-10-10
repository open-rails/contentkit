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
        resolve: { alias },
        test: {
          name: "integration",
          include: ["test/**/*.test.ts"],
          environment: "node",
          testTimeout: 300_000,
          hookTimeout: 300_000,
          fileParallelism: false,
        },
      },
    ],
  },
});
