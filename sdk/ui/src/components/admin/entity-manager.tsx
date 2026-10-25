import { cn } from "cn";
import { Add01Icon, ArrowLeft02Icon, Search01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { LETTERS } from "../../client/entity-fields.js";
import type { Content, ContentModel, EntityCard, Membership } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import { useAdminLocation, useFlowClient, useStaffAccess, type AdminLocation, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { useEntities, useKinds, type EntitySort } from "../../react/entities.js";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Tabs, TabsList, TabsTrigger } from "#ckui/ui/tabs";
import { EntityEditor, type EntityEditorProps } from "../entities/editor.js";
import { EntityMembers, type EntityMembersProps } from "../entities/entity-members.js";
import { labelOf, type Labels } from "../entities/parts.js";
import { ScopedEntities } from "../entities/scoped.js";
import { RenditionImg } from "../rendition-img.js";
import { contentError, useAction } from "../social/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Rows, Segmented, type AdminFlowProps, type AdminRouteProps } from "./shell.js";

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
  /** Tabs to switch kind (default true); off when the host's own navigation already lists the kinds. */
  kindTabs?: boolean;
}

/** Whether the caller may manage entities (app:entities:write). */
export function useEntityManagerAccess(o: FlowClientOptions = {}): StaffAccess<"entities"> {
  return useStaffAccess(["entities"] as const, o);
}

const SORTS: readonly EntitySort[] = ["name", "count", "newest"];

/**
 * Staff: entities per kind. A kind's entities by letter, order and language; a
 * new entity; an entity's editor (EntityEditor) and its members (EntityMembers).
 * Kinds are at basePath/{kind}, an entity at basePath/{kind}/{id}, filters in the query.
 */
export function EntityManager({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: EntityManagerProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="entity-manager" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Manager {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<EntityManagerProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };
type Ctx = Inner & { loc: AdminLocation; href?: (view: string) => string; kind: string; kinds: ContentModel; onLeaveGuard: NonNullable<EntityEditorProps["onLeaveGuard"]>; merged: (into: string) => void };

function Manager({ basePath, navigation, ...p }: Inner) {
  const { t } = useMessages();
  const loc = useAdminLocation(basePath, navigation);
  const leaveGuard = useRef<(() => Promise<boolean>) | undefined>(undefined);
  const registerLeaveGuard = useCallback((guard: (() => Promise<boolean>) | undefined) => { leaveGuard.current = guard; }, []);
  const go: AdminLocation["go"] = (...args) => {
    void (async () => {
      if ((await leaveGuard.current?.()) ?? true) loc.go(...args);
    })();
  };
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
  // Routed flows link their views, so staff can open them in new tabs.
  const href = basePath === undefined ? undefined : (view: string) => `${basePath.replace(/\/+$/, "")}/${view}`;
  const ctx: Ctx = { ...p, loc: { ...loc, go }, href, kind, kinds, onLeaveGuard: registerLeaveGuard, merged: (into) => loc.go(`${kind}/${into}`, undefined, { replace: true }) };
  return (
    <>
      {p.kindTabs !== false && list.length > 1 && (
        <Tabs value={kind} onValueChange={(v) => go(String(v))}>
          <TabsList variant="line" className="flex-wrap group-data-horizontal/tabs:h-auto">
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
    <Button variant="ghost" size="sm" className="-ms-2 h-8 justify-self-start gap-1.5 px-2 text-muted-foreground hover:text-foreground" onClick={() => loc.go(kind)}>
      <HugeiconsIcon icon={ArrowLeft02Icon} strokeWidth={2} data-icon="inline-start" />
      {t("entityManager.back")}
    </Button>
  );
}

function List({ loc, href, kind, kinds, client, contentKind, labels, onError, pageSize = 50 }: Ctx) {
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
  const items = entities.items;
  // Tags have no pictures; artists, series and seasons do. Only show the column when one is there.
  const pictured = items.some((g) => g.avatar?.renditions?.length || g.cover?.renditions?.length);
  // From md up the rows line up as a table: picture, name, contents, action.
  const columns = [pictured && "5rem", "minmax(0,1fr)", "6rem", "6rem"].filter(Boolean).join(" ");
  return (
    <div className="grid gap-5" data-ckui="entity-list" data-kind={kind}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 className="text-xl font-semibold tracking-tight">{label}</h2>
        <Button size="sm" aria-label={t("entityManager.new", { kind: labelOf(labels, kind, "one") })} onClick={() => loc.go(`${kind}/new`)}>
          <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
          {t("entityManager.newShort")}
        </Button>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Segmented
          label={label}
          options={[
            { key: "active", label: t("entityManager.active"), pressed: !deleted, onSelect: () => filter({ deleted: undefined }) },
            { key: "deleted", label: t("entityManager.deleted"), pressed: deleted, onSelect: () => filter({ deleted: "1" }) },
          ]}
        />
        <div className="relative min-w-48 flex-1">
          <HugeiconsIcon icon={Search01Icon} strokeWidth={2} aria-hidden className="pointer-events-none absolute start-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            type="search"
            className="h-8 ps-8"
            aria-label={t("entityManager.search")}
            placeholder={t("entityManager.search")}
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
        </div>
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
      <nav aria-label={t("entities.byLetter")} className="flex flex-wrap gap-0.5" data-ckui="letters">
        {[undefined, ...LETTERS].map((l) => (
          <Button
            key={l ?? "all"}
            size="xs"
            variant="ghost"
            aria-pressed={letter === l}
            aria-label={l === undefined ? undefined : l === "#" ? t("entities.otherLetters") : l.toUpperCase()}
            className={cn("min-w-7 max-sm:h-10 max-sm:min-w-10", letter === l ? "bg-muted text-foreground" : "text-muted-foreground")}
            onClick={() => filter({ letter: l })}
          >
            {l === undefined ? t("entities.all") : l.toUpperCase()}
          </Button>
        ))}
      </nav>
      {entities.loading && !items.length ? (
        <Rows />
      ) : entities.error && !items.length ? (
        <LoadFailed error={entities.error} message={t("entityManager.loadFailed")} retry={entities.reload} />
      ) : !items.length ? (
        <div className="grid justify-items-center gap-3 rounded-lg border border-dashed border-border px-6 py-12 text-center">
          <p className="text-muted-foreground">{t(deleted ? "entityManager.noDeleted" : query ? "entityManager.noMatches" : "entities.empty")}</p>
          {!deleted && !query && (
            <Button size="sm" onClick={() => loc.go(`${kind}/new`)}>
              <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
              {t("entityManager.newShort")}
            </Button>
          )}
        </div>
      ) : (
        <ul className="overflow-hidden rounded-lg border border-border" style={{ "--ck-cols": columns } as CSSProperties} aria-busy={entities.loading}>
          <li aria-hidden className="hidden min-h-10 items-center gap-4 border-b border-border bg-muted/40 px-3 py-1.5 text-xs font-medium text-muted-foreground md:grid md:grid-cols-(--ck-cols)" data-ckui="list-head">
            {pictured && <span />}
            <span>{label}</span>
            <span className="text-end">{t("entityManager.count")}</span>
            <span />
          </li>
          {items.map((g) => (
            <li
              key={g.id}
              className="flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-border px-3 py-2.5 transition-colors last:border-b-0 hover:bg-muted/30 md:grid md:grid-cols-(--ck-cols) md:gap-4"
              data-ckui="entity-row"
              data-slug={g.slug}
              data-state={deleted ? "deleted" : "active"}
            >
              {pictured && <EntityPicture card={g} />}
              <div className="grid min-w-[min(100%,12rem)] flex-1 gap-0.5">
                <div className="flex min-w-0 items-center gap-2">
                  {deleted ? (
                    <span className="truncate font-medium text-muted-foreground line-through">{g.name || g.slug}</span>
                  ) : (
                    <EntityLink href={href?.(`${kind}/${g.id}`)} title={g.name || g.slug} onOpen={() => loc.go(`${kind}/${g.id}`)}>
                      {g.name || g.slug}
                    </EntityLink>
                  )}
                  {g.internal && <span className="shrink-0 text-xs text-muted-foreground">{t("entityEditor.internal")}</span>}
                </div>
                <span className="truncate font-mono text-xs text-muted-foreground">{g.slug}</span>
              </div>
              <span className="text-sm text-muted-foreground tabular-nums md:text-end" title={t("entityManager.count")}>
                {new Intl.NumberFormat(m.language).format(g.count)}
              </span>
              <div className="ms-auto flex justify-end">
                {deleted ? (
                  <Button size="sm" variant="outline" disabled={restore.pending}
                    onClick={() => void restore.run(() => client.entities.update(g.id, { state: "active" })).then((d) => d !== undefined && entities.reload())}>
                    {t("entityEditor.restore")}
                  </Button>
                ) : (
                  <Button size="sm" variant="outline" tabIndex={-1} aria-hidden onClick={() => loc.go(`${kind}/${g.id}`)}>
                    {t("comments.edit")}
                  </Button>
                )}
              </div>
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

/** An entity's name opening its editor: a link when the flow is routed, so it also opens in a new tab. */
function EntityLink({ href, title, onOpen, children }: { href?: string; title: string; onOpen: () => void; children: ReactNode }) {
  const className = "truncate text-start font-medium underline-offset-4 hover:underline";
  if (!href)
    return (
      <button type="button" className={className} title={title} onClick={onOpen}>
        {children}
      </button>
    );
  return (
    <a
      href={href}
      className={className}
      title={title}
      onClick={(e) => {
        // A modified or middle click opens it the browser's way.
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
        e.preventDefault();
        onOpen();
      }}
    >
      {children}
    </a>
  );
}

/** An entity's picture in its own shape: a round avatar, or its cover as wide or tall as the image is. */
function EntityPicture({ card }: { card: EntityCard }) {
  const avatar = card.avatar?.renditions?.length ? card.avatar.renditions : undefined;
  const outputs = avatar ?? (card.cover?.renditions?.length ? card.cover.renditions : undefined);
  if (!outputs) return <span aria-hidden className="hidden md:block" />;
  const first = outputs[0]!;
  const shape = first.w > 0 && first.h > 0 ? first.w / first.h : 1;
  return (
    <RenditionImg
      outputs={outputs}
      alt=""
      loading="lazy"
      className={cn("shrink-0 bg-muted object-cover", avatar ? "size-10 rounded-full" : cn("rounded-md", shape >= 1 ? "w-20" : "w-12"))}
      style={avatar ? undefined : { aspectRatio: String(shape) }}
    />
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

function Edit({ loc, kind, client, labels, onError, renderContent, contentHref, newContentHref, id, onLeaveGuard, merged }: Ctx & { id: string }) {
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
        onLeaveGuard={onLeaveGuard}
        onMerged={(r) => merged(r.into)}
      >
        {(entity) => (
          <>
            <ScopedEntities entity={entity} management labels={labels} client={client} onSelect={(c) => loc.go(`${c.kind}/${c.id}`)} className="rounded-lg border border-border bg-card p-4" />
            <EntityMembers
              entity={entity}
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
