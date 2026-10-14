import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { CatalogText, Content, ContentInput, ContentPatch, Version } from "../client/generated/wire.js";
import { BODY_SUFFIX, IMAGE_SCHEME, imageRef, imageRefs, type ManageQuery } from "../client/contents.js";
import { useMediaRead } from "./read.js";
import { useGuarded } from "./context.js";
import { keyOf, offsetPages, useContentScope, useList, useResource, type UseList } from "./use-resource.js";

export interface ManageFilter extends Omit<ManageQuery, "limit" | "offset"> {
  pageSize?: number;
}

/** The content items the caller manages, newest first, a page at a time. */
export function useManagedContents(f: ManageFilter = {}, o: { client?: ContentKitClient } = {}): UseList<Content> {
  const { client, store, scope } = useContentScope(o.client);
  const { pageSize, ...q } = f;
  const size = pageSize ?? 20;
  return useList(
    store,
    keyOf("contents", scope, q.kind, q.state, q.owner_group, q.q, q.language, size),
    { type: "contents", scope: q.state === "deleted" ? "deleted" : undefined },
    offsetPages(size, (page, signal) => client.contents.manage({ ...q, ...page }, signal)),
  );
}

export interface UseContent {
  /** null before create (or while loading). */
  content: Content | null;
  loading: boolean;
  error?: ContentKitError;
  /** Writes in flight. */
  saving: boolean;
  /** Reads the item again. */
  reload: () => void;
  /** Creates the item as a draft; the hook then edits it. */
  create: (input: ContentInput) => Promise<Content>;
  update: (patch: ContentPatch) => Promise<Content>;
  /** Sets the text in the hook's language; a body from the editor is stored with image references (storedBody). */
  putText: (text: CatalogText) => Promise<Content>;
  /** Publishes now, or schedules for at. */
  publish: (at?: string) => Promise<Content>;
  unpublish: () => Promise<Content>;
  remove: () => Promise<void>;
  restore: () => Promise<Content>;
  /**
   * The version whose cover and body images the hook edits: the one asked
   * for, else the first. Every version has its own cover and its own copy of
   * the body's images.
   */
  version: Version | null;
  /** Uploads the version's cover (null returns to its first upload); resolves with a URL that shows it now. */
  setCover: (file: Blob | null) => Promise<string | null>;
  /** Uploads an image for the body into the version, copied into the others; resolves with a URL that shows it now, to place in the editor. */
  uploadImage: (file: Blob) => Promise<string>;
  /**
   * A URL that shows one of the item's images now, by name ("i-{uuid}") or
   * reference: a fresh upload from its file, else its public file
   * (`content.images`), else, while it has none (an unpublished item's), its
   * signed editor view from an editor read. Anything else is returned as is.
   */
  imageSrc: (image: string | null | undefined) => string | undefined;
  /** The version's cover through imageSrc. */
  coverSrc: string | undefined;
  /** The language's stored text, its body's images through imageSrc: what an editor shows. */
  text: CatalogText | undefined;
  /** HTML from the editor with every image URL this hook showed turned back into the image's reference. */
  storedBody: (html: string) => string;
}

/** The names of the images a body references, once each. */
const refNames = (body: string | undefined) => [...new Set([...(body ?? "").matchAll(imageRefs())].map((m) => m[1]!))];
const uploadName = (path: string) => {
  const base = path.slice(path.lastIndexOf("/") + 1);
  const dot = base.lastIndexOf(".");
  return dot > 0 ? base.slice(0, dot) : base;
};

/**
 * One content item of kind for its editors: read, create, text in language,
 * publish, delete, restore, a version's cover and the body's images. null
 * creates a new item.
 */
export function useContent(
  id: string | null | undefined,
  kind: string,
  language: string,
  o: { initial?: Content; client?: ContentKitClient; version?: string } = {},
): UseContent {
  const { client, store, scope } = useContentScope(o.client);
  const [created, setCreated] = useState<string | null>(null);
  const cid = id ?? created;
  const key = cid ? keyOf("content", scope, cid) : null;
  const r = useResource<Content | null>(store, key, { type: "content", id: cid ?? undefined }, (s) => client.contents.get(cid!, language, s), { initial: o.initial });
  const content = r.data ?? null;
  const [inflight, setInflight] = useState(0);
  const guarded = useGuarded();
  const run = useCallback(async <T>(fn: () => Promise<T>): Promise<T> => {
    setInflight((n) => n + 1);
    try {
      return await guarded(fn);
    } finally {
      setInflight((n) => n - 1);
    }
  }, [guarded]);
  const need = useCallback(() => {
    if (!cid) throw new Error("contentkit-ui: create the item first");
    return cid;
  }, [cid]);
  const stored = content?.text?.[language];
  const body = stored?.body;
  const version = useMemo(() => {
    const vs = [...(content?.versions ?? [])].sort((a, b) => a.position - b.position);
    return vs.find((v) => v.id === o.version) ?? vs[0] ?? null;
  }, [content?.versions, o.version]);
  const vid = version?.id;
  const cover = version?.cover;
  const needVersion = useCallback(() => {
    if (!vid) throw new Error("contentkit-ui: the item has no version yet");
    return vid;
  }, [vid]);

  // Images: fresh uploads show from their file, published ones from
  // content.images, the rest (an unpublished item's) from an editor read.
  const [previews, setPreviews] = useState<ReadonlyMap<string, string>>(new Map());
  const blobs = useRef<string[]>([]);
  useEffect(() => {
    const urls = blobs.current;
    return () => {
      for (const u of urls) URL.revokeObjectURL(u);
    };
  }, []);
  const published = useMemo(() => {
    const out = new Map(Object.entries(content?.images ?? {}));
    if (cover?.image && cover.url) out.set(cover.image, cover.url);
    return out;
  }, [content?.images, cover?.image, cover?.url]);
  // The cover is the version's folder's; body images its body folder's ({kind}.body).
  const coverPending = !!cover?.image && !cover.url && !previews.has(cover.image) && !published.has(cover.image);
  const bodyPending = useMemo(() => refNames(body).filter((n) => !previews.has(n) && !published.has(n)), [body, previews, published]);
  const pending = useMemo(() => (coverPending ? [...bodyPending, cover!.image!] : bodyPending), [coverPending, bodyPending, cover]);
  const folder = useMemo(() => ({ kind, id: cid ?? "none", version: vid }), [kind, cid, vid]);
  const bodyFolder = useMemo(() => ({ kind: kind + BODY_SUFFIX, id: cid ?? "none", version: vid }), [kind, cid, vid]);
  const coverRead = useMediaRead(folder, { editor: true, read: coverPending && vid ? undefined : null, client });
  const bodyRead = useMediaRead(bodyFolder, { editor: true, read: bodyPending.length && vid ? undefined : null, client });
  const files = useMemo(() => [...(coverRead.read?.files ?? []), ...(bodyRead.read?.files ?? [])], [coverRead.read, bodyRead.read]);
  const views = useMemo(() => {
    const out = new Map<string, string>();
    for (const f of files) if (f.upload && f.editor_url) out.set(uploadName(f.path), f.editor_url);
    return out;
  }, [files]);
  // Editor views render on the first editor read that misses them: read again, backing off.
  const failed = (n: string) => files.some((f) => f.failed && uploadName(f.path) === n);
  const missing = (!!coverRead.read || !!bodyRead.read) && pending.some((n) => !views.has(n) && !failed(n));
  const tries = useRef(0);
  const reloadCover = coverRead.reload;
  const reloadBody = bodyRead.reload;
  const reload = useCallback(() => {
    reloadCover();
    reloadBody();
  }, [reloadCover, reloadBody]);
  useEffect(() => {
    if (!missing) {
      tries.current = 0;
      return;
    }
    if (tries.current >= 6) return;
    const t = setTimeout(() => {
      tries.current++;
      reload();
    }, 1000 * 2 ** tries.current);
    return () => clearTimeout(t);
  }, [missing, files, reload]);

  // Every URL shown for an image, of this and earlier reads, back to its name.
  const shown = useRef(new Map<string, string>());
  useEffect(() => {
    for (const m of [previews, published, views]) for (const [n, u] of m) shown.current.set(u, n);
  }, [previews, published, views]);

  const imageSrc = useCallback(
    (image: string | null | undefined) => {
      if (!image) return undefined;
      const n = image.startsWith(IMAGE_SCHEME) ? image.slice(IMAGE_SCHEME.length) : image;
      return previews.get(n) ?? published.get(n) ?? views.get(n) ?? image;
    },
    [previews, published, views],
  );
  const text = useMemo(
    () => (stored ? { ...stored, body: (stored.body ?? "").replace(imageRefs(), (ref, n: string) => previews.get(n) ?? published.get(n) ?? views.get(n) ?? ref) } : undefined),
    [stored, previews, published, views],
  );
  const storedBody = useCallback(
    (html: string) => {
      const back = new Map(shown.current);
      for (const m of [previews, published, views]) for (const [n, u] of m) back.set(u, n);
      let out = html;
      for (const [u, n] of back) out = out.split(u).join(imageRef(n)).split(u.replaceAll("&", "&amp;")).join(imageRef(n));
      return out;
    },
    [previews, published, views],
  );
  const preview = useCallback((name: string, file: Blob): string | undefined => {
    if (typeof URL.createObjectURL !== "function") return undefined;
    const url = URL.createObjectURL(file);
    blobs.current.push(url);
    setPreviews((m) => new Map(m).set(name, url));
    return url;
  }, []);

  const create = useCallback(
    (input: ContentInput) =>
      run(async () => {
        const c = await client.contents.create(input);
        setCreated(c.id);
        return c;
      }),
    [client, run],
  );
  const update = useCallback((patch: ContentPatch) => run(() => client.contents.update(need(), patch)), [client, run, need]);
  const putText = useCallback(
    (t: CatalogText) => run(() => client.contents.putText(need(), language, t.body === undefined ? t : { ...t, body: storedBody(t.body) })),
    [client, run, need, language, storedBody],
  );
  const publish = useCallback((at?: string) => run(() => client.contents.publish(need(), at)), [client, run, need]);
  const unpublish = useCallback(() => run(() => client.contents.unpublish(need())), [client, run, need]);
  const remove = useCallback(() => run(() => client.contents.remove(need())), [client, run, need]);
  const restore = useCallback(() => run(() => client.contents.restore(need())), [client, run, need]);
  const setCover = useCallback(
    (file: Blob | null) =>
      run(async () => {
        if (!file) {
          await client.contents.setCover(need(), needVersion(), null);
          return null;
        }
        const ref = { kind, id: need(), version: needVersion() };
        const up = await client.media.uploadNamed(ref, file, { path: await client.media.presetPath(kind, "cover", file) });
        const saved = await client.contents.setCover(ref.id, ref.version, up.name);
        const url = preview(up.name, file) ?? saved.cover?.url ?? null;
        if (url) shown.current.set(url, up.name);
        return url;
      }),
    [client, run, need, needVersion, kind, preview],
  );
  const uploadImage = useCallback(
    (file: Blob) =>
      run(async () => {
        const up = await client.media.uploadInline({ kind: kind + BODY_SUFFIX, id: need(), version: needVersion() }, file);
        const url = preview(up.name, file) ?? up.url ?? up.ref;
        shown.current.set(url, up.name);
        return url;
      }),
    [client, run, need, needVersion, kind, preview],
  );
  return {
    content,
    reload: r.reload,
    loading: !!key && (r.loading || (!r.loaded && o.initial === undefined)),
    error: r.error,
    saving: inflight > 0,
    create,
    update,
    putText,
    publish,
    unpublish,
    remove,
    restore,
    version,
    setCover,
    uploadImage,
    imageSrc,
    coverSrc: cover?.image ? imageSrc(cover.image) : (cover?.url ?? undefined),
    text,
    storedBody,
  };
}
