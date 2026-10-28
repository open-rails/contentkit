import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type {
  ContentMemberships,
  ContentModel,
  Entity,
  EntityCard,
  EntityDetail,
  EntityInput,
  EntityPage,
  EntityUpdate,
  MemberRef,
  Membership,
  MergeReport,
  Name,
  NewEntity,
  Edit,
} from "../client/generated/wire.js";
import type { EntityImageSlot } from "../client/entities.js";
import { useGuarded } from "./context.js";
import { keyOf, useContentScope, useList, useResource, type UseList } from "./use-resource.js";

export interface UseKinds {
  /** null until it loads (or when it fails). */
  kinds: ContentModel | null;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
}

/** The site's content model (GET /v1/kinds): languages, tiers, kinds and entity kinds; one read per client. */
export function useKinds(o: { client?: ContentKitClient } = {}): UseKinds {
  const { client, store } = useContentScope(o.client);
  const r = useResource<ContentModel>(store, keyOf("kinds"), { type: "kinds" }, (s) => client.kinds(s));
  return { kinds: r.data ?? null, loading: !r.data && (r.loading || !r.loaded), error: r.error, reload: r.reload };
}

export type EntitySort = "name" | "count" | "newest";

export interface EntityFilter {
  kind?: string;
  /** Children of this entity. */
  parent?: string;
  /** Entities scoped by this entity (characters of a series). */
  scope?: string;
  slug?: string;
  /** Sort names starting with this letter (a-z), or with anything else ("#"). */
  letter?: string;
  /** Names, counts and paths in this language; default the provider's. */
  language?: string;
  /** Count contents of this kind only. */
  contentKind?: string;
  /** Default "name". */
  sort?: EntitySort;
  /** Default 50, at most 200. */
  pageSize?: number;
  /** Management: a canonical name, alias or slug containing this. */
  query?: string;
  /** Management: the deleted, restorable entities instead of active ones. */
  deleted?: boolean;
}

/** One numbered page of entities and every matching entity's total. */
export interface UseEntitiesPage {
  items: EntityCard[];
  total?: number;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
}

const NO_ENTITIES: EntityCard[] = [];

/** Page `page` (from 1) of active entities with the total. */
export function useEntitiesPage(f: EntityFilter, page: number, o: { client?: ContentKitClient } = {}): UseEntitiesPage {
  const { client, store, scope, language: provided } = useContentScope(o.client);
  const language = f.language ?? provided;
  const size = f.pageSize ?? 50;
  const n = Math.max(1, Math.floor(page));
  const r = useResource<EntityPage>(store, keyOf("entities", scope, "page", f.kind, f.parent, f.scope, f.slug, f.letter, language, f.contentKind, f.sort, size, n), { type: "entities" }, (signal) =>
    client.entities.list({ kind: f.kind, parent: f.parent, scope: f.scope, slug: f.slug, letter: f.letter, language, content_kind: f.contentKind, sort: f.sort, page: n, limit: size }, signal),
  );
  return { items: r.data?.entities ?? NO_ENTITIES, total: r.data?.total, loading: r.loading || !r.loaded, error: r.error, reload: r.reload };
}

/** Active entities with name, path, cover and count, a cursor page at a time. */
export function useEntities(f: EntityFilter = {}, o: { client?: ContentKitClient; management?: boolean } = {}): UseList<EntityCard> {
  const { client, store, scope, language: provided } = useContentScope(o.client);
  const language = f.language ?? provided;
  const size = f.pageSize ?? 50;
  return useList(
    store,
    keyOf("entities", scope, o.management, f.kind, f.parent, f.scope, f.slug, f.letter, language, f.contentKind, f.sort, size, f.query, f.deleted),
    { type: "entities" },
    async (cursor, signal) => {
      const query = { kind: f.kind, parent: f.parent, scope: f.scope, slug: f.slug, letter: f.letter, language, content_kind: f.contentKind, sort: f.sort, cursor: cursor as string | undefined, limit: size };
      const page = await (o.management
        ? client.entities.manage({ ...query, q: f.query?.trim() || undefined, state: f.deleted ? "deleted" : undefined }, signal)
        : client.entities.list(query, signal));
      return { items: page.entities, next: page.next_cursor ?? null };
    },
  );
}

export interface UseEntity {
  /** The entity as its page shows it; null before create and while loading. A merged entity reads as its survivor. */
  entity: EntityDetail | null;
  /** What this hook deleted, until restored: reads no longer answer for it. */
  deleted: Entity | null;
  loading: boolean;
  error?: ContentKitError;
  /** Writes in flight. */
  saving: boolean;
  reload: () => void;
  /** Creates the entity; the hook then shows it. */
  create: (input: EntityInput) => Promise<Entity>;
  update: (patch: EntityUpdate) => Promise<Entity>;
  remove: () => Promise<Entity>;
  restore: () => Promise<Entity>;
  /** Replaces ("set"), adds or removes names and aliases; a new canonical name demotes the old one to an alias. */
  names: (op: "set" | "add" | "remove", names: Name[]) => Promise<Name[]>;
  /** Merges the entity into another of its kind; the hook then shows the survivor. */
  merge: (into: string) => Promise<MergeReport>;
  /** Uploads an image (cropped by edit) and selects it as the cover or avatar. */
  uploadImage: (slot: EntityImageSlot, file: Blob & { name?: string }, o?: { wait?: boolean; edit?: Edit }) => Promise<Entity>;
  /** Shows the images uploaded here until their renditions exist. */
  previews: Partial<Record<EntityImageSlot, string>>;
}

/** One entity: its page for anyone; create, edit, names, cover, merge, delete and restore for staff (app:entities:write). null creates. */
export function useEntity(id: string | null | undefined, o: { language?: string; initial?: EntityDetail; client?: ContentKitClient; management?: boolean } = {}): UseEntity {
  const { client, store, scope, language: provided } = useContentScope(o.client);
  const language = o.language ?? provided;
  const [created, setCreated] = useState<string | null>(null);
  const [merged, setMerged] = useState<string | null>(null);
  const [deleted, setDeleted] = useState<Entity | null>(null);
  const gid = merged ?? id ?? created;
  useEffect(() => {
    setMerged(null);
    setDeleted(null);
  }, [id]);
  const key = gid && !deleted ? keyOf("entity", scope, o.management, gid, language) : null;
  const r = useResource<EntityDetail>(store, key, { type: "entity", id: gid ?? undefined }, (s) => o.management ? client.entities.getForManagement(gid!, language, s) : client.entities.get(gid!, language, s), { initial: o.initial });
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

  const entity = r.data ?? null;
  return {
    entity: deleted ? null : entity,
    deleted,
    loading: !!key && (r.loading || (!r.loaded && o.initial === undefined)),
    error: deleted ? undefined : r.error,
    saving: inflight > 0,
    reload: r.reload,
    create: useCallback(
      (input: EntityInput) =>
        run(async () => {
          const [g] = await client.entities.create([input]);
          setCreated(g!.id);
          return g!;
        }),
      [client, run],
    ),
    update: useCallback((patch: EntityUpdate) => run(() => client.entities.update(need(), patch)), [client, run, need]),
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
          const kind = entity?.kind;
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
      [client, run, need, entity?.kind],
    ),
    previews: { cover: entity?.cover?.image ? previews.cover : undefined, avatar: entity?.avatar?.image ? previews.avatar : undefined },
  };
}

/** A membership with a stable id: (entity, content, version, role) is its identity. */
export type Member = Membership & { id: string };

export const memberId = (m: Membership): string => [m.group_id ?? "", m.content_id ?? "", m.version_id ?? "", m.role ?? ""].join("|");

export interface UseEntityMembers extends UseList<Member> {
  saving: boolean;
  /** Adds members, or updates their role's position, window and state. */
  add: (members: Membership[]) => Promise<number>;
  remove: (members: Membership[]) => Promise<number>;
  /** Orders an ordered entity: the listed members first, in list order. */
  reorder: (members: MemberRef[]) => Promise<number>;
}

/** Staff: a entity's memberships in member order, with writes. */
export function useEntityMembers(id: string | null | undefined, o: { pageSize?: number; client?: ContentKitClient } = {}): UseEntityMembers {
  const { client, store, scope } = useContentScope(o.client);
  const size = o.pageSize ?? 50;
  const list = useList<Member>(store, id ? keyOf("entity-members", scope, id, size) : null, { type: "entity-members", id: id ?? undefined }, async (cursor, signal) => {
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

export interface UseContentEntities {
  memberships: Membership[];
  /** The entities the memberships name. */
  entities: ReadonlyMap<string, EntityCard>;
  loading: boolean;
  error?: ContentKitError;
  saving: boolean;
  reload: () => void;
  /** Replaces the content's memberships, its versions' included, in these entity kinds (every kind its kind joins when omitted); create links new names, creating entities of creatable kinds. */
  set: (memberships: Membership[], kinds?: string[], create?: NewEntity[]) => Promise<ContentMemberships>;
}

/** Staff: one content's memberships, its versions' included, and their replacement. */
const NO_MEMBERSHIPS: Membership[] = [];

export function useContentEntities(contentId: string | null | undefined, o: { language?: string; client?: ContentKitClient } = {}): UseContentEntities {
  const { client, store, scope, language: provided } = useContentScope(o.client);
  const language = o.language ?? provided;
  const key = contentId ? keyOf("entity-memberships", scope, contentId, language) : null;
  const r = useResource<ContentMemberships>(store, key, { type: "entity-memberships", id: contentId ?? undefined }, (s) => client.entities.ofContent(contentId!, language, s));
  const guarded = useGuarded();
  const [inflight, setInflight] = useState(0);
  const set = useCallback(
    async (memberships: Membership[], kinds?: string[], create?: NewEntity[]) => {
      if (!contentId) throw new Error("contentkit-ui: no content");
      setInflight((n) => n + 1);
      try {
        const out = await guarded(() => client.entities.setOfContent(contentId, { kinds, memberships, ...(create?.length ? { create } : {}) }, language));
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
