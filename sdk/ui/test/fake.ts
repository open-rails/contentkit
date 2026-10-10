import { createHash } from "node:crypto";
import { createContentKitClient, type ContentKitClient } from "../src/client/client.js";
import { ContentKitError } from "../src/client/errors.js";
import type { CommitBody, ErrorReply, Failure, FileInfo, Op, PartBody, PresignBody, PublicImage, ReadResult, RequestReply } from "../src/client/generated/wire.js";
import type { MediaOptions } from "../src/client/media/client.js";
import type { PresetRule, UploadRule } from "../src/client/media/rules.js";
import { stem } from "../src/client/media/client.js";
import type { Transport } from "../src/client/media/transport.js";

const MiB = 1 << 20;
const EXT: Record<string, string> = { "image/png": "png", "image/jpeg": "jpg", "video/mp4": "mp4", "application/x-subrip": "srt" };

interface Upload {
  path: string;
  /** The staged name the parts write. */
  blob: string;
  type: string;
  size: number;
  parts: Map<number, { size: number; sha256: string }>;
  signed: Map<number, PartBody>;
  complete: boolean;
}

/**
 * An in-process upload and read API and bucket with media.UploadHandler's
 * and Reader.Handler's semantics, for unit tests of the client's scheduling
 * and the components. The integration suite runs the real handlers over MinIO.
 * Mounts: upload "http://x/api", media "http://x/read".
 *
 * Fresh uploads are staged (u-{uuid}); a commit places them at once at
 * sha256-{hex} of the declared hash, so an identical upload then exists.
 */
export class FakeServer {
  /** Objects in the bucket by name (staged u-… or placed sha256-…), with their size. */
  objects = new Map<string, number>();
  /** Names the sweep may take: presign stages them again and commit refuses them. */
  stale = new Set<string>();
  /** Every presign's body. */
  presigns: PresignBody[] = [];
  /** Answer not_uploaded without the blobs field. */
  omitBlobs = false;
  uploads = new Map<string, Upload>();
  calls: string[] = [];
  puts: string[] = [];
  /** Every commit's ops. */
  commits: Op[][] = [];
  commitRequests: CommitBody[] = [];
  private receipts = new Map<string, string>();
  refuse?: ErrorReply & { status: number };
  /** Presigns answer process_on_upload. */
  processOnUpload = false;
  /** Fail the next n storage PUTs with a dropped connection. */
  dropPuts = 0;
  /** Editor reads that answer pending after each commit. */
  pendingReads = 0;
  /** Editor reads that answer the uploads staged (not placed yet) after each commit. */
  stagedReads = 0;
  /** Editor reads report the item full. */
  full = false;
  /** Each item's uploads, by "kind/id". */
  items = new Map<string, FileInfo[]>();
  /** Published renditions supplied by a component fixture, not inferred from paths. */
  publicImages = new Map<string, PublicImage[]>();
  /** Frame grabs as "t@w". */
  frames: string[] = [];
  /** Each kind's upload rules, carried by editor reads. */
  rules = new Map<string, UploadRule[]>();
  /** GET /presets. */
  presets: PresetRule[] = [];
  /** Reads answer rate_limited (retry_after 1) this many times. */
  limitReads = 0;
  /** Each read's query. */
  reads: URLSearchParams[] = [];
  /** Reads' expiry, unix seconds (0: none). */
  expires = 0;
  /** Uploads the worker fails, by path: their failure appears on the next commit. */
  failures = new Map<string, Failure>();
  private pendingLeft = 0;
  private stagedLeft = 0;
  private seq = 0;
  /** Each staged name's declared hash. */
  private hashes = new Map<string, string>();

  /** Seeds an item's uploads (editor read view). */
  seed(ref: { kind: string; id: string }, files: FileInfo[]) {
    this.items.set(key(ref), files.map((f) => ({ upload: true, ...f })));
  }

  fetch: typeof fetch = async (input, init) => {
    const url = new URL(String(input));
    const path = url.pathname.replace(/^\/api/, "");
    this.calls.push(path.startsWith("/read/") ? "/read" : path);
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    try {
      if (path === "/frame") {
        this.frames.push(`${url.searchParams.get("t")}@${url.searchParams.get("w")}`);
        return new Response(new Blob([bytes(32, 3)], { type: "image/jpeg" }), { status: 200 });
      }
      if (path === "/read/presets") return json(200, this.presets);
      if (path.startsWith("/read/")) {
        this.reads.push(url.searchParams);
        if (this.limitReads > 0) {
          this.limitReads--;
          throw new ContentKitError("rate_limited", "slow down", { status: 429, retryAfter: 1 });
        }
        const [, , kind, id] = path.split("/");
        return json(200, this.read({ kind: kind!, id: id! }, url.searchParams));
      }
      return json(200, this.route(path, body));
    } catch (e) {
      if (!(e instanceof ContentKitError)) throw e;
      const headers: Record<string, string> = e.retryAfter ? { "Retry-After": String(e.retryAfter) } : {};
      return json(e.status, { error: e.message, code: e.code, retry_after: e.retryAfter, blobs: e.blobs }, headers);
    }
  };

  transport: Transport = async (req, body, { signal, onProgress }) => {
    if (signal?.aborted) throw new ContentKitError("aborted", "aborted");
    this.puts.push(req.url);
    if (this.dropPuts > 0) {
      this.dropPuts--;
      onProgress?.(Math.floor(body.size / 2));
      throw new ContentKitError("network", "connection reset");
    }
    const bytes = new Uint8Array(await body.arrayBuffer());
    const sum = createHash("sha256").update(bytes).digest("base64");
    if (req.headers["X-Amz-Checksum-Sha256"] !== sum) throw new ContentKitError("storage", "BadDigest", { status: 400 });
    const [, kind, a, b] = new URL(req.url).pathname.split("/");
    if (kind === "put") {
      this.objects.set(a!, bytes.length);
      this.stale.delete(a!);
    } else {
      const u = this.uploads.get(a!)!;
      const s = u.signed.get(Number(b))!;
      if (s.size !== bytes.length) throw new ContentKitError("storage", "length", { status: 403 });
      u.parts.set(Number(b), { size: bytes.length, sha256: s.sha256 });
    }
    onProgress?.(bytes.length);
  };

  private read(ref: { kind: string; id: string }, q: URLSearchParams): ReadResult {
    const prefix = q.get("prefix") ?? "";
    const editor = q.has("editor");
    const offset = Number(q.get("offset") ?? 0);
    const limit = Number(q.get("limit") ?? 0) || 50;
    const pending = this.pendingLeft > 0 && (this.pendingLeft--, true);
    const staged = this.stagedLeft > 0 && (this.stagedLeft--, true);
    const files = (this.items.get(key(ref)) ?? [])
      .filter((f) => f.path.startsWith(prefix))
      .map((f, i) => ({
        ...f,
        ...(pending ? { pending: ["render"] } : {}),
        ...(staged ? { staged: true } : {}),
        ...(editor && f.type.startsWith("image/") && f.size ? { editor_url: `fake://cdn/private/e-${f.path}` } : {}),
        ...(i >= offset && i < offset + limit ? { url: `fake://cdn/private/${f.path}?at=${this.reads.length}` } : {}),
      }));
    const published = pending || staged ? [] : (this.publicImages.get(key(ref)) ?? []).filter((p) => files.some((f) => f.path === p.from && !f.unattached));
    const rules = editor ? this.rules.get(ref.kind) : undefined;
    return { access: "full", expires: this.expires, total: files.length, offset, limit, files, public: published, ...(this.full ? { full: true } : {}), ...(rules ? { uploads: rules } : {}) } as ReadResult;
  }

  private route(path: string, b: any): unknown {
    switch (path) {
      case "/presign": {
        const p = b as PresignBody;
        if (this.refuse) {
          const r = this.refuse;
          throw new ContentKitError(r.code, r.error, { status: r.status, retryAfter: r.retry_after });
        }
        if (!/^[0-9a-f]{64}$/.test(p.sha256)) throw new ContentKitError("invalid_request", "sha256 required", { status: 400 });
        this.presigns.push(p);
        const ext = EXT[p.type] ?? "bin";
        // Named uploads are named by the server; a path without an extension gets one.
        const at = p.path.startsWith("inline/") ? `inline/i-${++this.seq}.${ext}` : /\.\w+$/.test(p.path) ? p.path : `${p.path}.${ext}`;
        const placed = "sha256-" + p.sha256;
        if (this.objects.get(placed) === p.size && !this.stale.has(placed)) return { path: at, blob: placed, exists: true, process_on_upload: this.processOnUpload };
        // Anything else is staged under a fresh name.
        const blob = `u-00000000-0000-4000-8000-${String(++this.seq).padStart(12, "0")}`;
        this.hashes.set(blob, p.sha256);
        const out = { path: at, blob, process_on_upload: this.processOnUpload };
        if (p.size <= 64 * MiB) return { ...out, put: req(`fake://s3/put/${blob}`, { "Content-Type": p.type, "X-Amz-Checksum-Sha256": b64(p.sha256) }) };
        const ticket = `t${++this.seq}`;
        this.uploads.set(ticket, { path: at, blob, type: p.type, size: p.size, parts: new Map(), signed: new Map(), complete: false });
        return { ...out, multipart: { ticket, min_part_size: 8 * MiB, max_part_size: 16 * MiB, max_parts: Math.ceil(p.size / (8 * MiB)) } };
      }
      case "/parts": {
        const u = this.ticket(b.ticket);
        return {
          parts: (b.parts as PartBody[]).map((p) => {
            if (p.size > 16 * MiB || !p.sha256) throw new ContentKitError("invalid_request", "bad part", { status: 400 });
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
        if (!u) throw new ContentKitError("not_found", "no upload", { status: 404 });
        const parts = [...u.parts].sort((x, y) => x[0] - y[0]);
        let total = 0;
        parts.forEach(([n, p], i) => {
          if (n !== i + 1) throw new ContentKitError("incomplete", `part ${i + 1} missing`, { status: 409 });
          if (i < parts.length - 1 && p.size < 8 * MiB) throw new ContentKitError("invalid_request", "small part", { status: 400 });
          total += p.size;
        });
        if (total !== u.size) throw new ContentKitError("incomplete", "short", { status: 409 });
        u.complete = true;
        this.objects.set(u.blob, total);
        return { blob: u.blob, type: u.type, size: total };
      }
      case "/abort":
        this.uploads.delete(b.ticket);
        return undefined;
      case "/commit": {
        this.commitRequests.push(b);
        if (!b.operation_id) throw new ContentKitError("invalid_request", "operation_id required", { status: 400 });
        const input = JSON.stringify({ ref: b.ref, ops: b.ops });
        const prior = this.receipts.get(b.operation_id);
        if (prior !== undefined) {
          if (prior !== input) throw new ContentKitError("conflict", "commit inputs changed", { status: 409 });
          return { files: this.items.get(key(b.ref)) ?? [] };
        }
        const files = this.commit(b.ref, b.ops);
        this.receipts.set(b.operation_id, input);
        return { files };
      }
    }
    throw new ContentKitError("not_found", path, { status: 404 });
  }

  private commit(ref: { kind: string; id: string }, ops: Op[]): FileInfo[] {
    const missing = [...new Set(ops.filter((op) => op.op === "put").map((op) => op.blob!))].filter((n) => this.stale.has(n) || !this.objects.has(n));
    if (missing.length) throw new ContentKitError("not_uploaded", "upload again", { status: 409, blobs: this.omitBlobs ? undefined : missing });
    this.commits.push(ops);
    let files = [...(this.items.get(key(ref)) ?? [])];
    const at = (p: string) => files.findIndex((f) => f.path === p || stem(f.path) === stem(p));
    const place = (f: FileInfo, index?: number) => {
      const i = at(f.path);
      if (i >= 0) files[i] = f;
      else files.splice(index ?? files.length, 0, f);
    };
    for (const op of ops) {
      const i = op.path ? at(op.path) : -1;
      const cur = files[i];
      if (op.op !== "put" && op.op !== "frame" && !cur) throw new ContentKitError("not_found", `no upload ${op.path}`, { status: 404 });
      switch (op.op) {
        case "put": {
          const type = op.path!.endsWith(".mp4") ? "video/mp4" : op.path!.endsWith(".srt") ? "application/x-subrip" : "image/png";
          const size = this.objects.get(op.blob!)!;
          const dims = type.startsWith("image/") ? { w: 4000, h: 3000 } : type.startsWith("video/") ? { w: 1920, h: 1080, dur: 12 } : {};
          place({ path: op.path!, type, size, ...dims, upload: true, ...(op.edit ? { edit: op.edit } : {}), ...(op.meta ? { meta: op.meta } : {}), ...(op.unattached ? { unattached: true } : {}) }, op.index);
          // The worker places a staged upload at the hash of its bytes.
          const sum = this.hashes.get(op.blob!);
          if (sum) this.objects.set(`sha256-${sum}`, size);
          break;
        }
        case "frame":
          place({ path: `${stem(op.path!)}.png`, type: "image/png", size: 100, w: 1920, h: 1080, upload: true, frame: op.auto ? { auto: true } : { t: op.t }, ...(op.edit ? { edit: op.edit } : {}) });
          break;
        case "edit":
          files[i] = { ...cur!, edit: op.edit };
          if (!op.edit) delete files[i]!.edit;
          break;
        case "remove":
          files.splice(i, 1);
          break;
        case "attach":
          files.splice(i, 1);
          files.push({ ...cur!, unattached: undefined, ...(op.meta ? { meta: op.meta } : {}) });
          break;
        case "rename":
          files[i] = { ...cur!, path: /\.\w+$/.test(op.to!) ? op.to! : `${op.to}${cur!.path.slice(stem(cur!.path).length)}` };
          break;
        case "move":
          files.splice(i, 1);
          files.splice(op.index!, 0, cur!);
          break;
      }
    }
    files = files.map((f) => {
      const failed = this.failures.get(f.path);
      return JSON.parse(JSON.stringify(failed ? { ...f, failed } : f)) as FileInfo;
    });
    this.items.set(key(ref), files);
    this.pendingLeft = this.pendingReads;
    this.stagedLeft = this.stagedReads;
    return files;
  }

  private ticket(t: string): Upload {
    const u = this.uploads.get(t);
    if (!u || u.complete) throw new ContentKitError("not_found", "multipart upload not found", { status: 404 });
    return u;
  }
}

const key = (ref: { kind: string; id: string }) => `${ref.kind}/${ref.id}`;

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

/** A client on a FakeServer. */
export function fakeClient(s: FakeServer, o: MediaOptions = {}): ContentKitClient {
  return createContentKitClient({
    baseUrl: "http://x",
    mounts: { upload: "http://x/api", media: "http://x/read" },
    fetch: s.fetch,
    media: { transport: s.transport, retryDelay: () => 0, ...o },
  });
}
