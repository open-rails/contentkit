import path from "node:path";
import { build } from "vite";

const ui = path.resolve(import.meta.dirname, "..", "..");

/** Builds the demo pages (demo/) against dist into e2e/.app, which the harness serves; run `pnpm build` first. */
export async function buildDemo(): Promise<string> {
  const outDir = path.join(ui, "e2e", ".app");
  await build({
    configFile: path.join(ui, "demo", "vite.config.ts"),
    logLevel: "warn",
    build: {
      outDir,
      emptyOutDir: true,
      target: "es2022",
      minify: false,
      rollupOptions: { input: Object.fromEntries(["index", "gallery", "upload", "folder", "social", "post"].map((n) => [n, path.join(ui, "demo", `${n}.html`)])) },
    },
  });
  return outDir;
}
