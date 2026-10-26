import { useMemo } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { Content, Decision, Version } from "../client/generated/wire.js";
import { parseVersionCode } from "../urls/index.js";
import { useVersionParam } from "./location.js";
import { keyOf, useContentScope, useResource } from "./use-resource.js";

/** A content's live versions, first first. */
export function liveVersions(c: Content | null | undefined): Version[] {
  return (c?.versions ?? []).filter((v) => v.live).sort((a, b) => a.position - b.position);
}

/**
 * The version a reader of language opens by default: the first live one by
 * position serving it, else the first live one. The server picks cards'
 * versions, covers and meta by the same rule. An explicit single-version
 * response preserves the server's selection, including an authorized draft.
 */
export function defaultVersion(c: Content | null | undefined, language?: string): Version | null {
  if (c?.versions.length === 1 && c.versions[0]!.id === c.selected_version_id) return c.versions[0]!;
  return editionOf(liveVersions(c), language);
}

/** defaultVersion among versions: the first by position serving language, else the first. */
export function editionOf(versions: readonly Version[], language?: string): Version | null {
  const sorted = [...versions].sort((a, b) => a.position - b.position);
  return (language ? sorted.find((v) => v.languages.includes(language)) : undefined) ?? sorted[0] ?? null;
}

/** The version selected names: its code (any spelling, the page's ?v=) or its id; undefined when none. */
export function findVersion(versions: readonly Version[], selected: string | null | undefined): Version | undefined {
  if (!selected) return undefined;
  const code = parseVersionCode(selected);
  return versions.find((v) => v.id === selected) ?? (code ? versions.find((v) => v.code === code) : undefined);
}

export interface UseContentDetail {
  content: Content | null;
  /** Live versions, first first. */
  versions: Version[];
  /** The version shown: the one asked for (?v=) when live, else the language's default. */
  version: Version | null;
  /** The shown version's access decision: allowed, what is sold, previews. */
  decision: Decision | null;
  /** Every language a live version serves, in version order. */
  languages: string[];
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
}

export interface ContentDetailOptions {
  language?: string;
  /** The version to show, by code or id; default the page's ?v=. Unknown or not live: the language's default. */
  version?: string | null;
  /** The page's location for its ?v=; default window.location. */
  location?: string;
  client?: ContentKitClient;
}

/**
 * A content page's data: text in the reader's language, live versions with
 * their segments and access decisions, groups, attributes and the version to show.
 */
export function useContentDetail(id: string | null | undefined, o: ContentDetailOptions = {}): UseContentDetail {
  const { client, store, scope, language: provided } = useContentScope(o.client);
  const language = o.language ?? provided;
  const [param] = useVersionParam({ location: o.location });
  const selected = o.version === undefined ? param : o.version;
  const key = id ? keyOf("content-detail", scope, id, language) : null;
  const r = useResource<Content>(store, key, { type: "content-detail", id: id ?? undefined }, (s) => client.contents.get(id!, language, s));
  const parent = r.data ?? null;
  const versions = useMemo(() => liveVersions(parent), [parent]);
  const picked = findVersion(versions, selected) ?? defaultVersion(parent, language);
  const exact = !!picked && picked.id !== parent?.selected_version_id;
  const editionKey = exact ? keyOf("version-detail", scope, picked.id, language) : null;
  const edition = useResource<Content>(store, editionKey, { type: "content-detail", id: id ?? undefined },
    (signal) => client.contents.getByVersion(picked!.id, language, signal));
  // The parent supplies the visible edition index. Text and image URLs must
  // come from the exact selected edition, never a sibling's projection.
  const content = exact ? edition.data ?? null : parent;
  return useMemo(() => {
    const version = exact ? content?.versions.find((v) => v.id === picked?.id) ?? null : picked;
    return {
      content,
      versions,
      version,
      decision: version?.decision ?? content?.decision ?? null,
      languages: [...new Set(versions.flatMap((v) => v.languages))],
      loading: !!key && (r.loading || !r.loaded || exact && (edition.loading || !edition.loaded)),
      error: r.error ?? (exact ? edition.error : undefined),
      reload: () => { r.reload(); if (exact) edition.reload(); },
    };
  }, [content, versions, picked, exact, language, key, r.loading, r.loaded, r.error, r.reload, edition.loading, edition.loaded, edition.error, edition.reload]);
}
