import { createHash } from "node:crypto";
import { UploadError } from "../src/errors.js";
import type { Transport } from "../src/transport.js";
import type { SlotManifest, VideoImages } from "../src/wire.gen.js";
import type { ErrorReply, PartBody, PresignBody, RequestReply } from "../src/wire.gen.js";

const MiB = 1 << 20;

interface Upload {
  name: string;
  type: string;
  size: number;
  parts: Map<number, { size: number; sha256: string }>;
  signed: Map<number, PartBody>;
  complete: boolean;
}

/**
 * An in-process upload API and bucket with media.UploadHandler's semantics,
 * for unit tests of the client's scheduling. The integration suite runs the
 * real handler over MinIO.
 */
export class FakeServer {
  objects = new Map<string, number>();
  /** Originals the sweep may take: presign re-uploads them and commit refuses them. */
  stale = new Set<string>();
  /** Answer not_uploaded without the originals field. */
  omitOriginals = false;
  uploads = new Map<string, Upload>();
  calls: string[] = [];
  slots: string[] = [];
  puts: string[] = [];
  refuse?: ErrorReply & { status: number };
  /** Fail the next n storage PUTs with a dropped connection. */
  dropPuts = 0;
  /** Rendered slots by "kind/id#slot". */
  slotState = new Map<string, SlotManifest>();
  /** Bodies of commit-slot and recrop-slot calls. */
  slotCalls: any[] = [];
  /** Slot reads that answer pending after each commit or re-crop. */
  pendingReads = 0;
  private pendingLeft = 0;
  private seq = 0;
  /** A video item's images (every ref shares it). */
  video: VideoImages = {
    poster: { aspect: 16 / 9, outputs: [], pending: false, selection: { source: "auto", file: "clip.mp4", time: 3 } },
    hover_preview: { selection: { file: "clip.mp4", start: 3, duration: 3, auto: true }, mp4: [], webp: [], pending: false },
    video: { file: "clip.mp4", duration: 12, w: 1920, h: 1080, encoded: true },
  };
  /** Bodies of /video-poster and /video-preview. */
  videoCalls: any[] = [];
  /** Frame grabs as "t@w". */
  frames: string[] = [];

  fetch: typeof fetch = async (input, init) => {
    const path = new URL(String(input)).pathname.replace(/^\/api/, "");
    this.calls.push(path);
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    try {
      if (path === "/frame") {
        const q = new URL(String(input)).searchParams;
        this.frames.push(`${q.get("t")}@${q.get("w")}`);
        return new Response(new Blob([bytes(32, 3)], { type: "image/jpeg" }), { status: 200 });
      }
      if (path === "/slot-original") {
        if (!this.slotState.get(slotKey(body.ref, body.slot))?.dims) throw new UploadError("not_found", "no committed original", 404);
        return new Response(new Blob([bytes(64, 9)], { type: "image/jpeg" }), { status: 200 });
      }
      return json(200, this.route(path, body));
    } catch (e) {
      if (!(e instanceof UploadError)) throw e;
      const headers: Record<string, string> = e.retryAfter ? { "Retry-After": String(e.retryAfter) } : {};
      return json(e.status, { error: e.message, code: e.code, retry_after: e.retryAfter, originals: e.originals }, headers);
    }
  };

  transport: Transport = async (req, body, { signal, onProgress }) => {
    if (signal?.aborted) throw new UploadError("aborted", "aborted");
    this.puts.push(req.url);
    if (this.dropPuts > 0) {
      this.dropPuts--;
      onProgress?.(Math.floor(body.size / 2));
      throw new UploadError("network", "connection reset");
    }
    const bytes = new Uint8Array(await body.arrayBuffer());
    const sum = createHash("sha256").update(bytes).digest("base64");
    if (req.headers["X-Amz-Checksum-Sha256"] !== sum) throw new UploadError("storage", "BadDigest", 400);
    const [, kind, a, b] = new URL(req.url).pathname.split("/");
    if (kind === "put") {
      this.objects.set(a!, bytes.length);
      this.stale.delete(a!);
    }
    else {
      const u = this.uploads.get(a!)!;
      const s = u.signed.get(Number(b))!;
      if (s.size !== bytes.length) throw new UploadError("storage", "length", 403);
      u.parts.set(Number(b), { size: bytes.length, sha256: s.sha256 });
    }
    onProgress?.(bytes.length);
  };

  private route(path: string, b: any): unknown {
    switch (path) {
      case "/presign": {
        const p = b as PresignBody;
        if (this.refuse) {
          const r = this.refuse;
          throw new UploadError(r.code, r.error, r.status, r.retry_after);
        }
        if (p.size <= 64 * MiB || p.slot || p.inline) {
          if (!p.sha256) throw new UploadError("invalid_request", "sha256 required", 400);
          const name = p.inline ? `i-${++this.seq}` : (p.slot ?? "sha256-" + p.sha256);
          if (!p.slot && !p.inline && this.objects.get(name) === p.size && !this.stale.has(name)) return { name, exists: true };
          return { name, put: req(`fake://s3/put/${name}`, { "Content-Type": p.type, "X-Amz-Checksum-Sha256": b64(p.sha256) }) };
        }
        const ticket = `t${++this.seq}`;
        const name = `u-${this.seq}`;
        this.uploads.set(ticket, { name, type: p.type, size: p.size, parts: new Map(), signed: new Map(), complete: false });
        return { name, multipart: { ticket, min_part_size: 8 * MiB, max_part_size: 16 * MiB, max_parts: Math.ceil(p.size / (8 * MiB)) } };
      }
      case "/parts": {
        const u = this.ticket(b.ticket);
        return {
          parts: (b.parts as PartBody[]).map((p) => {
            if (p.size > 16 * MiB || !p.sha256) throw new UploadError("invalid_request", "bad part", 400);
            u.signed.set(p.number, p);
            return { number: p.number, request: req(`fake://s3/part/${b.ticket}/${p.number}`, { "X-Amz-Checksum-Sha256": b64(p.sha256) }) };
          }),
        };
      }
      case "/parts/list": {
        const u = this.ticket(b.ticket);
        return { parts: [...u.parts].sort((x, y) => x[0] - y[0]).map(([number, p]) => ({ number, ...p })) };
      }
      case "/complete": {
        const u = this.uploads.get(b.ticket);
        if (!u) throw new UploadError("not_found", "no upload", 404);
        const parts = [...u.parts].sort((x, y) => x[0] - y[0]);
        let total = 0;
        parts.forEach(([n, p], i) => {
          if (n !== i + 1) throw new UploadError("incomplete", `part ${i + 1} missing`, 409);
          if (i < parts.length - 1 && p.size < 8 * MiB) throw new UploadError("invalid_request", "small part", 400);
          total += p.size;
        });
        if (total !== u.size) throw new UploadError("incomplete", "short", 409);
        u.complete = true;
        this.objects.set(u.name, total);
        return { name: u.name, type: u.type, size: total };
      }
      case "/abort":
        this.uploads.delete(b.ticket);
        return undefined;
      case "/commit":
        {
          const missing = [...new Set<string>(b.ops.map((op: any) => op.original))].filter(
            (n) => n && (this.stale.has(n) || !this.objects.has(n)),
          );
          if (missing.length) {
            throw new UploadError("not_uploaded", "upload again", 409, undefined, { originals: this.omitOriginals ? undefined : missing });
          }
        }
        return { files: b.ops.map((op: any) => ({ name: op.name, original: op.original, size: this.objects.get(op.original) })) };
      case "/commit-slot":
        if (!this.objects.has(b.slot)) throw new UploadError("not_uploaded", "upload the original first", 409);
        this.slots.push(b.slot);
        this.slotCalls.push(b);
        return this.render(b.ref, b.slot, b.edit);
      case "/edit-slot":
        if (!this.slotState.has(slotKey(b.ref, b.slot))) throw new UploadError("not_found", "no original", 404);
        this.slotCalls.push(b);
        return this.render(b.ref, b.slot, b.edit);
      case "/video-images":
        return { ...this.video, poster: { ...this.video.poster, pending: this.video.poster.pending && this.pendingLeft-- > 0 } };
      case "/video-poster": {
        this.videoCalls.push(b);
        if (b.source === "upload" && !this.objects.has("poster")) throw new UploadError("not_uploaded", "upload the poster first", 409);
        const v = ++this.seq;
        const outputs = [480, 960, 1920].map((w) => ({ name: `poster_${w}`, w, h: Math.round((w * 9) / 16), url: `fake://cdn/public/poster_${w}.webp?v=${v}` }));
        const selection = { source: b.source, file: b.file ?? "clip.mp4", ...(b.time !== undefined ? { time: b.time } : {}) };
        this.pendingLeft = this.pendingReads;
        this.video = { ...this.video, poster: { aspect: 16 / 9, ...(b.edit ? { edit: b.edit } : {}), dims: { w: 1920, h: 1080 }, version: String(v), outputs, pending: this.pendingReads > 0, selection } };
        return this.video;
      }
      case "/video-preview": {
        this.videoCalls.push(b);
        const v = String(++this.seq);
        const out = (ext: string) => [320, 640].map((w) => ({ w, h: Math.round((w * 9) / 16), url: `fake://cdn/public/hover_preview_${w}.${ext}?v=${v}` }));
        const selection = b.start === undefined ? { file: "clip.mp4", start: 3, duration: 3, auto: true } : { file: b.file ?? "clip.mp4", start: b.start, duration: b.duration ?? 3 };
        this.video = { ...this.video, hover_preview: { selection, version: v, mp4: out("mp4"), webp: out("webp"), pending: false } };
        return this.video;
      }
      case "/slot": {
        const m = this.slotState.get(slotKey(b.ref, b.slot));
        if (!m) return { aspect: slotAspect(b.slot), outputs: [], pending: false };
        return { ...m, pending: this.pendingLeft-- > 0 };
      }
    }
    throw new UploadError("not_found", path, 404);
  }

  private render(ref: { kind: string; id: string }, slot: string, edit?: SlotManifest["edit"]): SlotManifest {
    const aspect = slotAspect(slot);
    const widths = slot === "avatar" ? [128, 256, 512] : [1500, 3000];
    const v = ++this.seq;
    const m: SlotManifest = {
      aspect,
      ...(edit ? { edit } : {}),
      dims: { w: 4000, h: 3000 },
      version: String(v),
      outputs: widths.map((w) => ({ name: `${slot}_${w}`, w, h: Math.round(w / aspect), url: `fake://cdn/public/${slot}_${w}.webp?v=${v}` })),
      pending: false,
    };
    this.slotState.set(slotKey(ref, slot), m);
    this.pendingLeft = this.pendingReads;
    return { ...m, pending: this.pendingReads > 0 };
  }

  private ticket(t: string): Upload {
    const u = this.uploads.get(t);
    if (!u || u.complete) throw new UploadError("not_found", "multipart upload not found", 404);
    return u;
  }
}

function slotKey(ref: { kind: string; id: string }, slot: string): string {
  return `${ref.kind}/${ref.id}#${slot}`;
}

function slotAspect(slot: string): number {
  return slot === "avatar" ? 1 : 3;
}

function req(url: string, headers: Record<string, string>): RequestReply {
  return { method: "PUT", url, headers, expires: new Date(Date.now() + 900_000).toISOString() };
}

function b64(hex: string): string {
  return Buffer.from(hex, "hex").toString("base64");
}

function json(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  if (body === undefined) return new Response(null, { status: 204 });
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
}

/** Deterministic pseudo-random bytes. */
export function bytes(n: number, seed = 1): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(n);
  let x = seed * 2654435761;
  for (let i = 0; i < n; i++) {
    x ^= x << 13;
    x ^= x >>> 17;
    x ^= x << 5;
    out[i] = x & 0xff;
  }
  return out;
}
