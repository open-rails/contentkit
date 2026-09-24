import { centeredCrop, editedSize, rotation, type Edit, type SlotManifest, type Transport } from "@openrails/contentkit-upload";

const WIDTHS: Record<string, number[]> = { avatar: [128, 256, 512], cover: [1500, 3000] };
const ASPECT: Record<string, [number, number]> = { avatar: [1, 1], cover: [3, 1] };

/** An in-browser stand-in for media.UploadHandler that renders slot outputs with canvas. */
export class DemoServer {
  private blobs = new Map<string, Blob>();
  private slots = new Map<string, SlotManifest>();
  private encoding = new Map<string, Promise<unknown>>();
  private version = 0;

  private aspect(slot: string) {
    const [w, h] = ASPECT[slot]!;
    return w / h;
  }
  delay = 250;

  fetch: typeof fetch = async (input, init) => {
    const path = new URL(String(input), location.href).pathname.replace(/^\/api/, "");
    const b = JSON.parse(String(init?.body));
    await sleep(this.delay);
    const key = `${b.ref?.kind}/${b.ref?.id}#${b.slot}`;
    switch (path) {
      case "/slot-original":
        return new Response(this.blobs.get(key)!, { status: 200 });
      case "/presign":
        return json({ name: b.slot, put: { method: "PUT", url: `demo://${key}`, headers: {}, expires: "" } });
      case "/commit-slot":
      case "/edit-slot": {
        // Encoding is asynchronous, like the server's queue: answer pending first.
        const done = this.render(key, b.slot, b.edit);
        this.encoding.set(key, done);
        void done.then(() => this.encoding.delete(key));
        const prev = this.slots.get(key);
        return json({ aspect: this.aspect(b.slot), outputs: prev?.outputs ?? [], ...(b.edit ? { edit: b.edit } : {}), pending: true });
      }
      case "/slot": {
        const m = this.slots.get(key) ?? { aspect: this.aspect(b.slot), outputs: [], pending: false };
        return json({ ...m, pending: this.encoding.has(key) });
      }
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

  async seed(ref: { kind: string; id: string }, slot: string, original: Blob, edit?: Edit) {
    const key = `${ref.kind}/${ref.id}#${slot}`;
    this.blobs.set(key, original);
    await this.render(key, slot, edit);
  }

  private async render(key: string, slot: string, edit?: Edit): Promise<SlotManifest> {
    await sleep(this.delay * 3);
    const bmp = await createImageBitmap(this.blobs.get(key)!, { imageOrientation: "from-image" });
    const [aw, ah] = ASPECT[slot]!;
    const rot = rotation(edit?.rotate ?? 0);
    const src = { width: bmp.width, height: bmp.height };
    const c = edit?.crop ?? centeredCrop(src, aw / ah, rot);
    const out = editedSize({ width: c.w, height: c.h }, rot);
    const outputs = [];
    for (const width of WIDTHS[slot]!.filter((w) => w <= Math.max(out.width, WIDTHS[slot]![0]!))) {
      const height = Math.round((width * ah) / aw);
      const cv = new OffscreenCanvas(width, height);
      const g = cv.getContext("2d")!;
      g.imageSmoothingQuality = "high";
      g.translate(width / 2, height / 2);
      g.rotate((rot * Math.PI) / 180);
      const [dw, dh] = rot === 90 || rot === 270 ? [height, width] : [width, height];
      g.drawImage(bmp, c.x, c.y, c.w, c.h, -dw / 2, -dh / 2, dw, dh);
      const url = URL.createObjectURL(await cv.convertToBlob({ type: "image/webp", quality: 0.9 }));
      outputs.push({ name: `${slot}_${width}`, w: width, h: height, url });
    }
    const version = String(++this.version);
    const m: SlotManifest = { aspect: aw / ah, ...(edit ? { edit } : {}), dims: { w: src.width, h: src.height }, version, outputs, pending: false };
    this.slots.set(key, m);
    return m;
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
