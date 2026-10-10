// Starts media/internal/uploadtestserver (contentkit.Runtime.Handler at /ck over
// PostgreSQL and MinIO, see test/server.ts) and serves it on CONTENT_PORT, so
// the social demo (demo/social.html, proxied by Vite at /ck) talks to the real
// handlers. Without CONTENTKIT_TEST_URL and CONTENTKIT_TEST_S3_ENDPOINT it
// answers 503 and e2e/social.spec.ts skips.
import { createServer, request } from "node:http";
import { startServer, stopServer } from "../test/server.ts";

const port = Number(process.env.CONTENT_PORT ?? 4181);
const endpoint = process.env.CONTENTKIT_TEST_S3_ENDPOINT;
const upstream = process.env.CONTENTKIT_TEST_URL && endpoint ? await startServer(endpoint) : null;
const target = upstream ? new URL(upstream.url) : null;

const server = createServer((req, res) => {
  if (req.url === "/health") return res.writeHead(200).end(target ? "ok" : "off");
  if (!target) return res.writeHead(503, { "Content-Type": "application/json" }).end('{"error":"no content server","code":"unavailable"}');
  const up = request({ host: target.hostname, port: target.port, method: req.method, path: req.url, headers: req.headers }, (ur) => {
    res.writeHead(ur.statusCode!, ur.headers);
    ur.pipe(res);
  });
  up.on("error", () => res.destroy());
  req.pipe(up);
});
server.listen(port, "127.0.0.1");

const stop = async () => {
  server.close();
  if (upstream) await stopServer(upstream.proc).catch(() => {});
  process.exit(0);
};
process.on("SIGTERM", stop);
process.on("SIGINT", stop);
