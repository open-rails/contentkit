import { useMemo } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitError } from "../client/errors.js";
import type { Content, Decision, Version } from "../client/generated/wire.js";
import { keyOf, useContentScope, useResource } from "./use-resource.js";

/** A content's live versions, first first. */
export function liveVersions(c: Content | null | undefined): Version[] {
  return (c?.versions ?? []).filter((v) => v.live).sort((a, b) => a.position - b.position);
}

/** The version a reader of language opens by default: the first live one serving it, else the first live one. */
export function defaultVersion(c: Content | null | undefined, language?: string): Version | null {
  const live = liveVersions(c);
  return (language ? live.find((v) => v.languages.includes(language)) : undefined) ?? live[0] ?? null;
}

export interface UseContentDetail {
  content: Content | null;
  /** Live versions, first first. */
  versions: Version[];
  /** The version shown: the one asked for when live, else the language's default. */
  version: Version | null;
  /** The shown version's access decision: allowed, what is sold, previews. */
  decision: Decision | null;
  /** Every language a live version serves, in version order. */
  languages: string[];
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
}

/**
 * A content page's data: text in the reader's language, live versions with
 * their segments and access decisions, groups, attributes and the version to show.
 */
export function useContentDetail(id: string | null | undefined, o: { language?: string; version?: string | null; client?: ContentKitClient } = {}): UseContentDetail {
  const { client, store, scope, language: provided } = useContentScope(o.client);
  const language = o.language ?? provided;
  const key = id ? keyOf("content-detail", scope, id, language) : null;
  const r = useResource<Content>(store, key, { type: "content-detail", id: id ?? undefined }, (s) => client.contents.get(id!, language, s));
  const content = r.data ?? null;
  return useMemo(() => {
    const versions = liveVersions(content);
    const version = versions.find((v) => v.id === o.version) ?? defaultVersion(content, language);
    return {
      content,
      versions,
      version,
      decision: version?.decision ?? content?.decision ?? null,
      languages: [...new Set(versions.flatMap((v) => v.languages))],
      loading: !!key && (r.loading || !r.loaded),
      error: r.error,
      reload: r.reload,
    };
  }, [content, o.version, language, key, r.loading, r.loaded, r.error, r.reload]);
}
