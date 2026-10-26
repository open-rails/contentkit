const reference = /^contentkit:(i-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/;

function source(value: string, images: Readonly<Record<string, string>>): string | null {
  const name = reference.exec(value)?.[1];
  if (!name) return null;
  const url = Object.hasOwn(images, name) ? images[name] : undefined;
  if (!url || url.trim() !== url || [...url].some((character) => character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127)) return "";
  if (url.startsWith("/") && !url.startsWith("//") && !url.includes("\\")) return url;
  try {
    const parsed = new URL(url);
    return parsed.protocol === "https:" || parsed.protocol === "http:" ? url : "";
  } catch {
    return "";
  }
}

/**
 * Resolve inline image references using only the selected edition's images map.
 * Missing or unsafe URLs leave no image source; another edition is never read.
 * This is reference resolution, not an HTML sanitizer: hosts retain their normal
 * markup sanitization. Without a DOMParser, HTML is returned unchanged; SSR
 * hosts should resolve HTML after hydration. Quill resolution works on servers.
 */
export function resolveInlineImageReferences(body: string, images: Readonly<Record<string, string>>, format: "html" | "quill" = "html"): string {
  if (!body.includes("contentkit:")) return body;
  if (format === "quill") {
    let value: unknown;
    try { value = JSON.parse(body); } catch { return body; }
    const object = value && typeof value === "object" ? value as Record<string, unknown> : null;
    const operations = Array.isArray(value) ? value : object && Array.isArray(object.ops) ? object.ops : null;
    if (!operations) return body;
    const resolved = operations.flatMap((operation: unknown) => {
      if (!operation || typeof operation !== "object") return [operation];
      const op = operation as Record<string, unknown>;
      const insert = op.insert && typeof op.insert === "object" ? op.insert as Record<string, unknown> : null;
      if (!insert || typeof insert.image !== "string") return [op];
      const url = source(insert.image, images);
      return url === null ? [op] : url ? [{ ...op, insert: { ...insert, image: url } }] : [];
    });
    return JSON.stringify(Array.isArray(value) ? resolved : { ...object, ops: resolved });
  }
  if (typeof DOMParser === "undefined") return body;
  const document = new DOMParser().parseFromString(body, "text/html");
  for (const image of document.body.querySelectorAll("img[src]")) {
    const url = source(image.getAttribute("src")!, images);
    if (url === null) continue;
    if (url) image.setAttribute("src", url);
    else image.removeAttribute("src");
  }
  return document.body.innerHTML;
}
