import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { VersionText, Content, ContentInput, ContentPatch, Version } from "../client/generated/wire.js";
import { IMAGE_SCHEME, imageRef, imageRefs, type ManageQuery } from "../client/contents.js";
import { selectedEdition } from "../client/edition-access.js";
import { nextPublicationRefresh } from "../client/publication.js";
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
  saveText: (text: VersionText) => Promise<Version>;
  /** Publishes now, or schedules for at. */
  publish: (at?: string) => Promise<Content>;
  unpublish: () => Promise<Content>;
  /** Deletes the selected edition; the last edition deletes its parent too. */
  removeVersion: () => Promise<void>;
  /** Restores the selected edition and its automatically deleted parent. */
  restoreVersion: () => Promise<Version>;
  /** Restores an independently hidden parent (such as moderation). */
  restore: () => Promise<Content>;
  /**
   * The version whose cover and body images the hook edits: the one asked
   * for, else the selected edition. Every version owns its text, cover and images.
   */
  version: Version | null;
  /** Uploads the version's cover (null returns to its first upload); resolves with a URL that shows it now. */
  setCover: (file: Blob | null) => Promise<string | null>;
  /** Uploads an image for this version's body; resolves with a URL that shows it now, to place in the editor. */
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
  text: VersionText | undefined;
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
 * publish, version deletion and restoration, a version's cover and the body's
 * images. null creates a new item.
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
  const key = cid ? keyOf("content", scope, cid, language, o.version) : null;
  const r = useResource<Content | null>(store, key, { type: "content", id: cid ?? undefined }, (s) => o.version ? client.contents.getByVersion(o.version, language, s) : client.contents.get(cid!, language, s), { initial: o.initial });
  const content = r.data ?? null;
  const reloadPublication = r.reload;
  useEffect(() => {
    if (r.loading || r.error) return;
    const at = nextPublicationRefresh(content);
    if (at === null) return;
    const timer = setTimeout(reloadPublication, Math.min(2 ** 31 - 1, Math.max(0, at - Date.now()) + 250));
    return () => clearTimeout(timer);
  }, [content, r.loading, r.error, reloadPublication]);
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
  const version = useMemo(() => content ? selectedEdition(content, o.version) ?? null : null, [content, o.version]);
  const stored = version?.text?.[language];
  const body = stored?.body;
  const vid = version?.id;
  const cover = version?.cover;
  const needVersion = useCallback(() => {
    if (!vid) throw new Error("contentkit-ui: the item has no version yet");
    return vid;
  }, [vid]);

  // Images: fresh uploads show from their file, published ones from
  // content.images, the rest (an unpublished item's) from an editor read.
  const [previewState, setPreviews] = useState<{ version?: string; images: ReadonlyMap<string, string> }>({ images: new Map() });
  const previews = useMemo(() => previewState.version === vid ? previewState.images : new Map<string, string>(), [previewState, vid]);
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
  // The cover and the body's images are the version's.
  const coverPending = !!cover?.image && !cover.url && !previews.has(cover.image) && !published.has(cover.image);
  const bodyPending = useMemo(() => refNames(body).filter((n) => !previews.has(n) && !published.has(n)), [body, previews, published]);
  const pending = useMemo(() => (coverPending ? [...bodyPending, cover!.image!] : bodyPending), [coverPending, bodyPending, cover]);
  const folder = useMemo(() => ({ kind, id: cid ?? "none", version: vid }), [kind, cid, vid]);
  const folderRead = useMediaRead(folder, { editor: true, read: pending.length && vid ? undefined : null, client });
  const files = useMemo(() => folderRead.read?.files ?? [], [folderRead.read]);
  const views = useMemo(() => {
    const out = new Map<string, string>();
    for (const f of files) if (f.upload && f.editor_url) out.set(uploadName(f.path), f.editor_url);
    return out;
  }, [files]);
  // Editor views render on the first editor read that misses them: read again, backing off.
  const failed = (n: string) => files.some((f) => f.failed && uploadName(f.path) === n);
  const missing = !!folderRead.read && pending.some((n) => !views.has(n) && !failed(n));
  const tries = useRef(0);
  const reload = folderRead.reload;
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
  const shown = useMemo(() => new Map<string, string>(), [vid]);
  useEffect(() => {
    for (const m of [previews, published, views]) for (const [n, u] of m) shown.set(u, n);
  }, [previews, published, views, shown]);

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
      const back = new Map(shown);
      for (const m of [previews, published, views]) for (const [n, u] of m) back.set(u, n);
      let out = html;
      for (const [u, n] of back) out = out.split(u).join(imageRef(n)).split(u.replaceAll("&", "&amp;")).join(imageRef(n));
      return out;
    },
    [previews, published, views, shown],
  );
  const preview = useCallback((name: string, file: Blob): string | undefined => {
    if (typeof URL.createObjectURL !== "function") return undefined;
    const url = URL.createObjectURL(file);
    blobs.current.push(url);
    setPreviews((state) => ({ version: vid, images: new Map(state.version === vid ? state.images : []).set(name, url) }));
    return url;
  }, [vid]);

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
  const saveText = useCallback(
    (text: VersionText) => run(() => client.contents.updateVersion(need(), needVersion(), {
      text: { ...version?.text, [language]: text.body === undefined ? text : { ...text, body: storedBody(text.body) } },
    })),
    [client, run, need, needVersion, version?.text, language, storedBody],
  );
  const publish = useCallback((at?: string) => run(() => client.contents.publish(need(), at)), [client, run, need]);
  const unpublish = useCallback(() => run(() => client.contents.unpublish(need())), [client, run, need]);
  const removeVersion = useCallback(() => run(() => client.contents.removeVersion(need(), needVersion())), [client, run, need, needVersion]);
  const restoreVersion = useCallback(() => run(() => client.contents.updateVersion(need(), needVersion(), { state: "published" })), [client, run, need, needVersion]);
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
        if (url) shown.set(url, up.name);
        return url;
      }),
    [client, run, need, needVersion, kind, preview, shown],
  );
  const uploadImage = useCallback(
    (file: Blob) =>
      run(async () => {
        const up = await client.media.uploadInline({ kind, id: need(), version: needVersion() }, file);
        const url = preview(up.name, file) ?? up.url ?? up.ref;
        shown.set(url, up.name);
        return url;
      }),
    [client, run, need, needVersion, kind, preview, shown],
  );
  return {
    content,
    reload: r.reload,
    loading: !!key && (r.loading || (!r.loaded && o.initial === undefined)),
    error: r.error,
    saving: inflight > 0,
    create,
    update,
    saveText,
    publish,
    unpublish,
    removeVersion,
    restoreVersion,
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
