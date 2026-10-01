import { centeredCrop, fill, publicURL, rotation, stem, type Edit, type FileInfo, type Op, type PublicImage, type RefBody, type Transport } from "@openrails/contentkit-upload";

/** The demo kinds' public presets by upload path: fixed names, as the app's registry declares them. */
const PRESETS: Record<string, { to: string; widths: number[]; aspect: [number, number] }> = {
  avatar: { to: "avatar-{w}.webp", widths: [128, 256, 512], aspect: [1, 1] },
  cover: { to: "cover-{w}.webp", widths: [1500, 3000], aspect: [3, 1] },
  poster: { to: "poster-{w}.webp", widths: [480, 960, 1920], aspect: [16, 9] },
};

export const NAMESPACE = "demo";

/**
 * An in-browser stand-in for media.UploadHandler, Reader.Handler and the
 * worker: it keeps each item's uploads, renders public presets with canvas
 * and writes them to e2e/media-server.ts, which serves /v1/ like media-access.
 */
export class DemoServer {
  private blobs = new Map<string, Blob>();
  private items = new Map<string, FileInfo[]>();
  private views = new Map<string, string>();
  delay = 250;

  constructor(readonly media: string) {}

  /** The public preset of an upload path for an item. */
  image(ref: RefBody, path: string): PublicImage {
    const p = PRESETS[path]!;
    return { base: this.media, namespace: NAMESPACE, kind: ref.kind, id: ref.id, to: p.to, widths: p.widths, aspect: p.aspect.join(":") };
  }

  fetch: typeof fetch = async (input, init) => {
    const url = new URL(String(input), location.href);
    if (url.pathname === "/api/frame") {
      await sleep(this.delay / 2);
      const w = Number(url.searchParams.get("w") ?? 640);
      return new Response(await videoFrame(Number(url.searchParams.get("t")), w, "image/jpeg"), { status: 200 });
    }
    await sleep(this.delay);
    if (url.pathname.startsWith("/read/")) {
      const [, , kind, id] = url.pathname.split("/");
      const files = this.uploads({ kind: kind!, id: id! }).map((f) => ({ ...f, editor_url: this.view(f) }));
      return json({ access: "full", expires: 0, total: files.length, offset: 0, limit: 50, files });
    }
    const b = JSON.parse(String(init?.body));
    switch (url.pathname) {
      case "/api/presign": {
        const blob = `sha256-${b.sha256}`;
        const path = /\.\w+$/.test(b.path) ? b.path : `${b.path}.${b.type === "image/png" ? "png" : "jpg"}`;
        return json(this.blobs.has(blob) ? { path, blob, exists: true } : { path, blob, put: { method: "PUT", url: `demo://${blob}`, headers: {}, expires: "" } });
      }
      case "/api/commit":
        return json({ files: await this.commit(b.ref, b.ops) });
    }
    return json({ error: "not found", code: "not_found" }, 404);
  };

  transport: Transport = async (req, body, { onProgress }) => {
    for (let i = 1; i <= 8; i++) {
      await sleep(this.delay / 4);
      onProgress?.((body.size * i) / 8);
    }
    this.blobs.set(req.url.slice("demo://".length), body);
  };

  /** Puts an image as an item's upload, rendered. */
  async seed(ref: RefBody, path: string, image: Blob, edit?: Edit) {
    const blob = `sha256-seed-${ref.id}-${path}`;
    this.blobs.set(blob, image);
    await this.commit(ref, [{ op: "put", path: `${path}.jpg`, blob, ...(edit ? { edit } : {}) }]);
    await sleep(this.delay * 4);
  }

  /** Adds a video upload to an item. */
  seedVideo(ref: RefBody) {
    this.uploads(ref).push({ path: "source.mp4", type: "video/mp4", size: 1, w: 1920, h: 1080, dur: VIDEO_SECONDS, upload: true });
  }

  private uploads(ref: RefBody): FileInfo[] {
    const key = `${ref.kind}/${ref.id}`;
    if (!this.items.has(key)) this.items.set(key, []);
    return this.items.get(key)!;
  }

  private view(f: FileInfo): string | undefined {
    const blob = (f as { blob?: string }).blob;
    if (!blob || !f.type.startsWith("image/")) return undefined;
    if (!this.views.has(blob)) this.views.set(blob, URL.createObjectURL(this.blobs.get(blob)!));
    return this.views.get(blob);
  }

  private async commit(ref: RefBody, ops: Op[]): Promise<FileInfo[]> {
    const files = this.uploads(ref);
    const at = (p: string) => files.findIndex((f) => stem(f.path) === stem(p));
    for (const op of ops) {
      const i = at(op.path!);
      if (op.op === "remove") {
        if (i < 0) throw new Error("no upload");
        const [f] = files.splice(i, 1);
        void this.unpublish(ref, f!);
        continue;
      }
      let f: FileInfo & { blob?: string };
      if (op.op === "put") {
        const src = this.blobs.get(op.blob!)!;
        const bmp = await createImageBitmap(src, { imageOrientation: "from-image" });
        f = { path: op.path!, type: src.type || "image/jpeg", size: src.size, w: bmp.width, h: bmp.height, upload: true, blob: op.blob };
      } else if (op.op === "frame") {
        const t = op.t ?? 3;
        const blob = `sha256-frame-${t}`;
        this.blobs.set(blob, await videoFrame(t, 1920, "image/png"));
        f = { path: `${stem(op.path!)}.png`, type: "image/png", size: 1, w: 1920, h: 1080, upload: true, blob, frame: op.auto ? { auto: true, t } : { t } };
      } else f = { ...files[i]! };
      if (op.edit) f.edit = op.edit;
      else if (op.op === "edit" || op.op === "frame") delete f.edit;
      f.pending = [stem(f.path)];
      if (i >= 0) files[i] = f;
      else files.push(f);
      // Rendering is asynchronous, like the worker's queue: pending first.
      void this.render(ref, f);
    }
    return files.map((f) => ({ ...f }));
  }

  private async render(ref: RefBody, f: FileInfo & { blob?: string }) {
    await sleep(this.delay * 3);
    const preset = PRESETS[stem(f.path)];
    if (!preset) return;
    const bmp = await createImageBitmap(this.blobs.get(f.blob!)!, { imageOrientation: "from-image" });
    const [aw, ah] = preset.aspect;
    const rot = rotation(f.edit?.rotate ?? 0);
    const c = f.edit?.crop ?? centeredCrop({ width: bmp.width, height: bmp.height }, aw / ah, rot);
    for (const width of preset.widths) {
      const height = Math.round((width * ah) / aw);
      const cv = new OffscreenCanvas(width, height);
      const g = cv.getContext("2d")!;
      g.imageSmoothingQuality = "high";
      g.translate(width / 2, height / 2);
      g.rotate((rot * Math.PI) / 180);
      const [dw, dh] = rot === 90 || rot === 270 ? [height, width] : [width, height];
      g.drawImage(bmp, c.x, c.y, c.w, c.h, -dw / 2, -dh / 2, dw, dh);
      const body = await cv.convertToBlob({ type: "image/webp", quality: 0.9 });
      await fetch(publicURL(this.media, NAMESPACE, ref.kind, ref.id, fill(preset.to, { w: width })), { method: "PUT", body, headers: { "Content-Type": "image/webp" } });
    }
    delete f.pending;
  }

  private async unpublish(ref: RefBody, f: FileInfo) {
    const preset = PRESETS[stem(f.path)];
    for (const w of preset?.widths ?? []) await fetch(publicURL(this.media, NAMESPACE, ref.kind, ref.id, fill(preset!.to, { w })), { method: "DELETE" });
  }
}

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** A landscape photo-like scene for the seeded cover and avatar. */
export async function sampleImage(width: number, height: number, hue: number): Promise<Blob> {
  const cv = new OffscreenCanvas(width, height);
  const g = cv.getContext("2d")!;
  const sky = g.createLinearGradient(0, 0, 0, height);
  sky.addColorStop(0, `hsl(${hue} 70% 62%)`);
  sky.addColorStop(0.6, `hsl(${hue + 30} 75% 78%)`);
  sky.addColorStop(1, `hsl(${hue + 50} 60% 85%)`);
  g.fillStyle = sky;
  g.fillRect(0, 0, width, height);
  g.fillStyle = "hsl(45 95% 70%)";
  g.beginPath();
  g.arc(width * 0.68, height * 0.38, height * 0.13, 0, Math.PI * 2);
  g.fill();
  const hills: [number, string][] = [[0.62, `hsl(${hue + 160} 30% 42%)`], [0.72, `hsl(${hue + 150} 35% 30%)`], [0.84, `hsl(${hue + 140} 40% 20%)`]];
  for (const [y, color] of hills) {
    g.fillStyle = color;
    g.beginPath();
    g.moveTo(0, height);
    for (let x = 0; x <= width; x += width / 12) g.lineTo(x, height * y + Math.sin(x / (width / 7) + y * 9) * height * 0.06);
    g.lineTo(width, height);
    g.fill();
  }
  return cv.convertToBlob({ type: "image/jpeg", quality: 0.9 });
}

/** A face-like avatar source: a bust on a soft background. */
export async function sampleAvatar(size: number): Promise<Blob> {
  const cv = new OffscreenCanvas(size * 1.5, size);
  const g = cv.getContext("2d")!;
  const bg = g.createRadialGradient(size * 0.75, size * 0.4, 10, size * 0.75, size * 0.5, size);
  bg.addColorStop(0, "hsl(200 70% 80%)");
  bg.addColorStop(1, "hsl(260 45% 45%)");
  g.fillStyle = bg;
  g.fillRect(0, 0, size * 1.5, size);
  g.fillStyle = "hsl(25 45% 35%)";
  g.beginPath();
  g.ellipse(size * 0.75, size * 1.05, size * 0.38, size * 0.33, 0, 0, Math.PI * 2);
  g.fill();
  g.fillStyle = "hsl(28 60% 72%)";
  g.beginPath();
  g.arc(size * 0.75, size * 0.45, size * 0.2, 0, Math.PI * 2);
  g.fill();
  g.fillStyle = "hsl(20 30% 20%)";
  g.beginPath();
  g.arc(size * 0.75, size * 0.37, size * 0.21, Math.PI, Math.PI * 2);
  g.fill();
  return cv.convertToBlob({ type: "image/jpeg", quality: 0.9 });
}

const VIDEO_SECONDS = 12;

/** A frame of the demo video: colour bars that drift, with the timestamp burned in. */
async function videoFrame(t: number, width: number, type: string): Promise<Blob> {
  const w = Math.max(64, Math.min(1920, width));
  const h = Math.round((w * 9) / 16);
  const cv = new OffscreenCanvas(w, h);
  const g = cv.getContext("2d")!;
  const bars = 7;
  for (let i = 0; i < bars; i++) {
    g.fillStyle = `hsl(${(i * 360) / bars + t * 30} 70% 55%)`;
    g.fillRect((i * w) / bars, 0, w / bars + 1, h);
  }
  g.fillStyle = "rgba(0,0,0,.55)";
  g.beginPath();
  g.arc(w * (0.15 + (0.7 * t) / VIDEO_SECONDS), h * 0.5, h * 0.18, 0, Math.PI * 2);
  g.fill();
  g.fillStyle = "#fff";
  g.font = `600 ${Math.round(h / 8)}px system-ui, sans-serif`;
  g.fillText(`${t.toFixed(2)} s`, w * 0.05, h * 0.9);
  return cv.convertToBlob({ type, quality: 0.85 });
}
