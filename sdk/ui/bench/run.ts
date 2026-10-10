// Upload benchmark: Chromium → MinIO through media.UploadHandler (test/server.ts).
//   CONTENTKIT_TEST_S3_ENDPOINT=... [BENCH_SRC=<other src>] node bench/run.ts <file> [runs]
import http from "node:http";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import type { AddressInfo } from "node:net";
import { build } from "vite";
import { chromium } from "@playwright/test";
import { startServer, stopServer } from "../test/server.ts";

const here = import.meta.dirname;
const file = process.argv[2]!;
const runs = Number(process.argv[3] ?? 2);
const concurrency = process.env.BENCH_CONCURRENCY ? Number(process.env.BENCH_CONCURRENCY) : undefined;

const out = await build({
  configFile: false,
  logLevel: "warn",
  plugins: [
    {
      name: "hash-probe",
      enforce: "pre",
      resolveId(id, importer) {
        if (process.env.BENCH_SRC && id.startsWith("../src/") && importer?.startsWith(here))
          return resolve(process.env.BENCH_SRC, id.slice(7).replace(/\.js$/, ".ts"));
        if (process.env.BENCH_PROBE !== "0" && id === "./hash.js" && importer && !importer.endsWith("probe.ts") && !importer.startsWith(here))
          return resolve(here, "probe.ts");
      },
    },
  ],
  build: { write: false, minify: false, lib: { entry: resolve(here, "page.ts"), formats: ["es"], fileName: "bundle" } },
});
const outputs = (Array.isArray(out) ? out[0] : out) as any;
const assets = new Map<string, string | Uint8Array>();
for (const o of outputs.output) assets.set("/" + o.fileName, o.type === "chunk" ? o.code : o.source);
assets.set("/bundle.js", assets.get("/bundle.js") ?? [...assets.values()][0]!);

const { url: api, proc } = await startServer(process.env.CONTENTKIT_TEST_S3_ENDPOINT!);
const agent = new http.Agent({ keepAlive: true });
const web = http.createServer((req, res) => {
  if (req.url!.startsWith("/upload/")) {
    const u = new URL(api);
    const up = http.request({ host: u.hostname, port: u.port, path: req.url, method: req.method, headers: req.headers, agent }, (r) => {
      res.writeHead(r.statusCode!, r.headers);
      r.pipe(res);
    });
    req.pipe(up);
    return;
  }
  if (req.url === "/") return res.writeHead(200, { "content-type": "text/html" }).end(readFileSync(resolve(here, "index.html")));
  const a = assets.get(req.url!);
  if (!a) return res.writeHead(404).end();
  res.writeHead(200, { "content-type": "text/javascript" }).end(a);
});
await new Promise<void>((r) => web.listen(0, "127.0.0.1", r));
const origin = `http://localhost:${(web.address() as AddressInfo).port}`;

const browser = await chromium.launch({ args: ["--no-sandbox"] });
try {
  const page = await browser.newPage();
  page.on("console", (m) => console.error("[page]", m.text()));
  await page.goto(origin);
  await page.setInputFiles("#f", file);
  if (process.env.BENCH_HASH !== "0") {
    const h = await page.evaluate(() => (globalThis as any).bench.hashBench(16));
    console.log("hash bench (16 MiB parts):", h);
  }
  for (let i = 0; i < runs; i++) {
    const load = readFileSync("/proc/loadavg", "utf8").split(" ")[0];
    const r = await page.evaluate((o) => (globalThis as any).bench.upload(o), { concurrency });
    console.log(JSON.stringify({ run: i, load, ...summarize(r) }));
  }
} finally {
  await browser.close();
  web.close();
  agent.destroy();
  await stopServer(proc);
}

function summarize(r: { size: number; t0: number; uploadMs: number; commitMs: number; spans: any[] }) {
  const puts = r.spans.filter((s) => s.kind === "put").sort((a, b) => a.start - b.start);
  const hashes = r.spans.filter((s) => s.kind === "hash");
  const apis = r.spans.filter((s) => s.kind.startsWith("api"));
  const byKind: Record<string, { n: number; ms: number; max: number }> = {};
  for (const s of apis) {
    const k = (byKind[s.kind] ??= { n: 0, ms: 0, max: 0 });
    k.n++;
    k.ms += s.end - s.start;
    k.max = Math.max(k.max, s.end - s.start);
  }
  for (const k of Object.values(byKind)) k.ms = Math.round(k.ms / k.n);
  // Time with zero PUTs in flight between the first PUT start and the last PUT end.
  const edges = puts.flatMap((p) => [[p.start, 1], [p.end, -1]] as [number, number][]).sort((a, b) => a[0] - b[0] || a[1] - b[1]);
  let depth = 0, idle = 0, last = edges[0]?.[0] ?? 0, weighted = 0;
  for (const [t, d] of edges) {
    if (depth === 0) idle += t - last;
    weighted += depth * (t - last);
    depth += d;
    last = t;
  }
  const putSpan = puts.length ? puts.at(-1)!.end - puts[0]!.start : 0;
  const putMs = puts.reduce((n, p) => n + p.end - p.start, 0);
  const mb = r.size / 1e6;
  return {
    MB: Math.round(mb),
    totalS: +((r.uploadMs + r.commitMs) / 1000).toFixed(2),
    MBps: +(mb / ((r.uploadMs + r.commitMs) / 1000)).toFixed(1),
    uploadS: +(r.uploadMs / 1000).toFixed(2),
    commitMs: Math.round(r.commitMs),
    firstPutAtMs: Math.round((puts[0]?.start ?? r.t0) - r.t0),
    parts: puts.length,
    partMiB: [...new Set(puts.map((p) => Math.round(p.bytes / 1048576)))],
    perPutMBps: +((r.size / 1e6) / (putMs / 1000)).toFixed(1),
    avgInFlight: +(weighted / Math.max(putSpan, 1)).toFixed(2),
    idleMs: Math.round(idle),
    hash: { calls: hashes.length, totalMs: Math.round(hashes.reduce((n, h) => n + h.end - h.start, 0)), avgMs: Math.round(hashes.reduce((n, h) => n + h.end - h.start, 0) / Math.max(hashes.length, 1)) },
    api: byKind,
  };
}
