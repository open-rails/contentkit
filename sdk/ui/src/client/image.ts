import type { Size } from "./crop.js";
import { UploadError } from "./errors.js";

/** An image to crop: a displayable URL and the size of the EXIF-oriented original. */
export interface CropSource extends Size {
  url: string;
  /** Set when the source was picked locally; revoke() frees its preview. */
  file?: File;
  revoke?: () => void;
}

export interface DecodeOptions {
  /** Longest side of the preview; the crop stays normalized to the original. Default 2048. */
  maxPreview?: number;
}

/**
 * Decodes a picked file with its EXIF orientation applied and returns a
 * downscaled preview URL plus the oriented original size. Throws
 * UploadError("decode") for files the browser cannot read as an image.
 */
export async function decodeImage(file: File, o: DecodeOptions = {}): Promise<CropSource> {
  const max = o.maxPreview ?? 2048;
  let bitmap: ImageBitmap;
  try {
    bitmap = await createImageBitmap(file, { imageOrientation: "from-image" });
  } catch (err) {
    throw new UploadError("decode", "the file is not a readable image", 0, undefined, { cause: err });
  }
  const { width, height } = bitmap;
  try {
    const scale = Math.min(1, max / Math.max(width, height));
    const w = Math.max(1, Math.round(width * scale));
    const h = Math.max(1, Math.round(height * scale));
    const blob = await render(bitmap, w, h, /png|webp|gif|avif/.test(file.type) ? "image/png" : "image/jpeg");
    const url = URL.createObjectURL(blob);
    return { url, width, height, file, revoke: () => URL.revokeObjectURL(url) };
  } finally {
    bitmap.close();
  }
}

async function render(bitmap: ImageBitmap, w: number, h: number, type: string): Promise<Blob> {
  if (typeof OffscreenCanvas !== "undefined") {
    const c = new OffscreenCanvas(w, h);
    c.getContext("2d")!.drawImage(bitmap, 0, 0, w, h);
    return c.convertToBlob({ type, quality: 0.9 });
  }
  const c = document.createElement("canvas");
  c.width = w;
  c.height = h;
  c.getContext("2d")!.drawImage(bitmap, 0, 0, w, h);
  return new Promise((resolve, reject) =>
    c.toBlob((b) => (b ? resolve(b) : reject(new UploadError("decode", "could not render a preview"))), type, 0.9),
  );
}

/**
 * Whether an image file is animated (GIF with several frames, animated WebP,
 * AVIF/HEIF sequence): a cheap pre-check for slots whose `animation` is
 * "reject". The server decides.
 */
export async function isAnimatedImage(file: Blob): Promise<boolean> {
  const head = new Uint8Array(await file.slice(0, 64).arrayBuffer());
  const ascii = (a: number, b: number) => String.fromCharCode(...head.subarray(a, b));
  if (ascii(0, 4) === "RIFF" && ascii(8, 12) === "WEBP") return ascii(12, 16) === "VP8X" && (head[20]! & 0x02) !== 0;
  if (ascii(4, 8) === "ftyp") {
    const brands = ascii(8, Math.min(head.length, (head[0]! << 24) | (head[1]! << 16) | (head[2]! << 8) | head[3]!));
    return /avis|msf1|hevs/.test(brands);
  }
  if (ascii(0, 3) === "GIF") return gifFrames(new Uint8Array(await file.arrayBuffer())) > 1;
  return false;
}

function gifFrames(b: Uint8Array): number {
  let i = 13;
  if (b[10]! & 0x80) i += 3 * (1 << ((b[10]! & 7) + 1));
  const skipBlocks = () => {
    while (i < b.length && b[i]! !== 0) i += b[i]! + 1;
    i++;
  };
  let frames = 0;
  while (i < b.length) {
    const t = b[i++];
    if (t === 0x21) {
      i++;
      skipBlocks();
    } else if (t === 0x2c) {
      if (++frames > 1) return frames;
      const packed = b[i + 8]!;
      i += 9;
      if (packed & 0x80) i += 3 * (1 << ((packed & 7) + 1));
      i++; // LZW minimum code size
      skipBlocks();
    } else break;
  }
  return frames;
}
