import { defineConfig, devices } from "@playwright/test";

const port = Number(process.env.DEMO_PORT ?? 4179);
const mediaPort = Number(process.env.MEDIA_PORT ?? 4180);

// Visual check of the demo (demo/) against the built package: `pnpm build && pnpm screenshots`.
export default defineConfig({
  testDir: "e2e",
  outputDir: process.env.SCREENSHOT_DIR ? `${process.env.SCREENSHOT_DIR}/results` : "test-results",
  workers: 1,
  forbidOnly: !!process.env.CI,
  use: { baseURL: `http://127.0.0.1:${port}`, trace: "retain-on-failure" },
  projects: [
    { name: "desktop", use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 900 } } },
    { name: "mobile", use: { ...devices["Pixel 7"] } },
  ],
  webServer: [
    {
      command: `vite --config demo/vite.config.ts --port ${port}`,
      url: `http://127.0.0.1:${port}`,
      reuseExistingServer: false,
      timeout: 60_000,
    },
    {
      command: `node e2e/media-server.ts`,
      url: `http://127.0.0.1:${mediaPort}/cors/img/1.jpg`,
      env: { MEDIA_PORT: String(mediaPort) },
      reuseExistingServer: false,
      timeout: 30_000,
    },
  ],
});
