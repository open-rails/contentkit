import { useCallback, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { TypeaheadQuery } from "../../client/discovery.js";
import type { Card, EntityCard } from "../../client/generated/wire.js";
import { useContentScope } from "../../react/use-resource.js";
import { SearchBox, type SearchBoxProps } from "../content/search.js";

export interface AdminQuickSearchProps extends Omit<SearchBoxProps, "onSearch" | "href" | "kind" | "entityKinds" | "recent" | "requestSuggestions" | "selectionOnly" | "disabled"> {
  /** Content kinds for which the host offers an authorized editor. */
  contentKinds: readonly string[];
  /** Entity kinds for which the host offers an authorized editor. */
  entityKinds: readonly string[];
  /** An editor URL; returning undefined omits that suggestion. Public paths are never used. */
  contentHref: (item: Card) => string | undefined;
  entityHref: (entity: EntityCard) => string | undefined;
  disabled?: boolean;
}

/** Quick navigation to authorized editors, with the same combobox and header geometry as public search. */
export function AdminQuickSearch(p: AdminQuickSearchProps) {
  const { client, scope } = useContentScope(p.client);
  const disabled = p.disabled || (p.contentKinds.length === 0 && p.entityKinds.length === 0);
  const targets = JSON.stringify([p.contentKinds, p.entityKinds]);
  const [boundary, setBoundary] = useState({ client, scope, disabled, targets, key: 0 });
  let current = boundary;
  if (boundary.client !== client || boundary.scope !== scope || boundary.disabled !== disabled || boundary.targets !== targets) {
    current = { client, scope, disabled, targets, key: boundary.key + 1 };
    setBoundary(current);
  }
  return <QuickSearch key={current.key} {...p} client={client} disabled={disabled} />;
}

function QuickSearch({ contentKinds, entityKinds, contentHref, entityHref, ...p }: AdminQuickSearchProps & { client: ContentKitClient }) {
  const request = useCallback(async (query: TypeaheadQuery, signal: AbortSignal) => {
    const found = await p.client.discovery.adminSuggestions({
      q: query.q,
      display_language: query.display_language,
      content_kinds: contentKinds,
      entity_kinds: entityKinds,
      include_contents: contentKinds.length > 0,
      include_entities: entityKinds.length > 0,
      limit: query.limit,
    }, signal);
    return {
      contents: found.contents.filter((item) => contentKinds.includes(item.kind) && !!contentHref(item)),
      entities: found.entities.filter((entity) => entityKinds.includes(entity.kind) && !!entityHref(entity)),
    };
  }, [p.client, contentKinds, entityKinds, contentHref, entityHref]);
  return <SearchBox {...p} recent={false} selectionOnly requestSuggestions={request} onSearch={() => {}}
    href={(item) => "title" in item ? contentHref(item) : entityHref(item)} />;
}
