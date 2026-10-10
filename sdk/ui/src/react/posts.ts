import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { Sort } from "../client/content/types.js";
import type { Post, PostInput } from "../client/generated/wire.js";
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
  /** Updates the given fields; a body from the editor is stored with its images' public URLs (storedBody). */
  update: (patch: PostInput) => Promise<Post>;
  remove: () => Promise<void>;
  /** Restores the deleted post (PostWrite). */
  restore: () => Promise<Post>;
  /** Uploads the cover (null clears it); resolves with a URL that shows it now. */
  setCover: (file: Blob | null) => Promise<string | null>;
  /** Uploads an image for the body; resolves with a URL that shows it now, to place in the editor. */
  uploadImage: (file: Blob) => Promise<string>;
  /**
   * A URL that shows one of the post's images now (the cover, a body image):
   * an unpublished post's images are not public yet, so its editors see them
   * through signed URLs of an editor read; a fresh upload shows from the
   * file itself. Any other URL is returned as is.
   */
  imageSrc: (url: string | null | undefined) => string | undefined;
  /** post.body with its images through imageSrc: what a rich-text editor shows. */
  editorBody: string | undefined;
  /** HTML from the editor with the URLs imageSrc gave back to the images' public URLs. */
  storedBody: (html: string) => string;
}

const NAME = "i-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}";
const nameIn = (url: string) => url.match(new RegExp(NAME))?.[0];
// An image URL in HTML: everything around a name up to a quote, a space or a bracket.
const imageURLs = new RegExp(`[^\\s"'()<>]*${NAME}[^\\s"'()<>]*`, "g");

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

  // Images: fresh uploads show from their file; an unpublished post's from an editor read.
  const [previews, setPreviews] = useState<ReadonlyMap<string, string>>(new Map());
  const given = useRef(new Map<string, string>()); // each URL imageSrc gave -> the image's public URL
  useEffect(() => {
    const urls = given.current;
    return () => {
      for (const u of urls.keys()) if (u.startsWith("blob:")) URL.revokeObjectURL(u);
    };
  }, []);
  const names = useMemo(() => {
    const out = new Map<string, string>();
    for (const u of [...(post?.body.match(imageURLs) ?? []), post?.cover_url ?? ""]) {
      const n = nameIn(u);
      if (n && !out.has(n)) out.set(n, u);
    }
    return out;
  }, [post?.body, post?.cover_url]);
  const unread = !!post && !isPublished(post) && [...names.keys()].some((n) => !previews.has(n));
  const folder = useMemo(() => client.media.postRef(pid ?? "none"), [client, pid]);
  const read = useMediaRead(folder, { editor: true, read: unread ? undefined : null, client });
  const views = useMemo(() => {
    const out = new Map<string, string>();
    for (const f of read.read?.files ?? []) {
      const n = f.upload && f.editor_url ? nameIn(f.path) : undefined;
      if (n) out.set(n, f.editor_url!);
    }
    return out;
  }, [read.read]);
  // Editor views render on the first editor read that misses them: read again, backing off.
  const missing = unread && !!read.read && [...names.keys()].some((n) => !previews.has(n) && !views.has(n) && !read.read!.files.some((f) => f.failed && nameIn(f.path) === n));
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

  const imageSrc = useCallback(
    (url: string | null | undefined) => {
      if (!url) return undefined;
      const n = nameIn(url);
      const shown = n ? (previews.get(n) ?? views.get(n)) : undefined;
      if (!shown) return url;
      given.current.set(shown, url);
      return shown;
    },
    [previews, views],
  );
  const editorBody = useMemo(() => post?.body.replace(imageURLs, (u) => imageSrc(u) ?? u), [post?.body, imageSrc]);
  const storedBody = useCallback((html: string) => {
    let out = html;
    for (const [shown, url] of given.current) out = out.split(shown).join(url).split(shown.replaceAll("&", "&amp;")).join(url);
    return out;
  }, []);
  const preview = useCallback((name: string, url: string, file: Blob) => {
    if (typeof URL.createObjectURL !== "function") return url;
    const shown = URL.createObjectURL(file);
    given.current.set(shown, url);
    setPreviews((m) => new Map(m).set(name, shown));
    return shown;
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
        const url = await client.posts.setCover(need(), up.name);
        return url && preview(up.name, url, file);
      }),
    [client, run, need, preview],
  );
  const uploadImage = useCallback(
    (file: Blob) =>
      run(async () => {
        const up = await client.media.uploadInline(need(), file);
        return preview(up.name, up.url, file);
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
    editorBody,
    storedBody,
  };
}
