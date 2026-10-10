import { useCallback, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { Sort } from "../client/content/types.js";
import type { Post, PostInput } from "../client/generated/wire.js";
import { keyOf, offsetPages, useContentScope, useList, useResource, type UseList } from "./use-resource.js";

export interface PostFilter {
  language?: string;
  sort?: Sort;
  /** Staff: every post, drafts and scheduled ones included (PostWrite). */
  admin?: boolean;
  /** With admin: true only drafts, false no drafts. */
  draft?: boolean;
  pageSize?: number;
}

/** Posts, a page at a time: the published ones, or every one for staff. */
export function usePosts(f: PostFilter = {}, o: { client?: ContentKitClient } = {}): UseList<Post> {
  const { client, store, viewer } = useContentScope(o.client);
  const size = f.pageSize ?? 20;
  return useList(
    store,
    keyOf("posts", viewer, !!f.admin, f.language, f.sort, f.draft, size),
    { type: "posts" },
    offsetPages(size, (q, signal) =>
      f.admin ? client.posts.adminList({ ...q, language: f.language, draft: f.draft }, signal) : client.posts.list({ ...q, language: f.language, sort: f.sort }, signal),
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
  update: (patch: PostInput) => Promise<Post>;
  remove: () => Promise<void>;
  /** Uploads the cover (null clears it); resolves with its URL. */
  setCover: (file: Blob | null) => Promise<string | null>;
  /** Uploads an image for the body; resolves with the URL to place in it. */
  uploadImage: (file: Blob) => Promise<string>;
}

/** One post: read, and for staff (PostWrite) create, update, delete, cover and body images. null creates a new post. */
export function usePost(id: string | null | undefined, o: { initial?: Post; client?: ContentKitClient } = {}): UsePost {
  const { client, store, viewer } = useContentScope(o.client);
  const [created, setCreated] = useState<string | null>(null);
  const pid = id ?? created;
  const key = pid ? keyOf("post", viewer, pid) : null;
  const r = useResource<Post | null>(store, key, { type: "post", id: pid ?? undefined }, (s) => client.posts.get(pid!, s), { initial: o.initial });
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
  const create = useCallback(
    (input: PostInput) =>
      run(async () => {
        const post = await client.posts.create(input);
        setCreated(post.id);
        return post;
      }),
    [client, run],
  );
  const update = useCallback((patch: PostInput) => run(() => client.posts.update(need(), patch)), [client, run, need]);
  const remove = useCallback(() => run(() => client.posts.delete(need())), [client, run, need]);
  const setCover = useCallback((file: Blob | null) => run(() => client.posts.uploadCover(need(), file)), [client, run, need]);
  const uploadImage = useCallback((file: Blob) => run(async () => (await client.media.uploadInline(need(), file)).url), [client, run, need]);
  return {
    post: r.data ?? null,
    loading: !!key && (r.loading || (!r.loaded && o.initial === undefined)),
    error: r.error,
    saving: inflight > 0,
    create,
    update,
    remove,
    setCover,
    uploadImage,
  };
}
