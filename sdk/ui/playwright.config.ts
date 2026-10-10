import { defineConfig, devices } from "@playwright/test";

// Browser specs against the real ContentKit (e2e/server): the demo pages
// (demo/) built from dist, so run `pnpm build` first. CKUI_E2E_ORIGIN is set
// by the global setup before workers load this config.
export default defineConfig({
  testDir: "e2e/browser",
  globalSetup: "./e2e/support/playwright-setup.ts",
  tsconfig: "./e2e/tsconfig.json",
  outputDir: process.env.SCREENSHOT_DIR ? `${process.env.SCREENSHOT_DIR}/results` : "test-results",
  workers: 2,
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  use: { baseURL: process.env.CKUI_E2E_ORIGIN, trace: "retain-on-failure" },
  projects: [
    { name: "desktop", use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 900 } } },
    { name: "mobile", use: { ...devices["Pixel 7"] } },
  ],
});
