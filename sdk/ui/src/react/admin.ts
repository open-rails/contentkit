import { useCallback, useMemo, useState, useSyncExternalStore } from "react";
import { createContentKitClient, type ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { TokenSource } from "../client/http.js";
import { useContentKitClient } from "./context.js";
import { keyOf, useContentScope, useResource } from "./use-resource.js";

/** What a flow needs of the host's auth: an authenticating fetch, as auth-ui's AuthClient and IssuerClient have. */
export interface ContentKitAuth {
  authFetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
  /** The access token, for HLS playlists on the API origin. */
  getAccessToken?: TokenSource;
}

/** How a flow reaches ContentKit: its own client, or baseUrl with the host's auth; else the provider's client. */
export interface FlowClientOptions {
  client?: ContentKitClient;
  baseUrl?: string;
  auth?: ContentKitAuth;
}

/** The flow's client: `client`, else one made from `baseUrl` and `auth`, else the provider's. */
export function useFlowClient({ client, baseUrl, auth }: FlowClientOptions): ContentKitClient {
  const made = useMemo(
    () =>
      client || baseUrl === undefined
        ? null
        : createContentKitClient({
            baseUrl,
            fetch: auth ? (input, init) => auth.authFetch(input, init) : undefined,
            token: auth?.getAccessToken ? () => auth.getAccessToken!() : undefined,
          }),
    [client, baseUrl, auth],
  );
  return useContentKitClient(client ?? made);
}

/** The host router as a flow sees it. With React Router: `{ pathname, search }` of useLocation() and useNavigate(). */
export interface AdminNavigation {
  pathname: string;
  search: string;
  navigate: (to: string, options?: { replace?: boolean }) => void;
}

/** A query for a view; empty values are left out. */
export type AdminQuery = Record<string, string | number | null | undefined>;

export interface AdminLocation {
  /** The view: the path under basePath, "" at its root. */
  view: string;
  query: URLSearchParams;
  go: (view: string, query?: AdminQuery, options?: { replace?: boolean }) => void;
}

const MOVED = "ckui:navigate";
const subscribeHistory = (cb: () => void) => {
  addEventListener("popstate", cb);
  addEventListener(MOVED, cb);
  return () => {
    removeEventListener("popstate", cb);
    removeEventListener(MOVED, cb);
  };
};
const historySnapshot = () => location.pathname + location.search;
const noHistory = () => "";

function encode(query: AdminQuery = {}): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) if (v !== undefined && v !== null && v !== "") q.set(k, String(v));
  const s = q.toString();
  return s ? `?${s}` : "";
}

/**
 * A multi-view flow's place: under basePath in the host router (navigation),
 * else in the browser history; without basePath, in memory.
 */
export function useAdminLocation(basePath: string | undefined, navigation: AdminNavigation | undefined): AdminLocation {
  const history = useSyncExternalStore(navigation || basePath === undefined ? subscribeNone : subscribeHistory, historySnapshot, noHistory);
  const [memory, setMemory] = useState({ view: "", search: "" });
  const base = basePath?.replace(/\/+$/, "") ?? "";
  let view: string;
  let search: string;
  if (basePath === undefined && !navigation) {
    ({ view, search } = memory);
  } else {
    const at = navigation ? navigation.pathname + navigation.search : history;
    const [pathname, query = ""] = splitOnce(at, "?");
    search = query ? `?${query}` : "";
    view = pathname === base || pathname === `${base}/` ? "" : pathname.startsWith(`${base}/`) ? pathname.slice(base.length + 1).replace(/\/+$/, "") : "";
  }
  const go = useCallback(
    (to: string, query?: AdminQuery, o: { replace?: boolean } = {}) => {
      const qs = encode(query);
      if (basePath === undefined && !navigation) return setMemory({ view: to, search: qs });
      const url = (to ? `${base}/${to}` : base || "/") + qs;
      if (navigation) return navigation.navigate(url, o);
      window.history[o.replace ? "replaceState" : "pushState"](null, "", url);
      dispatchEvent(new Event(MOVED));
    },
    [basePath, base, navigation],
  );
  const query = useMemo(() => new URLSearchParams(search), [search]);
  return { view, query, go };
}

const subscribeNone = () => () => {};

function splitOnce(s: string, sep: string): [string, string?] {
  const i = s.indexOf(sep);
  return i < 0 ? [s] : [s.slice(0, i), s.slice(i + 1)];
}

/** A ContentKit staff area, each behind its own permission. */
export type StaffArea = "comments" | "queue" | "bans" | "polls" | "contents" | "groups";

// No group has this id: staff get not_found, others a refusal.
const NO_GROUP = "00000000-0000-7000-8000-000000000000";

const PROBES: Record<StaffArea, (c: ContentKitClient, signal: AbortSignal) => Promise<unknown>> = {
  comments: (c, signal) => c.comments.adminList({ limit: 1 }, signal),
  queue: (c, signal) => c.moderation.held({ kind: "comment", limit: 1 }, signal),
  bans: (c, signal) => c.bans.list("global", { limit: 1 }, signal),
  polls: (c, signal) => c.polls.adminList({ limit: 1 }, signal),
  contents: (c, signal) => c.contents.manage({ limit: 1 }, signal),
  groups: (c, signal) =>
    c.groups.members(NO_GROUP, { limit: 1 }, signal).catch((e) => {
      if (toContentKitError(e).code !== "not_found") throw e;
    }),
};

export interface StaffAccess<A extends StaffArea> {
  loading: boolean;
  /** Per area; true until known, and when the check itself failed (the area then shows its own error). */
  allowed: Record<A, boolean>;
  /** Some area is allowed. */
  any: boolean;
  error?: ContentKitError;
}

/**
 * What the caller may do in each area: one small read per area, refused
 * (401, 403) or not, shared by every hook asking on the client.
 */
export function useStaffAccess<A extends StaffArea>(areas: readonly A[], o: FlowClientOptions = {}): StaffAccess<A> {
  const client = useFlowClient(o);
  const { store, scope } = useContentScope(client);
  const r = useResource<Record<string, boolean>>(store, keyOf("access", scope, ...areas), { type: "access" }, async (signal) => {
    const out: Record<string, boolean> = {};
    await Promise.all(
      areas.map(async (a) => {
        try {
          await PROBES[a](client, signal);
          out[a] = true;
        } catch (e) {
          const err = toContentKitError(e);
          if (err.code !== "forbidden" && err.code !== "unauthorized") throw err;
          out[a] = false;
        }
      }),
    );
    return out;
  });
  const allowed = Object.fromEntries(areas.map((a) => [a, r.data?.[a] ?? true])) as Record<A, boolean>;
  return { loading: r.loading || !r.loaded, allowed, any: areas.some((a) => allowed[a]), error: r.error };
}
