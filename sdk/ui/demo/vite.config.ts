import path from "node:path";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const root = path.resolve(import.meta.dirname, "..");

// Builds the demo pages against the built package (dist), so screenshots show
// the shipped, scoped CSS; e2e/support/demo.ts builds them for the harness.
export default defineConfig({
  root: import.meta.dirname,
  plugins: [react()],
  resolve: {
    alias: [
      { find: /^@openrails\/contentkit-ui$/, replacement: path.join(root, "dist/index.js") },
      { find: /^@openrails\/contentkit-ui\/(client|react|urls|locales\/\w+)$/, replacement: path.join(root, "dist") + "/$1.js" },
    ],
  },
});
