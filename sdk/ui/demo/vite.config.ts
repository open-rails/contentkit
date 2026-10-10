import path from "node:path";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const root = path.resolve(import.meta.dirname, "..");

// Serves the built package (dist), so screenshots show the shipped, scoped CSS.
export default defineConfig({
  root: import.meta.dirname,
  plugins: [react()],
  resolve: {
    alias: [
      { find: /^@openrails\/contentkit-ui$/, replacement: path.join(root, "dist/index.js") },
      { find: /^@openrails\/contentkit-ui\/(client|react|urls|locales\/\w+)$/, replacement: path.join(root, "dist") + "/$1.js" },
    ],
  },
  // The social demo's ContentKit (e2e/content-server.ts).
  server: { host: "127.0.0.1", strictPort: true, proxy: { "/ck": `http://127.0.0.1:${process.env.CONTENT_PORT ?? 4181}` } },
});
