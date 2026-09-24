import { UploadClient, xhrTransport, type Transport } from "../src/index.js";
import { sha256 } from "@noble/hashes/sha2.js";

type Span = { kind: string; start: number; end: number; bytes?: number; part?: number };

async function upload(o: { concurrency?: number }) {
  const file = (document.getElementById("f") as HTMLInputElement).files![0]!;
  const spans: Span[] = [];
  ((globalThis as any).__hashLog ??= []).length = 0;
  const f: typeof fetch = async (input, init) => {
    const start = performance.now();
    try {
      return await fetch(input, init);
    } finally {
      spans.push({ kind: "api" + new URL(String(input), location.href).pathname.replace(/^\/upload/, ""), start, end: performance.now() });
    }
  };
  const transport: Transport = async (req, body, opts) => {
    const start = performance.now();
    await xhrTransport(req, body, opts);
    const part = Number(new URL(req.url).searchParams.get("partNumber")) || 0;
    spans.push({ kind: "put", start, end: performance.now(), bytes: body.size, part });
  };
  const c = new UploadClient({ endpoint: "/upload", headers: () => ({ "X-Test-Actor": "bench" }), fetch: f, transport, concurrency: o.concurrency });
  const ref = { kind: "video", id: "v" + Math.random().toString(36).slice(2), version: "" };
  const t0 = performance.now();
  const up = await c.upload(file, { ref });
  const t1 = performance.now();
  await c.commit(ref, [{ op: "insert", name: "v.mp4", original: up.name }]);
  const t2 = performance.now();
  for (const h of (globalThis as any).__hashLog) spans.push({ kind: "hash", ...h });
  return { size: file.size, t0, uploadMs: t1 - t0, commitMs: t2 - t1, spans, name: up.name };
}

// Standalone hashing costs on the same File.
async function hashBench(partMiB: number) {
  const file = (document.getElementById("f") as HTMLInputElement).files![0]!;
  const part = partMiB << 20;
  const out: Record<string, number> = {};
  let t = performance.now();
  for (let off = 0; off < file.size; off += part) await file.slice(off, off + part).arrayBuffer();
  out.readOnlyMs = performance.now() - t;
  t = performance.now();
  for (let off = 0; off < file.size; off += part) await crypto.subtle.digest("SHA-256", await file.slice(off, off + part).arrayBuffer());
  out.webcryptoMs = performance.now() - t;
  t = performance.now();
  for (let off = 0; off < Math.min(file.size, 256 << 20); off += 4 << 20) sha256(new Uint8Array(await file.slice(off, off + (4 << 20)).arrayBuffer()));
  out.noble256MiBMs = performance.now() - t;
  return out;
}

Object.assign(globalThis, { bench: { upload, hashBench } });
