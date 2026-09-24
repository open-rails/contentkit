import path from "node:path";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const root = path.resolve(import.meta.dirname, "..");

// Serves the built package (dist), so screenshots show the shipped, scoped CSS.
export default defineConfig({
  root: import.meta.dirname,
  plugins: [react()],
  resolve: {
    alias: [{ find: /^@openrails\/contentkit-upload(\/.*)?$/, replacement: path.join(root, "dist") + "$1" }],
  },
  server: { host: "127.0.0.1", strictPort: true },
});
