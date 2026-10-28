import { fileURLToPath } from "node:url";
export default {
  root: fileURLToPath(new URL(".", import.meta.url)),
  build: {
    outDir: process.env.CONTENTKIT_MEDIA_SSE_DIST ?? "../.media-sse-upload-dist",
    emptyOutDir: true,
    rollupOptions: { input: fileURLToPath(new URL("media-sse-upload.html", import.meta.url)) },
  },
};
