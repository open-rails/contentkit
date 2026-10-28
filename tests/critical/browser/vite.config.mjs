import { fileURLToPath } from "node:url";

export default {
  root: fileURLToPath(new URL(".", import.meta.url)),
  build: { outDir: "../.browser-dist", emptyOutDir: true,
    rollupOptions: { input: {
      index: fileURLToPath(new URL("index.html", import.meta.url)),
      popular: fileURLToPath(new URL("popular.html", import.meta.url)),
      "video-poster-picker": fileURLToPath(new URL("video-poster-picker.html", import.meta.url)),
    } },
  },
};
