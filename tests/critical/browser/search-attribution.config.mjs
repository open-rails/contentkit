import { fileURLToPath } from "node:url";
export default {
  root: fileURLToPath(new URL(".", import.meta.url)),
  build: {
    outDir: process.env.CONTENTKIT_ATTRIBUTION_DIST ?? "../.search-attribution-dist",
    emptyOutDir: true,
    rollupOptions: { input: fileURLToPath(new URL("search-attribution.html", import.meta.url)) },
  },
};
