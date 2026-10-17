import { useContext, useMemo } from "react";
import type { MineQuery } from "../../client/content/favorites.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext } from "../../react/context.js";
import { useMyFavorites } from "../../react/discovery.js";
import { Button } from "#ckui/ui/button";
import { CardGrid, cardListMode, type CardListProps } from "./grid.js";

/** Favorite contents, with the same filtering, paid previews and pagination as public lists; signed out, a way to sign in. */
export function ContentFavorites({ query = {}, ...p }: CardListProps & { query?: Omit<MineQuery, "target"> }) {
  const q = useMemo(
    () => ({ ...query, target: "contents" as const, mode: cardListMode(p.locked, query.mode), summary: query.summary ?? true, limit: query.limit ?? p.pageSize }),
    [query, p.pageSize, p.locked],
  );
  const favorites = useMyFavorites(q, { client: p.client });
  const items = useMemo(() => favorites.items.flatMap((item) => (item.content ? [item.content] : [])), [favorites.items]);
  const ctx = useContext(ContentKitContext);
  const { t } = useMessages();
  if (ctx?.viewer === null) {
    return (
      <div className="grid justify-items-start gap-3 text-sm text-muted-foreground" data-ckui="favorites-signed-out">
        <p>{t("favorites.signIn")}</p>
        {ctx.onSignIn && (
          <Button size="sm" onClick={ctx.onSignIn}>
            {t("paywall.signIn")}
          </Button>
        )}
      </div>
    );
  }
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
