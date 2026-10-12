import type { Http } from "./http.js";

// The site's content model as GET /v1/kinds serves it. Hand-written until the
// route is in the catalog; then these are the generated wire types.

/** A declared attribute of a kind or group kind. */
export interface AttributeSpec {
  type: "string" | "int" | "bool" | "enum" | "time";
  values?: string[];
  filterable?: boolean;
  sortable?: boolean;
}

/** A content kind: a gallery, a movie, a creator post, a blog post. */
export interface KindSpec {
  /** The URL segment: /{lang}/{route}/{CODE}/{slug}. */
  route: string;
  presentation: "reader" | "player" | "post";
  versions: { languages: "one" | "many"; traits?: string[]; hidden?: boolean };
  segments?: boolean;
  /** The group kinds its content joins. */
  groups?: string[];
  attributes?: Record<string, AttributeSpec>;
  sort?: string;
}

/** A kind of grouping: tag, artist, series, channel, rail. */
export interface GroupKindSpec {
  route?: string;
  /** Members carry positions. */
  ordered?: boolean;
  /** The group kind a group may sit under. */
  parent?: string;
  /** Slugs are unique within a group of this kind (a character within a series). */
  scoped_by?: string;
  owner?: "site" | "user";
  sellable?: boolean;
  auth_scope?: boolean;
  /** Memberships carry active_from/active_until. */
  windows?: boolean;
  /** Memberships may target one version. */
  version_level?: boolean;
  roles?: string[];
  attributes?: Record<string, AttributeSpec>;
}

/** A group viewers may hide, and whether it shows before their own choice. */
export interface ContentFilterSpec {
  group: { kind: string; slug: string };
  default_visible: boolean;
  labels?: Record<string, string>;
}

export interface Kinds {
  languages: string[];
  tiers?: string[];
  kinds: Record<string, KindSpec>;
  group_kinds?: Record<string, GroupKindSpec>;
  content_filters?: ContentFilterSpec[];
}

/** GET /v1/kinds. */
export function getKinds(http: Http, signal?: AbortSignal): Promise<Kinds> {
  return http.request<Kinds>(`${http.mount("groups")}/kinds`, { signal });
}
