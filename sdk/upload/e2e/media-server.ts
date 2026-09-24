// Serves e2e/fixtures/media like media-access, with failure modes by prefix:
//   /cors/…        CORS for the requesting origin (the healthy path)
//   /nocors/…      playlists with CORS, segments and sprites without (the missing MEDIA_ACCESS_CORS_ORIGINS incident)
//   /auth/{token}/ 403 unless token is "fresh"
//   /missing/…     404
//   /busy/…        429
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import path from "node:path";

const root = path.join(import.meta.dirname, "fixtures", "media");
const port = Number(process.env.MEDIA_PORT ?? 4180);
const types: Record<string, string> = {
  ".m3u8": "application/vnd.apple.mpegurl",
  ".seg": "video/mp2t",
  ".vtt": "text/vtt",
  ".jpg": "image/jpeg",
};

createServer(async (req, res) => {
  const url = new URL(req.url ?? "/", "http://x");
  const [, mode, ...rest] = url.pathname.split("/");
  let file = rest;
  const origin = req.headers.origin;
  const cors = () => {
    if (!origin) return;
    res.setHeader("Access-Control-Allow-Origin", origin);
    res.setHeader("Access-Control-Allow-Credentials", "true");
    res.setHeader("Vary", "Origin");
  };
  if (mode === "auth") {
    const [token, ...f] = rest;
    file = f;
    cors();
    if (token !== "fresh") return res.writeHead(403).end("forbidden");
  } else if (mode === "cors" || mode === "missing" || mode === "busy") cors();
  else if (mode === "nocors") {
    if (url.pathname.endsWith(".m3u8")) cors();
  } else return res.writeHead(404).end();
  if (mode === "missing") return res.writeHead(404).end("not found");
  if (mode === "busy") return res.writeHead(429, { "Retry-After": "5" }).end("rate limited");
  const p = path.join(root, ...file);
  if (!p.startsWith(root)) return res.writeHead(400).end();
  try {
    const body = await readFile(p);
    res.writeHead(200, { "Content-Type": types[path.extname(p)] ?? "application/octet-stream", "Cache-Control": "no-store" }).end(body);
  } catch {
    res.writeHead(404).end("not found");
  }
}).listen(port, "127.0.0.1");
