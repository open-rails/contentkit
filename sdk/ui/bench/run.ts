// Upload benchmark: Chromium → MinIO through the real upload API (the e2e
// stack, e2e/support/stack.ts), each run a fresh item.
//   [BENCH_SRC=<other src>] node bench/run.ts <file> [runs]
import { copyFileSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { build } from "vite";
import { chromium } from "@playwright/test";
import { Harness } from "../e2e/support/harness.ts";
import { startStack } from "../e2e/support/stack.ts";

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
const site = mkdtempSync(join(tmpdir(), "ckui-bench-"));
for (const o of outputs.output) writeFileSync(join(site, o.fileName), o.type === "chunk" ? o.code : o.source);
const entry = outputs.output.find((o: any) => o.type === "chunk" && o.isEntry);
if (entry.fileName !== "bundle.js") copyFileSync(join(site, entry.fileName), join(site, "bundle.js"));
copyFileSync(resolve(here, "index.html"), join(site, "index.html"));

const stack = await startStack({ static: site });
const origin = stack.origin;
const h = new Harness(origin);
const user = await h.user();

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
    const it = await h.item({ kind: "file", owner: user.id });
    const r = await page.evaluate((o) => (globalThis as any).bench.upload(o), { concurrency, token: user.access_token, ref: { kind: it.kind, id: it.id } });
    console.log(JSON.stringify({ run: i, load, ...summarize(r) }));
  }
} finally {
  await browser.close();
  await stack.stop();
  rmSync(site, { recursive: true, force: true });
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
