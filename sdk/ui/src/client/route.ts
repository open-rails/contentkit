import { CONTENTKIT_ROUTES } from "./generated/routes.js";
import type { Http } from "./http.js";

type AnyRoute = (typeof CONTENTKIT_ROUTES)[number];
export type Method = AnyRoute["method"];
/** A route path the catalog declares for the method. */
export type RoutePath<M extends Method> = Extract<AnyRoute, { method: M }>["path"];
type RouteOf<M extends Method, P extends string> = Extract<AnyRoute, { method: M; path: P }>;

type ParamNames<P extends string> = P extends `${string}{${infer K}}${infer R}` ? (K extends `${infer N}...` ? N : K) | ParamNames<R> : never;
/** The path's {wildcards}, each a string value. */
export type PathParams<P extends string> = { [K in ParamNames<P>]: string };
/** The query parameters the route declares. */
export type RouteQuery<M extends Method, P extends string> = Partial<Record<RouteOf<M, P>["query"][number], QueryValue>>;
type QueryValue = string | number | boolean | null | undefined | readonly string[];

export interface CallOptions<M extends Method, P extends string> {
  params?: PathParams<P>;
  query?: RouteQuery<M, P>;
  body?: unknown;
  signal?: AbortSignal;
}

const index = new Map<string, AnyRoute>(CONTENTKIT_ROUTES.map((r) => [`${r.method} ${r.path}`, r]));

/** The URL of a catalog route: its module's mount, the path with params filled, then the query. */
export function routeURL<M extends Method, P extends RoutePath<M>>(http: Http, method: M, path: P, o: Pick<CallOptions<M, P>, "params" | "query"> = {}): string {
  return build(http, method, path, o.params as Record<string, string> | undefined, o.query as Record<string, QueryValue> | undefined);
}

/**
 * Calls a catalog route and resolves with its JSON body (undefined for 204);
 * the caller's declared type names it.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function call<M extends Method, P extends RoutePath<M>, T = any>(http: Http, method: M, path: P, o: CallOptions<M, P> = {}): Promise<T> {
  const url = build(http, method, path, o.params as Record<string, string> | undefined, o.query as Record<string, QueryValue> | undefined);
  return http.request<T>(url, { method, body: o.body, signal: o.signal });
}

function build(http: Http, method: string, path: string, params: Record<string, string> = {}, query: Record<string, QueryValue> = {}): string {
  const r = index.get(`${method} ${path}`);
  if (!r) throw new Error(`contentkit: no route ${method} ${path}`);
  const filled = path.replace(/\{(\w+)(\.\.\.)?\}/g, (_, name: string, rest?: string) => {
    const v = params[name];
    if (v === undefined || v === "") throw new Error(`contentkit: ${method} ${path} needs ${name}`);
    return rest ? v.split("/").map(encodeURIComponent).join("/") : encodeURIComponent(v);
  });
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) {
    if (Array.isArray(v)) for (const x of v) q.append(k, x);
    else if (v !== undefined && v !== null && v !== "") q.set(k, String(v));
  }
  const s = q.toString();
  return http.mount(r.module) + filled + (s ? `?${s}` : "");
}

/** A page of a limit/offset list. */
export interface PageQuery {
  limit?: number;
  offset?: number;
}
