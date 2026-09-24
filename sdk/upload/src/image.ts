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
