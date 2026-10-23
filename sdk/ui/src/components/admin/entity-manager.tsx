import { cn } from "cn";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { LETTERS } from "../../client/entity-fields.js";
import type { Content, ContentModel, Membership } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import { useAdminLocation, useFlowClient, useStaffAccess, type AdminLocation, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { useEntities, useKinds, type EntitySort } from "../../react/entities.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Tabs, TabsList, TabsTrigger } from "#ckui/ui/tabs";
import { EntityEditor } from "../entities/editor.js";
import { EntityMembers, type EntityMembersProps } from "../entities/entity-members.js";
import { labelOf, type Labels } from "../entities/parts.js";
import { ScopedEntities } from "../entities/scoped.js";
import { RenditionImg } from "../rendition-img.js";
import { contentError, useAction } from "../social/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Rows, type AdminFlowProps, type AdminRouteProps } from "./shell.js";

export interface EntityManagerProps extends AdminFlowProps, AdminRouteProps {
  /** The entity kinds managed, in tab order; default the content model's site-owned kinds. */
  entityKinds?: readonly string[];
  /** Display names of kinds, attributes, roles and values by identifier. */
  labels?: Labels;
  /** Counts of this content kind; default every kind. */
  contentKind?: string;
  /** Where a member's card links, e.g. the host's editor; none: not a link. */
  contentHref?: (content: Content) => string | undefined;
  /** Where a new item of a content kind starts already in the entity being edited; none: no action. */
  newContentHref?: EntityMembersProps["newContentHref"];
  /** Replaces a member's card body; default its cover and title. */
  renderContent?: (member: Membership, content?: Content) => ReactNode;
  pageSize?: number;
}

/** Whether the caller may manage entities (app:entities:write). */
export function useEntityManagerAccess(o: FlowClientOptions = {}): StaffAccess<"entities"> {
  return useStaffAccess(["entities"] as const, o);
}

const SORTS: readonly EntitySort[] = ["name", "count", "newest"];

/**
 * Staff: entities per kind. A kind's entities by letter, order and language; a
 * new entity; a entity's editor (EntityEditor) and its members (EntityMembers).
 * Kinds are at basePath/{kind}, a entity at basePath/{kind}/{id}, filters in the query.
 */
export function EntityManager({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: EntityManagerProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="group-manager" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Manager {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<EntityManagerProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };
type Ctx = Inner & { loc: AdminLocation; kind: string; kinds: ContentModel };

function Manager({ basePath, navigation, ...p }: Inner) {
  const { t } = useMessages();
  const loc = useAdminLocation(basePath, navigation);
  const access = useEntityManagerAccess({ client: p.client });
  const model = useKinds({ client: p.client });
  const kinds = model.kinds;
  const list = useMemo(
    () =>
      p.entityKinds ??
      Object.entries(kinds?.entity_kinds ?? {})
        .filter(([, k]) => k.owner !== "user")
        .map(([name]) => name)
        .sort(),
    [p.entityKinds, kinds],
  );
  if (access.loading || model.loading) return <Rows />;
  if (!access.allowed.entities) return <NoAccess>{t("entityManager.noAccess")}</NoAccess>;
  if (!kinds) return <LoadFailed error={model.error} message={t("entityManager.kindsFailed")} retry={model.reload} />;
  if (!list.length) return <p className="text-muted-foreground">{t("entityManager.noKinds")}</p>;
  const [k, sub] = loc.view.split("/");
  const kind = list.includes(k ?? "") ? k! : list[0]!;
  const ctx: Ctx = { ...p, loc, kind, kinds };
  return (
    <>
      {list.length > 1 && (
        <Tabs value={kind} onValueChange={(v) => loc.go(String(v))}>
          <TabsList className="flex-wrap group-data-horizontal/tabs:h-auto">
            {list.map((x) => (
              <TabsTrigger key={x} value={x} className="h-7.5">
                {labelOf(p.labels, x)}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>
      )}
      {!sub ? <List key={kind} {...ctx} /> : sub === "new" ? <Create key={kind} {...ctx} /> : <Edit key={sub} {...ctx} id={sub} />}
    </>
  );
}

function Back({ loc, kind }: { loc: AdminLocation; kind: string }) {
  const { t } = useMessages();
  return (
    <Button variant="ghost" size="sm" className="justify-self-start" onClick={() => loc.go(kind)}>
      {t("entityManager.back")}
    </Button>
  );
}

function List({ loc, kind, kinds, client, contentKind, labels, onError, pageSize = 50 }: Ctx) {
  const m = useMessages();
  const { t } = m;
  const languageName = useLanguageName();
  const languages = kinds.languages;
  const deleted = loc.query.get("deleted") === "1";
  const query = loc.query.get("q") ?? "";
  // Typing narrows the list as it goes; the URL keeps the search.
  const [text, setText] = useState(query);
  useEffect(() => setText(query), [query]);
  const letterParam = loc.query.get("letter") ?? "";
  const letter = LETTERS.includes(letterParam) ? letterParam : undefined;
  const sortParam = loc.query.get("sort") ?? "";
  const sort = (SORTS as readonly string[]).includes(sortParam) ? (sortParam as EntitySort) : "name";
  const languageParam = loc.query.get("language") ?? "";
  const language = languages.includes(languageParam) ? languageParam : languages[0];
  const filters: { letter?: string; sort?: EntitySort; language?: string; q?: string; deleted?: string } = {
    letter,
    sort: sort === "name" ? undefined : sort,
    language: language === languages[0] ? undefined : language,
    q: query || undefined,
    deleted: deleted ? "1" : undefined,
  };
  const filter = (next: Partial<typeof filters>) => loc.go(kind, { ...filters, ...next });
  useEffect(() => {
    if (text.trim() === query) return;
    const timer = setTimeout(() => loc.go(kind, { ...filters, q: text.trim() || undefined }, { replace: true }), 250);
    return () => clearTimeout(timer);
  }, [text]); // eslint-disable-line react-hooks/exhaustive-deps
  const entities = useEntities({ kind, letter, sort, language, contentKind, pageSize, query: query || undefined, deleted }, { client, management: true });
  const restore = useAction("entity.save", onError);
  const label = labelOf(labels, kind);
  return (
    <div className="grid gap-4" data-ckui="entity-list" data-kind={kind}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold">{label}</h2>
        <Button size="sm" aria-label={t("entityManager.new", { kind: label })} onClick={() => loc.go(`${kind}/new`)}>
          {t("entityManager.newShort")}
        </Button>
      </div>
      <div className="flex gap-2" role="group" aria-label={label}>
        <Button size="sm" variant={deleted ? "outline" : "default"} aria-pressed={!deleted} onClick={() => filter({ deleted: undefined })}>
          {t("entityManager.active")}
        </Button>
        <Button size="sm" variant={deleted ? "default" : "outline"} aria-pressed={deleted} onClick={() => filter({ deleted: "1" })}>
          {t("entityManager.deleted")}
        </Button>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Input
          type="search"
          className="h-8 min-w-48 flex-1"
          aria-label={t("entityManager.search")}
          placeholder={t("entityManager.search")}
          value={text}
          onChange={(e) => setText(e.target.value)}
        />
        <Select value={sort} onValueChange={(v) => filter({ sort: v === "name" ? undefined : (String(v) as EntitySort) })}>
          <SelectTrigger size="sm" aria-label={t("entityManager.sort")}>
            <SelectValue>{(v: string) => t(`entityManager.sort_${v as EntitySort}`)}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {SORTS.map((s) => (
              <SelectItem key={s} value={s}>
                {t(`entityManager.sort_${s}`)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {languages.length > 1 && (
          <Select value={language} onValueChange={(v) => filter({ language: v === languages[0] ? undefined : String(v) })}>
            <SelectTrigger size="sm" aria-label={t("entityManager.language")}>
              <SelectValue>{(v: string) => languageName(v)}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {languages.map((l) => (
                <SelectItem key={l} value={l}>
                  {languageName(l)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
      </div>
      <nav aria-label={t("entities.byLetter")} className="flex flex-wrap gap-1" data-ckui="letters">
        {[undefined, ...LETTERS].map((l) => (
          <Button
            key={l ?? "all"}
            size="xs"
            variant={letter === l ? "default" : "ghost"}
            aria-pressed={letter === l}
            aria-label={l === undefined ? undefined : l === "#" ? t("entities.otherLetters") : l.toUpperCase()}
            className="min-w-7"
            onClick={() => filter({ letter: l })}
          >
            {l === undefined ? t("entities.all") : l.toUpperCase()}
          </Button>
        ))}
      </nav>
      {entities.loading && !entities.items.length ? (
        <Rows />
      ) : entities.error && !entities.items.length ? (
        <LoadFailed error={entities.error} message={t("entityManager.loadFailed")} retry={entities.reload} />
      ) : !entities.items.length ? (
        <p className="text-muted-foreground">{t(deleted ? "entityManager.noDeleted" : query ? "entityManager.noMatches" : "entities.empty")}</p>
      ) : (
        <ul className="grid divide-y divide-border rounded-lg border border-border" aria-busy={entities.loading}>
          {entities.items.map((g) => (
            <li key={g.id} className="flex flex-wrap items-center gap-3 p-3" data-ckui="entity-row" data-slug={g.slug} data-state={deleted ? "deleted" : "active"}>
              {(g.avatar?.renditions?.[0] ?? g.cover?.renditions?.[0]) && (
                <RenditionImg outputs={g.avatar?.renditions?.length ? g.avatar.renditions : g.cover?.renditions} alt="" loading="lazy" className={cn("size-24 shrink-0 object-cover sm:size-28", g.avatar?.renditions?.[0] ? "rounded-full" : "rounded-md")} />
              )}
              <div className="grid min-w-0 flex-1 gap-0.5">
                <button type="button" className="text-left font-medium break-words hover:underline" onClick={() => loc.go(`${kind}/${g.id}`)}>
                  {g.name || g.slug}
                </button>
                <span className="text-xs text-muted-foreground">{g.slug}</span>
                {g.internal && <Badge variant="secondary" className="justify-self-start">{t("entityEditor.internal")}</Badge>}
              </div>
              <span className="text-sm text-muted-foreground tabular-nums" title={t("entityManager.count")}>
                {new Intl.NumberFormat(m.language).format(g.count)}
              </span>
              {deleted && (
                <Button size="sm" variant="outline" disabled={restore.pending}
                  onClick={() => void restore.run(() => client.entities.update(g.id, { state: "active" })).then((d) => d !== undefined && entities.reload())}>
                  {t("entityEditor.restore")}
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
      {entities.hasMore && (
        <div className="grid justify-items-center gap-2">
          {entities.error && <p role="alert" className="text-destructive">{contentError(m, entities.error)}</p>}
          <Button variant="outline" size="sm" disabled={entities.loadingMore} onClick={() => void entities.loadMore()}>
            {entities.loadingMore ? t("entities.loading") : entities.error ? t("common.retry") : t("entities.loadMore")}
          </Button>
        </div>
      )}
    </div>
  );
}

function Create({ loc, kind, client, labels, onError }: Ctx) {
  return (
    <div className="grid gap-4">
      <Back loc={loc} kind={kind} />
      <EntityEditor
        management
        id={null}
        kind={kind}
        labels={labels}
        client={client}
        onError={onError}
        onCreated={(g) => loc.go(`${kind}/${g.id}`, undefined, { replace: true })}
      />
    </div>
  );
}

function Edit({ loc, kind, client, labels, onError, renderContent, contentHref, newContentHref, id }: Ctx & { id: string }) {
  return (
    <div className="grid gap-4">
      <Back loc={loc} kind={kind} />
      <EntityEditor
        management
        id={id}
        kind={kind}
        labels={labels}
        client={client}
        onError={onError}
        onMerged={(r) => loc.go(`${kind}/${r.into}`, undefined, { replace: true })}
      >
        {(entity) => (
          <>
            <ScopedEntities entityy={entity} management labels={labels} client={client} onSelect={(c) => loc.go(`${c.kind}/${c.id}`)} />
            <EntityMembers
              entityy={entity}
              labels={labels}
              client={client}
              onError={onError}
              renderContent={renderContent}
              contentHref={contentHref}
              newContentHref={newContentHref}
              onUseCover={(content) => client.entities.update(entity.id, { cover: { content_id: content } })}
            />
          </>
        )}
      </EntityEditor>
    </div>
  );
}
