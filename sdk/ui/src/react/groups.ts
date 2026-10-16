import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type {
  ContentMemberships,
  ContentModel,
  Edge,
  EdgeInput,
  Group,
  GroupCard,
  GroupDetail,
  GroupInput,
  GroupUpdate,
  MemberRef,
  Membership,
  MergeReport,
  MyGroup,
  Name,
  Edit,
} from "../client/generated/wire.js";
import type { EntityImageSlot, MyEntityQuery } from "../client/entities.js";
import { useGuarded } from "./context.js";
import { keyOf, useContentScope, useList, useResource, type UseList } from "./use-resource.js";

export interface UseKinds {
  /** null until it loads (or when it fails). */
  kinds: ContentModel | null;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
}

/** The site's content model (GET /v1/kinds): languages, tiers, kinds, group kinds and content filters; one read per client. */
export function useKinds(o: { client?: ContentKitClient } = {}): UseKinds {
  const { client, store } = useContentScope(o.client);
  const r = useResource<ContentModel>(store, keyOf("kinds"), { type: "kinds" }, (s) => client.kinds(s));
  return { kinds: r.data ?? null, loading: !r.data && (r.loading || !r.loaded), error: r.error, reload: r.reload };
}

export type GroupSort = "name" | "count" | "newest";

export interface GroupFilter {
  kind?: string;
  /** Children of this group. */
  parent?: string;
  /** Groups scoped by this group (characters of a series). */
  scope?: string;
  slug?: string;
  /** Sort names starting with this letter (a-z), or with anything else ("#"). */
  letter?: string;
  /** Names, counts and paths in this language; default the provider's. */
  language?: string;
  /** Count contents of this kind only. */
  contentKind?: string;
  /** Default "name". */
  sort?: GroupSort;
  /** Default 50, at most 200. */
  pageSize?: number;
}

/** Active groups with name, path, cover and count, a cursor page at a time. */
export function useGroups(f: GroupFilter = {}, o: { client?: ContentKitClient } = {}): UseList<GroupCard> {
  const { client, store, scope, language: provided } = useContentScope(o.client);
  const language = f.language ?? provided;
  const size = f.pageSize ?? 50;
  return useList(
    store,
    keyOf("groups", scope, f.kind, f.parent, f.scope, f.slug, f.letter, language, f.contentKind, f.sort, size),
    { type: "groups" },
    async (cursor, signal) => {
      const page = await client.entities.list(
        { kind: f.kind, parent: f.parent, scope: f.scope, slug: f.slug, letter: f.letter, language, content_kind: f.contentKind, sort: f.sort, cursor: cursor as string | undefined, limit: size },
        signal,
      );
      return { items: page.entities, next: page.next_cursor ?? null };
    },
  );
}

export interface UseGroup {
  /** The group as its page shows it; null before create and while loading. A merged group reads as its survivor. */
  group: GroupDetail | null;
  /** What this hook deleted, until restored: reads no longer answer for it. */
  deleted: Group | null;
  loading: boolean;
  error?: ContentKitError;
  /** Writes in flight. */
  saving: boolean;
  reload: () => void;
  /** Creates the group; the hook then shows it. */
  create: (input: GroupInput) => Promise<Group>;
  update: (patch: GroupUpdate) => Promise<Group>;
  remove: () => Promise<Group>;
  restore: () => Promise<Group>;
  /** Replaces ("set"), adds or removes names and aliases; a new canonical name demotes the old one to an alias. */
  names: (op: "set" | "add" | "remove", names: Name[]) => Promise<Name[]>;
  edges: (op: "add" | "remove", edges: EdgeInput[]) => Promise<Edge[]>;
  /** Merges the group into another of its kind; the hook then shows the survivor. */
  merge: (into: string) => Promise<MergeReport>;
  /** Uploads an image (cropped by edit) and selects it as the cover or avatar. */
  uploadImage: (slot: EntityImageSlot, file: Blob & { name?: string }, o?: { wait?: boolean; edit?: Edit }) => Promise<Group>;
  /** Shows the images uploaded here until their renditions exist. */
  previews: Partial<Record<EntityImageSlot, string>>;
}

/** One group: its page for anyone; create, edit, names, edges, cover, merge, delete and restore for staff (root:groups:write). null creates. */
export function useGroup(id: string | null | undefined, o: { language?: string; initial?: GroupDetail; client?: ContentKitClient } = {}): UseGroup {
  const { client, store, scope, language: provided } = useContentScope(o.client);
  const language = o.language ?? provided;
  const [created, setCreated] = useState<string | null>(null);
  const [merged, setMerged] = useState<string | null>(null);
  const [deleted, setDeleted] = useState<Group | null>(null);
  const gid = merged ?? id ?? created;
  useEffect(() => {
    setMerged(null);
    setDeleted(null);
  }, [id]);
  const key = gid && !deleted ? keyOf("group", scope, gid, language) : null;
  const r = useResource<GroupDetail>(store, key, { type: "group", id: gid ?? undefined }, (s) => client.entities.get(gid!, language, s), { initial: o.initial });
  const guarded = useGuarded();
  const [inflight, setInflight] = useState(0);
  const run = useCallback(
    async <T>(fn: () => Promise<T>): Promise<T> => {
      setInflight((n) => n + 1);
      try {
        return await guarded(fn);
      } finally {
        setInflight((n) => n - 1);
      }
    },
    [guarded],
  );
  const need = useCallback(() => {
    if (!gid) throw new Error("contentkit-ui: create the group first");
    return gid;
  }, [gid]);

  const [previews, setPreviews] = useState<Partial<Record<EntityImageSlot, string>>>({});
  const blobs = useRef<string[]>([]);
  useEffect(() => {
    const urls = blobs.current;
    return () => urls.forEach((u) => URL.revokeObjectURL(u));
  }, []);

  const group = r.data ?? null;
  return {
    group: deleted ? null : group,
    deleted,
    loading: !!key && (r.loading || (!r.loaded && o.initial === undefined)),
    error: deleted ? undefined : r.error,
    saving: inflight > 0,
    reload: r.reload,
    create: useCallback(
      (input: GroupInput) =>
        run(async () => {
          const [g] = await client.entities.create([input]);
          setCreated(g!.id);
          return g!;
        }),
      [client, run],
    ),
    update: useCallback((patch: GroupUpdate) => run(() => client.entities.update(need(), patch)), [client, run, need]),
    remove: useCallback(
      () =>
        run(async () => {
          const g = await client.entities.remove(need());
          setDeleted(g);
          return g;
        }),
      [client, run, need],
    ),
    restore: useCallback(
      () =>
        run(async () => {
          const g = await client.entities.update(need(), { state: "active" });
          setDeleted(null);
          return g;
        }),
      [client, run, need],
    ),
    names: useCallback((op: "set" | "add" | "remove", names: Name[]) => run(() => client.entities.names(need(), op, names)), [client, run, need]),
    edges: useCallback((op: "add" | "remove", edges: EdgeInput[]) => run(() => client.entities.edges(need(), op, edges)), [client, run, need]),
    merge: useCallback(
      (into: string) =>
        run(async () => {
          const report = await client.entities.merge(need(), into);
          setMerged(report.into);
          return report;
        }),
      [client, run, need],
    ),
    uploadImage: useCallback(
      (slot: EntityImageSlot, file: Blob & { name?: string }, u: { wait?: boolean; edit?: Edit } = {}) =>
        run(async () => {
          const kind = group?.kind;
          if (!kind) throw new Error("contentkit-ui: load the group first");
          const g = await client.entities.uploadImage({ kind, id: need() }, slot, file, u);
          // A cropped upload's renditions show the crop; until then the file shows uncropped.
          if (typeof URL.createObjectURL === "function" && !u.edit) {
            const url = URL.createObjectURL(file);
            blobs.current.push(url);
            setPreviews((p) => ({ ...p, [slot]: url }));
          }
          return g;
        }),
      [client, run, need, group?.kind],
    ),
    previews: { cover: group?.cover?.image ? previews.cover : undefined, avatar: group?.avatar?.image ? previews.avatar : undefined },
  };
}

/** Groups the signed-in person owns, edits or belongs to, a cursor page at a time; nothing signed out. */
export function useMyGroups(q: Omit<MyEntityQuery, "cursor" | "limit"> & { pageSize?: number } = {}, o: { client?: ContentKitClient } = {}): UseList<MyGroup> {
  const { client, store, scope, viewer, language } = useContentScope(o.client);
  const { pageSize, ...rest } = q;
  const query = { language, ...rest };
  return useList(store, viewer === null ? null : keyOf("groups-mine", scope, query, pageSize), { type: "groups-mine" }, async (cursor, signal) => {
    const page = await client.entities.mine({ ...query, cursor: cursor as string | undefined, limit: pageSize }, signal);
    return { items: page.entities, next: page.next_cursor ?? null };
  });
}

/** A membership with a stable id: (group, content, version, role) is its identity. */
export type Member = Membership & { id: string };

export const memberId = (m: Membership): string => [m.group_id ?? "", m.content_id ?? "", m.version_id ?? "", m.role ?? ""].join("|");

export interface UseGroupMembers extends UseList<Member> {
  saving: boolean;
  /** Adds members, or updates their role's position, window and state. */
  add: (members: Membership[]) => Promise<number>;
  remove: (members: Membership[]) => Promise<number>;
  /** Orders an ordered group: the listed members first, in list order. */
  reorder: (members: MemberRef[]) => Promise<number>;
}

/** Staff: a group's memberships in member order, with writes. */
export function useGroupMembers(id: string | null | undefined, o: { pageSize?: number; client?: ContentKitClient } = {}): UseGroupMembers {
  const { client, store, scope } = useContentScope(o.client);
  const size = o.pageSize ?? 50;
  const list = useList<Member>(store, id ? keyOf("group-members", scope, id, size) : null, { type: "group-members", id: id ?? undefined }, async (cursor, signal) => {
    const page = await client.entities.members(id!, { cursor: cursor as string | undefined, limit: size }, signal);
    return { items: page.members.map((m) => ({ ...m, id: memberId(m) })), next: page.next_cursor ?? null };
  });
  const guarded = useGuarded();
  const [inflight, setInflight] = useState(0);
  const write = useCallback(
    async (fn: (gid: string) => Promise<{ count: number }>) => {
      if (!id) throw new Error("contentkit-ui: no group");
      setInflight((n) => n + 1);
      try {
        return (await guarded(() => fn(id))).count;
      } finally {
        setInflight((n) => n - 1);
      }
    },
    [id, guarded],
  );
  return {
    ...list,
    saving: inflight > 0,
    add: useCallback((ms: Membership[]) => write((g) => client.entities.addMembers(g, ms)), [client, write]),
    remove: useCallback((ms: Membership[]) => write((g) => client.entities.removeMembers(g, ms)), [client, write]),
    reorder: useCallback((ms: MemberRef[]) => write((g) => client.entities.reorder(g, ms)), [client, write]),
  };
}

export interface UseContentGroups {
  memberships: Membership[];
  /** The groups the memberships name. */
  entities: ReadonlyMap<string, GroupCard>;
  loading: boolean;
  error?: ContentKitError;
  saving: boolean;
  reload: () => void;
  /** Replaces the content's memberships, its versions' included, in these group kinds (every kind its kind joins when omitted). */
  set: (memberships: Membership[], kinds?: string[]) => Promise<ContentMemberships>;
}

/** Staff: one content's memberships, its versions' included, and their replacement. */
const NO_MEMBERSHIPS: Membership[] = [];

export function useContentGroups(contentId: string | null | undefined, o: { language?: string; client?: ContentKitClient } = {}): UseContentGroups {
  const { client, store, scope, language: provided } = useContentScope(o.client);
  const language = o.language ?? provided;
  const key = contentId ? keyOf("group-memberships", scope, contentId, language) : null;
  const r = useResource<ContentMemberships>(store, key, { type: "group-memberships", id: contentId ?? undefined }, (s) => client.entities.ofContent(contentId!, language, s));
  const guarded = useGuarded();
  const [inflight, setInflight] = useState(0);
  const set = useCallback(
    async (memberships: Membership[], kinds?: string[]) => {
      if (!contentId) throw new Error("contentkit-ui: no content");
      setInflight((n) => n + 1);
      try {
        const out = await guarded(() => client.entities.setOfContent(contentId, { kinds, memberships }, language));
        if (key) store.set(key, out);
        return out;
      } finally {
        setInflight((n) => n - 1);
      }
    },
    [client, contentId, language, guarded, store, key],
  );
  const entities = useMemo(() => new Map((r.data?.entities ?? []).map((g) => [g.id, g])), [r.data]);
  // Stable while loading or failed: effects keyed on it must not see a new list every render.
  const memberships = useMemo(() => r.data?.memberships ?? NO_MEMBERSHIPS, [r.data]);
  return {
    memberships,
    entities,
    loading: !!key && (r.loading || !r.loaded),
    error: r.error,
    saving: inflight > 0,
    reload: r.reload,
    set,
  };
}
