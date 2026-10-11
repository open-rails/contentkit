import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { Sort } from "../client/content/types.js";
import type { Post, PostInput } from "../client/generated/wire.js";
import { IMAGE_SCHEME, imageRef, imageRefs } from "../client/content/posts.js";
import { useMediaRead } from "./read.js";
import { keyOf, offsetPages, useContentScope, useList, useResource, type UseList } from "./use-resource.js";

export interface PostFilter {
  language?: string;
  sort?: Sort;
  /** Staff: every post, drafts and scheduled ones included (PostWrite). */
  admin?: boolean;
  /** With admin: true only drafts, false no drafts. */
  draft?: boolean;
  /** With admin: the deleted posts instead of the live ones. */
  deleted?: boolean;
  /** With admin: only posts whose title, excerpt or body contains this text. */
  q?: string;
  pageSize?: number;
}

/** Posts, a page at a time: the published ones, or every one for staff. */
export function usePosts(f: PostFilter = {}, o: { client?: ContentKitClient } = {}): UseList<Post> {
  const { client, store, scope } = useContentScope(o.client);
  const size = f.pageSize ?? 20;
  return useList(
    store,
    keyOf("posts", scope, !!f.admin, f.language, f.sort, f.draft, f.deleted, f.q, size),
    { type: "posts", scope: f.admin && f.deleted ? "deleted" : undefined },
    offsetPages(size, (q, signal) =>
      f.admin
        ? client.posts.adminList({ ...q, language: f.language, draft: f.draft, deleted: f.deleted, q: f.q }, signal)
        : client.posts.list({ ...q, language: f.language, sort: f.sort }, signal),
    ),
  );
}

export interface UsePost {
  /** null before create (or while loading). */
  post: Post | null;
  loading: boolean;
  error?: ContentKitError;
  /** Writes in flight. */
  saving: boolean;
  /** Creates the post; the hook then edits it. */
  create: (input: PostInput) => Promise<Post>;
  /** Updates the given fields; a body from the editor is stored with image references (storedBody). */
  update: (patch: PostInput) => Promise<Post>;
  remove: () => Promise<void>;
  /** Restores the deleted post (PostWrite). */
  restore: () => Promise<Post>;
  /** Uploads the cover (null clears it); resolves with a URL that shows it now. */
  setCover: (file: Blob | null) => Promise<string | null>;
  /** Uploads an image for the body; resolves with a URL that shows it now, to place in the editor. */
  uploadImage: (file: Blob) => Promise<string>;
  /**
   * A URL that shows one of the post's images now, by name ("i-{uuid}") or
   * reference: a fresh upload from its file, else its public file
   * (`post.images`), else, while it has none (an unpublished post's), its
   * signed editor view from an editor read. Anything else is returned as is.
   */
  imageSrc: (image: string | null | undefined) => string | undefined;
  /** The cover through imageSrc. */
  coverSrc: string | undefined;
  /** post.body with its images through imageSrc: what a rich-text editor shows. */
  editorBody: string | undefined;
  /** HTML from the editor with every image URL this hook showed turned back into the image's reference. */
  storedBody: (html: string) => string;
}

/** The names of the images a body still references, once each. */
const refNames = (body: string | undefined) => [...new Set([...(body ?? "").matchAll(imageRefs())].map((m) => m[1]!))];
const uploadName = (path: string) => {
  const base = path.slice(path.lastIndexOf("/") + 1);
  const dot = base.lastIndexOf(".");
  return dot > 0 ? base.slice(0, dot) : base;
};

/** Whether readers see the post, and so its images' public files exist. */
export function isPublished(post: Post, now = Date.now()): boolean {
  return !post.is_draft && !post.moderation && (!post.live_at || Date.parse(post.live_at) <= now);
}

/** One post: read, and for staff (PostWrite) create, update, delete, restore, cover and body images. null creates a new post. */
export function usePost(id: string | null | undefined, o: { initial?: Post; client?: ContentKitClient } = {}): UsePost {
  const { client, store, scope } = useContentScope(o.client);
  const [created, setCreated] = useState<string | null>(null);
  const pid = id ?? created;
  const key = pid ? keyOf("post", scope, pid) : null;
  const r = useResource<Post | null>(store, key, { type: "post", id: pid ?? undefined }, (s) => client.posts.get(pid!, s), { initial: o.initial });
  const post = r.data ?? null;
  const [inflight, setInflight] = useState(0);
  const run = useCallback(async <T>(fn: () => Promise<T>): Promise<T> => {
    setInflight((n) => n + 1);
    try {
      return await fn();
    } finally {
      setInflight((n) => n - 1);
    }
  }, []);
  const need = useCallback(() => {
    if (!pid) throw new Error("contentkit-ui: create the post first");
    return pid;
  }, [pid]);

  // Images: fresh uploads show from their file, published ones from post.images,
  // the rest (an unpublished post's) from an editor read.
  const [previews, setPreviews] = useState<ReadonlyMap<string, string>>(new Map());
  const blobs = useRef<string[]>([]);
  useEffect(() => {
    const urls = blobs.current;
    return () => {
      for (const u of urls) URL.revokeObjectURL(u);
    };
  }, []);
  const published = useMemo(() => new Map(Object.entries(post?.images ?? {})), [post?.images]);
  const pending = useMemo(() => {
    const names = refNames(post?.body);
    if (post?.cover && !post.cover_url && !names.includes(post.cover)) names.push(post.cover);
    return names.filter((n) => !previews.has(n));
  }, [post?.body, post?.cover, post?.cover_url, previews]);
  const folder = useMemo(() => client.media.postRef(pid ?? "none"), [client, pid]);
  const read = useMediaRead(folder, { editor: true, read: pending.length ? undefined : null, client });
  const views = useMemo(() => {
    const out = new Map<string, string>();
    for (const f of read.read?.files ?? []) if (f.upload && f.editor_url) out.set(uploadName(f.path), f.editor_url);
    return out;
  }, [read.read]);
  // Editor views render on the first editor read that misses them: read again, backing off.
  const failed = (n: string) => !!read.read?.files.some((f) => f.failed && uploadName(f.path) === n);
  const missing = !!read.read && pending.some((n) => !views.has(n) && !failed(n));
  const tries = useRef(0);
  const { reload } = read;
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
  }, [missing, read.read, reload]);

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
  const editorBody = useMemo(() => post?.body.replace(imageRefs(), (ref, n: string) => previews.get(n) ?? views.get(n) ?? ref), [post?.body, previews, views]);
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
    (input: PostInput) =>
      run(async () => {
        const post = await client.posts.create(input);
        setCreated(post.id);
        return post;
      }),
    [client, run],
  );
  const update = useCallback(
    (patch: PostInput) => run(() => client.posts.update(need(), patch.body === undefined ? patch : { ...patch, body: storedBody(patch.body) })),
    [client, run, need, storedBody],
  );
  const remove = useCallback(() => run(() => client.posts.delete(need())), [client, run, need]);
  const restore = useCallback(() => run(() => client.posts.restore(need())), [client, run, need]);
  const setCover = useCallback(
    (file: Blob | null) =>
      run(async () => {
        if (!file) return client.posts.setCover(need(), null);
        const up = await client.media.uploadNamed(client.media.postRef(need()), file);
        const cover = await client.posts.setCover(need(), up.name);
        const url = preview(up.name, file) ?? cover;
        if (url) shown.current.set(url, up.name);
        return url;
      }),
    [client, run, need, preview],
  );
  const uploadImage = useCallback(
    (file: Blob) =>
      run(async () => {
        const up = await client.media.uploadInline(need(), file);
        const url = preview(up.name, file) ?? up.url ?? up.ref;
        shown.current.set(url, up.name);
        return url;
      }),
    [client, run, need, preview],
  );
  return {
    post,
    loading: !!key && (r.loading || (!r.loaded && o.initial === undefined)),
    error: r.error,
    saving: inflight > 0,
    create,
    update,
    remove,
    restore,
    setCover,
    uploadImage,
    imageSrc,
    coverSrc: imageSrc(post?.cover),
    editorBody,
    storedBody,
  };
}
