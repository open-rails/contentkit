import path from "node:path";
import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";
import dts from "vite-plugin-dts";
import { ckuiCssPlugin } from "./tooling/css/vite-plugin.ts";

const root = path.dirname(fileURLToPath(import.meta.url));
const src = (p: string) => path.resolve(root, "src", p);

export const LOCALES = ["en", "de", "es", "ja", "ko", "zh"];

const EXTERNAL = [
  "react",
  "react-dom",
  "@base-ui/react",
  "@hugeicons/core-free-icons",
  "@hugeicons/react",
  "@noble/hashes",
  "class-variance-authority",
  "cn",
  "react-easy-crop",
];

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    dts({
      include: ["src"],
      entryRoot: "src",
      exclude: ["src/**/*.test.ts", "src/**/*.test.tsx", "src/test/**"],
      tsconfigPath: path.resolve(root, "tsconfig.json"),
    }),
    ckuiCssPlugin({ entries: ["ui"] }),
  ],
  resolve: { alias: { "#ckui": path.resolve(root, "src") } },
  build: {
    lib: {
      entry: {
        index: src("index.ts"),
        react: src("react.ts"),
        ui: src("ui.ts"),
        ...Object.fromEntries(LOCALES.map((l) => [`locales/${l}`, src(`locales/${l}.ts`)])),
      },
      formats: ["es"],
      cssFileName: "styles",
    },
    sourcemap: true,
    rollupOptions: {
      external: (id) => EXTERNAL.some((dep) => id === dep || id.startsWith(`${dep}/`)),
      output: { preserveModules: true, preserveModulesRoot: "src" },
    },
  },
});
