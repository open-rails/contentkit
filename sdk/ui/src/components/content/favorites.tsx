import { useMemo } from "react";
import type { MineQuery } from "../../client/content/favorites.js";
import { useMyFavorites } from "../../react/discovery.js";
import { CardGrid, type CardListProps } from "./grid.js";

/** Favorite contents, with the same filtering, paid previews and pagination as public lists. */
export function ContentFavorites({ query = {}, ...p }: CardListProps & { query?: Omit<MineQuery, "target"> }) {
  const q = useMemo(() => ({ ...query, target: "contents" as const, summary: query.summary ?? true, limit: query.limit ?? p.pageSize }), [query, p.pageSize]);
  const favorites = useMyFavorites(q, { client: p.client });
  const items = useMemo(() => favorites.items.flatMap((item) => item.content ? [item.content] : []), [favorites.items]);
  return (
    <CardGrid
      surface="favorites"
      {...p}
      list={{ ...favorites, items }}
      render={JSON.stringify(q)}
      filterKinds={q.kind ? [q.kind] : undefined}
      filterLanguage={q.display_language ?? q.language}
    />
  );
}
