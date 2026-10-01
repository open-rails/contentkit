import { ratio, type AspectRatio } from "./aspect.js";
import type { Rendition } from "./rendition.js";

/**
 * A public file's URL, as media.PublicURL: `{base}/v1/{namespace}/{kind}/{id}/public/{name}`.
 * Public files live at fixed names; a missing one is served its kind's default.
 */
export function publicURL(base: string, namespace: string, kind: string, id: string, name: string): string {
  return `${base.replace(/\/+$/, "")}/v1/${namespace}/${kind}/${id}/public/${name}`;
}

/** Each `{key}` of template replaced by vars[key] ("" when missing), as the registry's templates fill. */
export function fill(template: string, vars: Record<string, string | number>): string {
  return template.replace(/\{([^}]*)\}/g, (_, k: string) => (k in vars ? String(vars[k]) : ""));
}

/** A srcset over a public template's widths, as media.SrcSet: "{url} 230w, …". */
export function srcSet(base: string, namespace: string, kind: string, id: string, to: string, widths: readonly number[]): string {
  return widths.map((w) => `${publicURL(base, namespace, kind, id, fill(to, { w }))} ${w}w`).join(", ");
}

/** One item's public image preset, as the app's registry declares it. */
export interface PublicImage {
  /** The site's media origin (the registry's BaseURL). */
  base: string;
  namespace: string;
  kind: string;
  id: string;
  /** The preset's To template, e.g. "cover-{w}.webp". */
  to: string;
  widths: readonly number[];
  /** The preset's "W:H"; "" or omitted is the source's own. */
  aspect?: AspectRatio;
}

/** The preset's files, narrowest first, with heights at its aspect. */
export function publicRenditions(p: PublicImage | null | undefined): Rendition[] {
  if (!p) return [];
  const r = ratio(p.aspect);
  return [...p.widths].sort((a, b) => a - b).map((w) => ({ w, h: r ? Math.round(w / r) : undefined, url: publicURL(p.base, p.namespace, p.kind, p.id, fill(p.to, { w })) }));
}

const listeners = new Set<() => void>();
let generation = 0;

/**
 * Refetches changed public files past the browser cache (`cache: "reload"`),
 * then remounts the images showing them: public files keep their URL, so
 * after a save the uploader sees the new bytes at once.
 */
export async function reloadPublic(urls: readonly string[]): Promise<void> {
  await Promise.all(
    urls.map((u) =>
      fetch(u, { cache: "reload", mode: "no-cors" }).then(
        (r) => r.body?.cancel(),
        () => {},
      ),
    ),
  );
  generation++;
  for (const fn of listeners) fn();
}

/** Subscribes to reloadPublic (useSyncExternalStore). */
export function subscribePublic(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export const publicGeneration = () => generation;
