import { fileURLToPath } from "node:url";

export default {
  root: fileURLToPath(new URL(".", import.meta.url)),
  build: { outDir: "../.browser-dist", emptyOutDir: true },
};
