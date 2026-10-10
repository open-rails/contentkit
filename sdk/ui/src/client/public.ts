import type { AspectRatio } from "./aspect.js";
import { sortRenditions, type Rendition } from "./rendition.js";

/**
 * A public file's URL, as media.PublicURL: `{base}/v1/{namespace}/{kind}/{id}/public/{name}`.
 * Pass an actual published name, not a preset template for a current image.
 */
export function publicURL(base: string, namespace: string, kind: string, id: string, name: string): string {
  return `${base.replace(/\/+$/, "")}/v1/${namespace}/${kind}/${id}/public/${name}`;
}

/** Each `{key}` of template replaced by vars[key] ("" when missing), as the registry's templates fill. */
export function fill(template: string, vars: Record<string, string | number>): string {
  return template.replace(/\{([^}]*)\}/g, (_, k: string) => (k in vars ? String(vars[k]) : ""));
}

/** A srcset from published URLs and their actual encoded widths. */
export function srcSet(renditions: readonly Rendition[]): string {
  return sortRenditions(renditions)
    .filter((r, i, all) => i === 0 || r.w !== all[i - 1]!.w)
    .map((r) => `${r.url} ${r.w}w`).join(", ");
}

/** One public image from a read or a host's authorized listing. */
export interface PublicImage {
  preset: string;
  renditions: readonly Rendition[];
  /** The preset's "W:H"; "" or omitted is the source's own. */
  aspect?: AspectRatio;
}

/** Published files, narrowest first; sizes and filenames are never inferred. */
export function publicRenditions(p: PublicImage | null | undefined): Rendition[] {
  if (!p) return [];
  return sortRenditions(p.renditions);
}
