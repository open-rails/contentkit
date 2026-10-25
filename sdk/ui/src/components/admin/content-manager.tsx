import { Add01Icon, ArrowLeft02Icon, Delete02Icon, Image01Icon, LinkSquare02Icon, MoreHorizontalIcon, Search01Icon, Upload04Icon, ViewIcon } from "@hugeicons/core-free-icons";
import { cn } from "cn";
import { HugeiconsIcon } from "@hugeicons/react";
import { useCallback, useEffect, useId, useMemo, useRef, useState, type CSSProperties, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import { imageRefs } from "../../client/contents.js";
import { selectedEdition } from "../../client/edition-access.js";
import { allowsEntityScope } from "../../client/entity-scopes.js";
import type { Content, EntityCard, EntityDetail, ManageCount, Rule, Version } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import type { Translator } from "../../i18n/messages.js";
import { useAdminLocation, useFlowClient, useStaffAccess, type AdminLocation, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { useContentKitClient, useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { useContent } from "../../react/contents.js";
import { useKinds } from "../../react/entities.js";
import { keyOf, useContentScope, usePaged, useResource } from "../../react/use-resource.js";
import { Button, buttonVariants } from "#ckui/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "#ckui/ui/dropdown-menu";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { Textarea } from "#ckui/ui/textarea";
import { localInput } from "../social/bans.js";
import { contentError, ErrorLine, formatDate, RelativeTime, useAction } from "../social/parts.js";
import { ReaderContentEditor } from "./reader-content-editor.js";
import { ContentUploadBody } from "./content-upload.js";
import { AccessEditor, ContentDetailsEditor, VersionsEditor, offersAccessChoice, versionLabel } from "../content/editors.js";
import { MembershipEditor } from "../entities/memberships.js";
import { labelOf, type Labels } from "../entities/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Pager, Rows, Segmented, pageOf, useConfirm, type AdminFlowProps, type AdminRouteProps } from "./shell.js";

type Choice = string | { value: string; label: string };

/** What ContentManager hands its body editor. */
export interface BodyEditorProps {
  /** The body as HTML, its images at URLs that show them now. */
  value: string;
  onChange: (html: string) => void;
  /** Uploads an image to the item and resolves with a URL that shows it; absent until the item exists. */
  uploadImage?: (file: File) => Promise<string>;
  disabled?: boolean;
  placeholder?: string;
  "aria-label"?: string;
}

export interface ContentManagerProps extends AdminFlowProps, AdminRouteProps {
  /** The content kind managed. */
  kind: string;
  /** Languages to write in: codes, or codes with labels; the first is a new item's. */
  languages?: readonly Choice[];
  /**
   * The body editor: `richTextEditor` from `@openrails/contentkit-ui/editor`,
   * or the host's own. Without it, the body is a plain HTML field.
   */
  renderEditor?: (props: BodyEditorProps) => ReactNode;
  /** A link to a live item's page. */
  contentHref?: (content: Content) => string | undefined;
  /** Display names of kinds, traits, tiers, attributes and values by identifier. */
  labels?: Labels;
  /** The list's default page size: 20, 50 or 100. */
  pageSize?: number;
  /** Row checkboxes for deleting or restoring hidden editions (default true). */
  selectable?: boolean;
  /** The kind's name, localized by the host and as it reads mid-sentence (e.g. gallery, galleries): the list heading, New button and empty state. */
  noun?: { one: string; other: string };
}

/** Whether the caller manages content: root:content:write holders, creators and owner-group editors. */
export function useContentManagerAccess(o: FlowClientOptions = {}): StaffAccess<"contents"> {
  return useStaffAccess(["contents"] as const, o);
}

export type ContentStatus = "draft" | "scheduled" | "publishing" | "published";

/** An item's status as the manager names it. */
export function contentStatus(c: Content): ContentStatus {
  if (c.state === "draft") return "draft";
  if (!c.live) return c.publish_at && Date.parse(c.publish_at) > Date.now() ? "scheduled" : "publishing";
  return "published";
}

const choices = (list?: readonly Choice[], name: (code: string) => string = (c) => c) => (list ?? []).map((c) => (typeof c === "string" ? { value: c, label: name(c) } : c));
const states = ["draft", "scheduled", "published"] as const;

/**
 * Content items of one kind for their creators and staff: every one the
 * caller manages, by state and text, deleted ones on their own to restore;
 * create and edit title, description, schedule, cover and body with
 * inline images per language, publish or keep a draft, and delete; then its
 * listing and attributes, access rule and price, versions (order, languages,
 * traits, schedule, access override, price, segments, media) and groups. The
 * list is at basePath, a new item at basePath/new and each item at
 * basePath/{code}, with editions at basePath/{code}/versions/{versionCode}
 * and list filters in the query.
 */
export function ContentManager({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: ContentManagerProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="content-manager" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Contents {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<ContentManagerProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };

function Contents({ basePath, navigation, ...p }: Inner) {
  const { t } = useMessages();
  const loc = useAdminLocation(basePath, navigation);
  const access = useContentManagerAccess({ client: p.client });
  const leaveGuard = useRef<(() => Promise<boolean>) | undefined>(undefined);
  const registerLeaveGuard = useCallback((guard: (() => Promise<boolean>) | undefined) => { leaveGuard.current = guard; }, []);
  // Editor URLs name items by content code; the editor itself works on ids.
  const known = useRef(new Map<string, string>()).current;
  const route = contentEditorRoute(loc.view);
  const createdCode = useRef<string | undefined>(undefined);
  const open = (c: Content, replace = false) => {
    known.set(c.code, c.id);
    createdCode.current = c.code;
    loc.go(encodeURIComponent(c.code), undefined, replace ? { replace: true } : undefined);
  };
  const target = useCodeTarget(p.client, p.kind, route?.code ?? null, known);
  const entities = useStartingEntities(p.client, loc.view === "new" ? (loc.query.get("with") ?? "") : "");
  if (access.loading) return <Rows />;
  if (!access.allowed.contents) return <NoAccess>{t("contentManager.noAccess")}</NoAccess>;
  if (!loc.view) return <List {...p} loc={loc} open={open} />;
  if (!route) return <NoAccess>{t("contentManager.notFound")}</NoAccess>;
  if (loc.view !== "new" && !target.id) {
    if (target.error) return <LoadFailed error={target.error} message={t("contentManager.loadFailed")} retry={target.retry} />;
    return target.missing ? <NoAccess>{t("contentManager.notFound")}</NoAccess> : <Rows />;
  }
  return (
    <>
      <Button
        variant="ghost"
        size="sm"
        className="-ms-2 gap-1.5 text-muted-foreground hover:text-foreground justify-self-start h-8 px-2"
        onClick={() => void (async () => {
          if ((await leaveGuard.current?.()) ?? true) loc.go("");
        })()}
      >
        <HugeiconsIcon icon={ArrowLeft02Icon} strokeWidth={2} data-icon="inline-start" />
        {p.noun?.other ? t("contentManager.backToNoun", { noun: p.noun.other }) : t("contentManager.back")}
      </Button>
      <ContentEditor
        management
        {...p}
        id={loc.view === "new" ? null : target.id}
        versionCode={route.versionCode}
        versionHref={(version) => {
          if (basePath === undefined && !navigation) return undefined;
          const code = route.code ?? createdCode.current;
          return code ? `${basePath?.replace(/\/+$/, "") ?? ""}/${encodeURIComponent(code)}/versions/${encodeURIComponent(version.code)}` : undefined;
        }}
        onVersionChange={(version, options) => {
          const code = route.code ?? createdCode.current;
          if (code) loc.go(version ? `${encodeURIComponent(code)}/versions/${encodeURIComponent(version.code)}` : encodeURIComponent(code), undefined, options);
        }}
        entities={entities}
        onCreated={(c) => open(c, true)}
        onDeleted={() => loc.go("", undefined, { replace: true })}
        onLeaveGuard={registerLeaveGuard}
      />
    </>
  );
}

/** Only the parent code is resolved through management; an edition stays scoped to that parent. */
function contentEditorRoute(view: string): { code?: string; versionCode?: string } | null {
  if (view === "new") return {};
  const parts = view.split("/");
  if (parts.length !== 1 && (parts.length !== 3 || parts[1] !== "versions")) return null;
  try {
    const code = decodeURIComponent(parts[0]!);
    const versionCode = parts.length === 3 ? decodeURIComponent(parts[2]!) : undefined;
    const invalid = (part: string) => !part || part.includes("/") || part.includes("\\");
    if (invalid(code) || code === "new" || (versionCode !== undefined && invalid(versionCode))) return null;
    return { code, versionCode };
  } catch {
    return null;
  }
}

/** Resolves an editor URL's content code to its id within the caller's manage rights; known codes resolve at once. */
function useCodeTarget(client: ContentKitClient, kind: string, code: string | null, known: Map<string, string>) {
  const [found, setFound] = useState<{ code: string; id: string | null; error?: ContentKitError } | null>(null);
  const [attempt, setAttempt] = useState(0);
  const cached = code ? known.get(code) : undefined;
  useEffect(() => {
    if (!code || cached) return;
    const ctl = new AbortController();
    client.contents.manage({ kind, code, limit: 1 }, ctl.signal).then(
      (items) => {
        const id = items[0]?.id ?? null;
        if (id) known.set(code, id);
        setFound({ code, id });
      },
      (e) => {
        if (!ctl.signal.aborted) setFound({ code, id: null, error: toContentKitError(e) });
      },
    );
    return () => ctl.abort();
  }, [client, kind, code, cached, known, attempt]);
  const retry = () => setAttempt((n) => n + 1);
  if (!code) return { id: null, retry };
  if (cached) return { id: cached, retry };
  if (found?.code !== code) return { id: null, retry };
  return { id: found.id, error: found.error, missing: !found.id && !found.error, retry };
}

const asCard = (g: EntityDetail): EntityCard => ({ ...g, count: 0 });

/** A new item's starting entities from `?with=CODE[,CODE]`, each with its scope (a character's series); unresolvable codes are skipped. */
function useStartingEntities(client: ContentKitClient, codes: string): readonly EntityCard[] | undefined {
  const [found, setFound] = useState<{ codes: string; entities: EntityCard[] } | null>(null);
  useEffect(() => {
    if (!codes) return;
    const ctl = new AbortController();
    const one = async (code: string) => {
      const r = await client.codes.resolve(code, { signal: ctl.signal });
      const g = await client.entities.getForManagement(r.content_id, undefined, ctl.signal);
      return g.scope ? [asCard(g), g.scope] : [asCard(g)];
    };
    void Promise.all(codes.split(",").filter(Boolean).map((c) => one(c).catch(() => [] as EntityCard[]))).then((all) => {
      if (!ctl.signal.aborted) setFound({ codes, entities: all.flat() });
    });
    return () => ctl.abort();
  }, [client, codes]);
  return codes && found?.codes === codes ? found.entities : undefined;
}

const PAGE_SIZES = [20, 50, 100] as const;
// Bulk writes run a few at a time so one selection never floods the server.
const BULK_CONCURRENCY = 4;

/** A rule as a short label: free, its tiers, pay per view or members only. */
export function accessLabel(t: Translator["t"], rule: Rule, labels?: Labels): string {
  switch (rule.level) {
    case "public":
      return t("contentManager.free");
    case "tier":
      return (rule.tiers ?? []).map((x) => labelOf(labels, x)).join(", ") || t("contentManager.premium");
    case "ppv":
      return t("contentManager.ppv");
    case "group":
      return t("contentManager.members");
    case "group_ppv":
      return t("contentManager.membersPpv");
  }
}

async function eachLimited<T>(items: readonly T[], fn: (item: T) => Promise<unknown>): Promise<number> {
  let failed = 0;
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const item = items[next++]!;
      try {
        await fn(item);
      } catch {
        failed++;
      }
    }
  };
  await Promise.all(Array.from({ length: Math.min(BULK_CONCURRENCY, items.length) }, worker));
  return failed;
}

function List({ loc, open, client, kind, languages: offered, contentHref, labels, pageSize: defaultSize = 20, selectable = true, noun, onError }: Inner & { loc: AdminLocation; open: (c: Content) => void }) {
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const { kinds: model } = useKinds({ client });
  const spec = model?.kinds[kind];
  const language = choices(offered)[0]?.value;
  const query = loc.query.get("q") ?? "";
  const status = states.find((s) => s === loc.query.get("status")) ?? "";
  const deleted = loc.query.get("deleted") === "1";
  const page = pageOf(loc.query);
  const sizeParam = Number(loc.query.get("size"));
  const size = PAGE_SIZES.find((n) => n === sizeParam) ?? defaultSize;
  const filters = { q: query, status, deleted: deleted ? "1" : undefined, size: size !== defaultSize ? size : undefined };
  const filter = (next: Partial<typeof filters>) => loc.go("", { ...filters, ...next });
  const filtered = !!(query || status);
  const [text, setText] = useState(query);
  useEffect(() => setText(query), [query]);
  // Searches as the admin types; Enter applies at once.
  useEffect(() => {
    if (text.trim() === query) return;
    const id = setTimeout(() => filter({ q: text.trim() }), 300);
    return () => clearTimeout(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [text]);
  // Reader works take one ZIP per draft, players one video per draft.
  const presentation = spec?.presentation;
  const hasHiddenVersion = !!spec?.versions.hidden;
  const showEditionAccess = offersAccessChoice(spec);
  const canSelect = selectable && hasHiddenVersion;
  const bulkable = presentation === "reader" || presentation === "player";
  const [bulkOpen, setBulkOpen] = useState(false);
  const { store, scope } = useContentScope(client);
  const state = deleted ? "deleted" : status || undefined;
  const tag = { type: "contents", scope: deleted ? "deleted" : undefined } as const;
  const filterQuery = { q: query || undefined, state };
  const listing = usePaged(store, keyOf("contents-page", scope, kind, query, state, language, size), tag, page, size, (q, signal) =>
    client.contents.manage({ ...q, ...filterQuery, kind, language }, signal),
  );
  const counted = useResource<ManageCount>(store, keyOf("contents-count", scope, kind, query, state), tag, (signal) =>
    client.contents.manageCount({ ...filterQuery, kind }, signal),
  );
  const items = listing.items;
  const total = counted.data?.total;
  const loading = listing.loading;
  const del = useAction("content.save", onError);
  const { confirm, dialog } = useConfirm();
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  useEffect(() => setSelected(new Set()), [kind, query, state, page, size]);
  const [bulk, setBulk] = useState<{ pending: boolean; summary?: string }>({ pending: false });
  const removeVersion = (c: Content) => {
    const version = c.versions.find((v) => v.state !== "deleted");
    if (!version) throw new Error("contentkit-ui: the item has no undeleted version");
    return client.contents.removeVersion(c.id, version.id);
  };
  const restore = async (c: Content) => {
    const version = c.versions.find((v) => v.state === "deleted");
    if (version) await client.contents.updateVersion(c.id, version.id, { state: "published" });
    else await client.contents.restore(c.id);
  };
  const remove = async (c: Content) => {
    if (await confirm({ title: t("contentManager.deleteTitle", { title: c.title }), description: t("contentManager.deleteBody"), confirm: t("contentManager.delete") })) {
      await del.run(() => removeVersion(c));
    }
  };
  const chosen = items.filter((c) => selected.has(c.id));
  const runBulk = async (targets: Content[], fn: (c: Content) => Promise<unknown>) => {
    setBulk({ pending: true });
    const failed = await eachLimited(targets, fn);
    const done = targets.length - failed;
    setSelected(new Set());
    setBulk({
      pending: false,
      summary: failed ? t("contentManager.bulkPartial", { done, total: targets.length, failed }) : m.plural("contentManager.bulkDone", done),
    });
  };
  const bulkDelete = async () => {
    if (await confirm({ title: m.plural("contentManager.bulkDeleteTitle", chosen.length), description: t("contentManager.bulkDeleteBody"), confirm: t("contentManager.delete") })) {
      await runBulk(chosen, (c) => removeVersion(c));
    }
  };
  const allChosen = items.length > 0 && chosen.length === items.length;
  const heading = noun?.other ?? t("contentManager.title");
  const newLabel = noun ? t("contentManager.newNamed", { one: noun.one }) : t("contentManager.new");
  // From md up the rows line up as a table: select, item, status, access, date, actions.
  const columns = [canSelect && "1rem", "minmax(0,1fr)", "8rem", showEditionAccess && "8rem", "9rem", "6.5rem"].filter(Boolean).join(" ");
  const toggle = (c: Content, on: boolean) =>
    setSelected((cur) => {
      const n = new Set(cur);
      if (on) n.add(c.id);
      else n.delete(c.id);
      return n;
    });
  return (
    <div className="grid gap-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-baseline gap-2">
          <h2 className="text-xl font-semibold tracking-tight first-letter:uppercase">{heading}</h2>
          {total !== undefined && (
            <span className="text-sm text-muted-foreground tabular-nums">
              {new Intl.NumberFormat(m.language).format(total)}
              {counted.data?.truncated && "+"}
            </span>
          )}
        </div>
        <div className="flex gap-2">
          {bulkable && !deleted && (
            <Button size="sm" variant="outline" aria-expanded={bulkOpen} onClick={() => setBulkOpen((o) => !o)}>
              <HugeiconsIcon icon={Upload04Icon} strokeWidth={2} data-icon="inline-start" />
              {t("contentManager.bulkUpload")}
            </Button>
          )}
          <Button size="sm" onClick={() => loc.go("new")}>
            <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
            {newLabel}
          </Button>
        </div>
      </div>
      {bulkable && bulkOpen && !deleted && model && (
        <section className="grid gap-3 rounded-lg border border-border p-4" aria-label={t("contentManager.bulkUpload")} data-ckui="bulk-upload">
          <p className="text-sm text-muted-foreground">{t(presentation === "reader" ? "contentManager.bulkHintArchive" : "contentManager.bulkHintFile")}</p>
          <ContentUploadBody client={client} kind={kind} model={model} languages={offered} labels={labels} open={open} onError={onError} />
        </section>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <Segmented
          label={heading}
          options={[
            { key: "items", label: t("contentManager.items"), pressed: !deleted, onSelect: () => loc.go("", { q: query }) },
            { key: "deleted", label: t("contentManager.deleted"), pressed: deleted, onSelect: () => loc.go("", { q: query, deleted: "1" }) },
          ]}
        />
        <form
          className="relative min-w-48 flex-1"
          role="search"
          onSubmit={(e) => {
            e.preventDefault();
            filter({ q: text.trim() });
          }}
        >
          <Label htmlFor={`${ids}-q`} className="sr-only">
            {t("contentManager.search")}
          </Label>
          <HugeiconsIcon icon={Search01Icon} strokeWidth={2} aria-hidden className="pointer-events-none absolute start-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input id={`${ids}-q`} className="h-8 ps-8" type="search" value={text} placeholder={t("contentManager.search")} onChange={(e) => setText(e.target.value)} />
        </form>
        {!deleted && (
          <Select value={status} onValueChange={(v) => filter({ status: String(v ?? "") as typeof status })}>
            <SelectTrigger size="sm" aria-label={t("contentManager.status")}>
              <SelectValue>{(v: string) => t(v ? `contentManager.${v as (typeof states)[number]}` : "contentManager.allStatuses")}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="">{t("contentManager.allStatuses")}</SelectItem>
              {states.map((s) => (
                <SelectItem key={s} value={s}>
                  {t(`contentManager.${s}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
      </div>
      <ErrorLine error={del.error} />
      {bulk.summary && (
        <p role="status" className="text-sm text-muted-foreground">
          {bulk.summary}
        </p>
      )}
      {loading && !items.length ? (
        <Rows />
      ) : listing.error && !items.length ? (
        <LoadFailed error={listing.error} message={t("contentManager.loadFailed")} retry={listing.reload} />
      ) : !items.length ? (
        <div className="grid justify-items-center gap-3 rounded-lg border border-dashed border-border px-6 py-12 text-center">
          <p className="text-muted-foreground">{filtered ? t("contentManager.noMatches") : deleted ? t("contentManager.noDeleted") : noun ? t("contentManager.emptyNamed", { other: noun.other }) : t("contentManager.emptyAll")}</p>
          {filtered ? (
            <Button size="sm" variant="outline" onClick={() => loc.go("", { deleted: deleted ? "1" : undefined })}>
              {t("contentManager.clearFilters")}
            </Button>
          ) : (
            !deleted && (
              <Button size="sm" onClick={() => loc.go("new")}>
                <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
                {newLabel}
              </Button>
            )
          )}
        </div>
      ) : (
        <ul className="overflow-hidden rounded-lg border border-border" style={{ "--ck-cols": columns } as CSSProperties} aria-busy={loading}>
          <li
            className={cn(
              "flex min-h-11 items-center gap-3 border-b border-border bg-muted/40 px-3 py-1.5 text-xs font-medium text-muted-foreground",
              !canSelect && "hidden md:flex",
              !chosen.length && "md:grid md:grid-cols-(--ck-cols) md:gap-4",
            )}
            data-ckui="list-head"
          >
            {canSelect && (
              <input
                type="checkbox"
                className="size-4 shrink-0 accent-primary"
                checked={allChosen}
                ref={(el) => {
                  if (el) el.indeterminate = chosen.length > 0 && !allChosen;
                }}
                aria-label={t("contentManager.selectPage")}
                onChange={(e) => setSelected(e.target.checked ? new Set(items.map((c) => c.id)) : new Set())}
              />
            )}
            {canSelect && chosen.length > 0 ? (
              <div className="flex flex-1 flex-wrap items-center gap-2" data-ckui="bulk-bar">
                <span className="me-2 text-sm font-medium text-foreground">{t("contentManager.selected", { count: chosen.length })}</span>
                {deleted ? (
                  <Button size="xs" variant="outline" disabled={bulk.pending} onClick={() => void runBulk(chosen, restore)}>
                    {t("moderation.restore")}
                  </Button>
                ) : (
                  <>
                    {hasHiddenVersion && <Button size="xs" variant="destructive" disabled={bulk.pending} onClick={() => void bulkDelete()}>
                      {t("contentManager.delete")}
                    </Button>}
                  </>
                )}
                <Button size="xs" variant="ghost" className="ms-auto" disabled={bulk.pending} onClick={() => setSelected(new Set())}>
                  {t("contentManager.clearSelection")}
                </Button>
              </div>
            ) : (
              <>
                {canSelect && <span className="md:hidden">{t("contentManager.selectPage")}</span>}
                <span className="hidden md:block">{t("contentManager.itemTitle")}</span>
                <span className="hidden md:block">{t("contentManager.status")}</span>
                {showEditionAccess && <span className="hidden md:block">{t("contentManager.access")}</span>}
                <span className="hidden md:block">{t("contentManager.date")}</span>
                <span className="hidden md:block" />
              </>
            )}
          </li>
          {items.map((c) => {
            const s = contentStatus(c);
            const href = s === "published" ? contentHref?.(c) : undefined;
            const editionRule = selectedEdition(c)?.access;
            const isPremium = editionRule?.level === "tier";
            const more = href || hasHiddenVersion;
            return (
              <li
                key={c.id}
                className="flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-border px-3 py-2.5 transition-colors last:border-b-0 hover:bg-muted/30 has-checked:bg-muted/40 md:grid md:grid-cols-(--ck-cols) md:gap-4"
                data-ckui="content-row"
                data-status={deleted ? "deleted" : s}
              >
                {canSelect && (
                  <input
                    type="checkbox"
                    className="size-4 shrink-0 accent-primary"
                    checked={selected.has(c.id)}
                    aria-label={t("contentManager.select", { title: c.title })}
                    onChange={(e) => toggle(c, e.target.checked)}
                  />
                )}
                <div className="flex min-w-[min(100%,16rem)] flex-1 items-center gap-3">
                  <RowCover url={c.cover?.url} portrait={presentation === "reader"} />
                  <div className="grid min-w-0 gap-0.5">
                    {deleted && hasHiddenVersion ? (
                      <span className="truncate font-medium text-muted-foreground line-through">{c.title}</span>
                    ) : (
                      <button type="button" className="truncate text-start font-medium underline-offset-4 hover:underline" title={c.title} onClick={() => open(c)}>
                        {c.title}
                      </button>
                    )}
                    <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                      <span className="font-mono">{c.code}</span>
                      <span aria-hidden>·</span>
                      <span className="uppercase">{c.language}</span>
                    </div>
                  </div>
                </div>
                <div className="text-sm">{deleted ? <span className="text-muted-foreground">{t("contentManager.deleted")}</span> : <StatusDot status={s} />}</div>
                {showEditionAccess && (
                  <div className={cn("text-sm", isPremium ? "font-medium text-foreground" : "text-muted-foreground")} data-ckui="access-badge">
                    {editionRule ? accessLabel(t, editionRule, labels) : "—"}
                  </div>
                )}
                <div className="grid text-sm text-muted-foreground">
                  {c.published_at && s === "published" && !deleted ? (
                    <time dateTime={c.published_at} title={formatDate(m, c.published_at)}>
                      {t("contentManager.liveOn", { date: formatDate(m, c.published_at, false) })}
                    </time>
                  ) : (
                    <RelativeTime at={deleted && c.deleted_at ? c.deleted_at : c.updated_at} />
                  )}
                  {s === "scheduled" && c.publish_at && <span className="text-xs">{t("pollManager.goesLive", { date: formatDate(m, c.publish_at) })}</span>}
                </div>
                <div className="ms-auto flex items-center justify-end gap-1">
                  {deleted ? hasHiddenVersion ? (
                    <Button size="sm" variant="outline" disabled={del.pending} onClick={() => void del.run(() => restore(c))}>
                      {t("moderation.restore")}
                    </Button>
                  ) : (
                    <Button size="sm" variant="outline" onClick={() => open(c)}>{t("contentManager.manageVersions")}</Button>
                  ) : (
                    <>
                      <Button size="sm" variant="outline" onClick={() => open(c)}>
                        {t("comments.edit")}
                      </Button>
                      {more ? (
                        <DropdownMenu>
                          <DropdownMenuTrigger
                            render={<Button size="icon-sm" variant="ghost" aria-label={t("contentManager.moreActions", { title: c.title })} />}
                          >
                            <HugeiconsIcon icon={MoreHorizontalIcon} strokeWidth={2} />
                          </DropdownMenuTrigger>
                          <DropdownMenuContent align="end" className="min-w-40">
                            {href && (
                              <DropdownMenuItem render={<a href={href} />}>
                                <HugeiconsIcon icon={ViewIcon} strokeWidth={2} />
                                {t("contentManager.view")}
                              </DropdownMenuItem>
                            )}
                            {hasHiddenVersion && href && <DropdownMenuSeparator />}
                            {hasHiddenVersion && (
                              <DropdownMenuItem className="text-destructive focus:text-destructive" disabled={del.pending} onClick={() => void remove(c)}>
                                <HugeiconsIcon icon={Delete02Icon} strokeWidth={2} />
                                {t("comments.delete")}
                              </DropdownMenuItem>
                            )}
                          </DropdownMenuContent>
                        </DropdownMenu>
                      ) : (
                        <span aria-hidden className="size-8" />
                      )}
                    </>
                  )}
                </div>
              </li>
            );
          })}
        </ul>
      )}
      <Pager
        page={page}
        hasNext={listing.hasNext}
        disabled={loading}
        total={total}
        truncated={counted.data?.truncated}
        pageSize={size}
        pageSizes={PAGE_SIZES}
        onPageSize={(n) => loc.go("", { ...filters, size: n !== defaultSize ? n : undefined, page: undefined })}
        onPage={(n) => loc.go("", { ...filters, page: n > 1 ? n : undefined })}
      />
      {dialog}
    </div>
  );
}

const dot: Record<ContentStatus, string> = { published: "bg-success", scheduled: "bg-sky-500", publishing: "bg-sky-500", draft: "bg-muted-foreground/50" };

/** A status as a coloured dot and its name. */
function StatusDot({ status }: { status: ContentStatus }) {
  const { t } = useMessages();
  return (
    <span className="inline-flex items-center gap-2 whitespace-nowrap text-foreground font-medium">
      <span aria-hidden className={cn("size-2 shrink-0 rounded-full", dot[status])} />
      {t(`contentManager.${status}`)}
    </span>
  );
}

/** A row's cover, portrait for reader works and wide for the rest, or a quiet placeholder. */
function RowCover({ url, portrait }: { url?: string; portrait?: boolean }) {
  const box = cn("shrink-0 rounded-md bg-muted ring-1 ring-foreground/5", portrait ? "aspect-[7/10] w-11" : "aspect-video w-24");
  return url ? (
    <img src={url} alt="" loading="lazy" className={cn(box, "object-cover")} />
  ) : (
    <span aria-hidden className={cn(box, "grid place-items-center text-muted-foreground")}>
      <HugeiconsIcon icon={Image01Icon} strokeWidth={1.5} className="size-5" />
    </span>
  );
}

function Spinner() {
  return <span aria-hidden className="size-3 animate-spin rounded-full border-2 border-current border-t-transparent" />;
}

interface Form {
  title: string;
  description: string;
  publishAt: string;
  body: string;
}

const empty: Form = { title: "", description: "", publishAt: "", body: "" };

export interface ContentEditorProps {
  /** Staff entity lookup, including internal classifications. */
  management?: boolean;
  /** The item edited; null creates one of kind. */
  id: string | null;
  kind: string;
  /** Languages to write in: codes, or codes with labels; the first is a new item's. */
  languages?: readonly Choice[];
  /** The body editor; without it, a plain HTML field. */
  renderEditor?: (props: BodyEditorProps) => ReactNode;
  /** A link to a live item's page. */
  contentHref?: (content: Content) => string | undefined;
  /** A reader edition code from the route; omitted selects its first edition. Other presentations do not accept nested edition routes. */
  versionCode?: string;
  /** An edition's stable editor URL, when the host provides routing. */
  versionHref?: (version: Version) => string | undefined;
  /** Navigate to an edition, or the parent when none remain; replace canonicalizes or retires an edition URL. */
  onVersionChange?: (version: Version | null, options?: { replace?: boolean }) => void;
  /** Registers the editor's confirmation before the enclosing flow leaves it. */
  onLeaveGuard?: (guard: (() => Promise<boolean>) | undefined) => void;
  labels?: Labels;
  /** A new item's owner group: a channel its editors write for. */
  ownerEntity?: string;
  /** Entities a new item starts linked to, e.g. the series it was created from; ignored for an existing item. */
  entities?: readonly EntityCard[];
  /** After create: show the new item (the editor goes on editing it). */
  onCreated?: (content: Content) => void;
  onDeleted?: (content: Content) => void;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  /** The kind's name, localized by the host (e.g. video, videos). */
  noun?: { one: string; other: string };
}

/**
 * One content item for its creators and staff: title, listing summary for
 * posts (description for other kinds),
 * schedule, cover and body per language, publish, unpublish and delete;
 * then listing and attributes, access rule and price, versions and groups.
 */
export function ContentEditor(p:ContentEditorProps){
  const {client,scope}=useContentScope(p.client);
  const allocated=useRef<{id:string;session:number}|null>(null);
  const [session,setSession]=useState({id:p.id,kind:p.kind,client,scope,versionCode:p.versionCode,key:0});
  let boundary=session;
  if(session.id!==p.id||session.kind!==p.kind||session.client!==client||session.scope!==scope||session.versionCode!==p.versionCode){
    const continues=session.id==null&&p.id===allocated.current?.id&&allocated.current?.session===session.key&&session.kind===p.kind&&session.client===client&&session.scope===scope;
    const sameItem=session.id===p.id&&session.kind===p.kind&&session.client===client&&session.scope===scope;
    const versionChanged=session.versionCode!==undefined&&session.versionCode!==p.versionCode;
    boundary={id:p.id,kind:p.kind,client,scope,versionCode:p.versionCode,key:(continues||sameItem)&&!versionChanged?session.key:session.key+1};
    setSession(boundary);
  }
  const {kinds,error,reload}=useKinds({client});
  const {t}=useMessages();
  if(!kinds)return error?<LoadFailed error={error} message={t("contentManager.loadFailed")} retry={reload}/>:<Rows/>;
  const props={...p,client,onCreated:(content:Content)=>{allocated.current={id:content.id,session:boundary.key};p.onCreated?.(content);}};
  if(kinds.kinds[p.kind]?.presentation==="reader")return <ReaderContentEditor key={boundary.key} {...props} model={kinds}/>;
  if(p.versionCode!==undefined)return <NoAccess>{t("contentManager.notFound")}</NoAccess>;
  return <StandardContentEditor key={boundary.key} {...props}/>;
}

function StandardContentEditor({ id, client: own, kind, languages: offered, renderEditor, contentHref, labels, ownerEntity, entities, onCreated, onDeleted, onError, management, noun }: ContentEditorProps) {
  const client = useContentKitClient(own);
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const languageName = useLanguageName();
  const languages = choices(offered, languageName);
  const [language, setLanguage] = useState(languages[0]?.value ?? "en");
  const ed = useContent(id, kind, language, { client });
  // A hidden version's cover is edited here; a visible version's in its own form.
  const model = useKinds({ client }).kinds;
  const hidden = !!model?.kinds[kind]?.versions.hidden;
  const post = model?.kinds[kind]?.presentation === "post";
  const report = useErrorReporter(onError);
  // The body is kept as stored (image references); the editor shows it with URLs.
  const [form, setForm] = useState<Form | null>(id ? null : empty);
  const [error, setError] = useState<ContentKitError | string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const { confirm, dialog } = useConfirm();
  const item = ed.content;
  const versionChoices = useMemo(
    () => (item?.versions ?? []).map((v) => ({ id: v.id, label: versionLabel(v, languageName, labels) })),
    [item?.versions, languageName, labels],
  );
  useEffect(() => {
    if (!item) return;
    const stored = item.text?.[language];
    setForm((f) => f ?? { title: stored?.title ?? "", description: stored?.description ?? "", body: stored?.body ?? "",
      publishAt: item.publish_at && !item.live ? localInput(new Date(item.publish_at)) : "" });
  }, [item, language]);
  const { imageSrc } = ed;
  const shown = useMemo(
    () => (form?.body ?? "").replace(imageRefs(), (ref, name: string) => {
      const src = imageSrc(name);
      return src && src !== name ? src.replaceAll("&", "&amp;") : ref;
    }),
    [form?.body, imageSrc],
  );
  if (id && !form) {
    if (ed.error) return <LoadFailed error={ed.error} message={t("contentManager.loadFailed")} retry={ed.reload} />;
    return (
      <div className="grid gap-3" aria-busy>
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }
  const f = form!;
  const set = (patch: Partial<Form>) => setForm((cur) => ({ ...cur!, ...patch }));
  const fail = (e: unknown) => {
    const err = toContentKitError(e);
    setError(err);
    report(err, "content.save");
  };
  const text = () => ({ title: f.title.trim(), description: f.description.trim(), body: f.body });
  const stored = item?.text?.[language];
  const dirty = f.title.trim() !== (stored?.title ?? "") || f.description.trim() !== (stored?.description ?? "") || f.body !== (stored?.body ?? "");
  // The server normalizes what it stores, so the form takes the stored text back.
  const synced = (c: Content) => {
    const saved = c.text?.[language];
    set({ title: saved?.title ?? "", description: saved?.description ?? "", body: saved?.body ?? "" });
  };
  const save = async () => {
    setError(null);
    setNotice(null);
    if (!f.title.trim()) return setError(t("contentManager.titleRequired"));
    try {
      if (!item) {
        const created = await ed.create({ kind, text: { [language]: text() }, owner_group_id: model?.kinds[kind]?.owner_entity ? undefined : ownerEntity });
        // Starting entities link once the item exists; version-level kinds need a version first.
        const linked = (entities ?? []).filter((g) => model?.kinds[kind]?.entities?.includes(g.kind) && allowsEntityScope(model.entity_kinds[g.kind], "content"));
        if (linked.length) {
          await client.entities
            .setOfContent(created.id, { kinds: [...new Set(linked.map((g) => g.kind))], memberships: linked.map((g) => ({ group_id: g.id })) }, language)
            .catch(fail);
        }
        onCreated?.(created);
        return;
      }
      synced(await ed.putText(text()));
      setNotice(t("pollEditor.saved"));
    } catch (e) {
      fail(e);
    }
  };
  const publish = async (live: boolean) => {
    setError(null);
    setNotice(null);
    if (live && dirty && !f.title.trim()) return setError(t("contentManager.titleRequired"));
    try {
      // Publishing makes the stored text public, so unsaved edits are saved first.
      if (live && dirty) synced(await ed.putText(text()));
      if (live) await ed.publish(f.publishAt ? new Date(f.publishAt).toISOString() : undefined);
      else await ed.unpublish();
      setNotice(t(live ? "contentManager.publishedNotice" : "contentManager.unpublishedNotice"));
    } catch (e) {
      fail(e);
    }
  };
  const remove = async () => {
    if (!item || !(await confirm({ title: t("contentManager.deleteTitle", { title: item.title }), description: t("contentManager.deleteBody"), confirm: t("contentManager.delete") }))) return;
    try {
      await ed.removeVersion();
      onDeleted?.(item);
    } catch (e) {
      fail(e);
    }
  };
  const s = item ? contentStatus(item) : "draft";
  const href = item && s === "published" ? contentHref?.(item) : undefined;
  const editor: BodyEditorProps = {
    value: shown,
    onChange: (html) => set({ body: ed.storedBody(html) }),
    uploadImage: item ? (file) => ed.uploadImage(file) : undefined,
    disabled: ed.saving,
    placeholder: t("contentManager.bodyPlaceholder"),
    "aria-label": t("contentManager.body"),
  };

  const storedTitle = stored?.title?.trim() || item?.title;
  const newHeading = noun?.one ? (
    <span className="first-letter:uppercase">{t("contentManager.newNamed", { one: noun.one })}</span>
  ) : (
    t("contentManager.newTitle")
  );
  const headingTitle = item ? (storedTitle || t("contentManager.editTitle")) : newHeading;

  return (
    <>
      <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_20rem] items-start">
        <div className="grid gap-6 min-w-0">
          <form
            className="grid gap-6"
            data-ckui="content-form"
            onSubmit={(e) => {
              e.preventDefault();
              void save();
            }}
          >
            {/* Header */}
            <header className="grid gap-2" data-ckui="content-head">
              <div className="flex flex-wrap items-start justify-between gap-3">
                <h2 className="min-w-0 truncate text-xl font-semibold tracking-tight" title={typeof storedTitle === "string" && storedTitle ? storedTitle : undefined}>
                  {headingTitle}
                </h2>
                <div className="flex flex-wrap items-center gap-2">
                  {!item && (
                    <Button
                      type="submit"
                      size="sm"
                      variant="default"
                      disabled={ed.saving}
                    >
                      {ed.saving ? (
                        <span className="inline-flex items-center gap-1.5">
                          <Spinner />
                          <span>{t("common.saving")}</span>
                        </span>
                      ) : (
                        t("contentManager.createDraft")
                      )}
                    </Button>
                  )}
                  {item && !item.live && (
                    <>
                      <Button
                        type="submit"
                        size="sm"
                        variant="outline"
                        disabled={ed.saving || item.state === "deleted"}
                      >
                        {ed.saving ? (
                          <span className="inline-flex items-center gap-1.5">
                            <Spinner />
                            <span>{t("common.saving")}</span>
                          </span>
                        ) : (
                          t("common.save")
                        )}
                      </Button>
                      {item.state !== "deleted" && (
                        <Button
                          type="button"
                          size="sm"
                          variant="default"
                          disabled={ed.saving}
                          onClick={() => void publish(true)}
                        >
                          {t("contentManager.publish")}
                        </Button>
                      )}
                    </>
                  )}
                  {item && item.live && (
                    <>
                      <Button
                        type="submit"
                        size="sm"
                        variant={dirty ? "default" : "outline"}
                        disabled={ed.saving || item.state === "deleted"}
                      >
                        {ed.saving ? (
                          <span className="inline-flex items-center gap-1.5">
                            <Spinner />
                            <span>{t("common.saving")}</span>
                          </span>
                        ) : (
                          t("common.save")
                        )}
                      </Button>
                      <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        disabled={ed.saving}
                        onClick={() => void publish(false)}
                      >
                        {t("contentManager.unpublish")}
                      </Button>
                    </>
                  )}
                  {item && hidden && (
                    <DropdownMenu>
                      <DropdownMenuTrigger
                        render={<Button size="icon-sm" variant="ghost" disabled={ed.saving} aria-label={t("contentManager.moreActions", { title: storedTitle || t("contentManager.itemTitle") })} />}
                      >
                        <HugeiconsIcon icon={MoreHorizontalIcon} strokeWidth={2} />
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end" className="min-w-40">
                        <DropdownMenuItem
                          className="text-destructive focus:text-destructive"
                          onClick={() => void remove()}
                        >
                          <HugeiconsIcon icon={Delete02Icon} strokeWidth={2} data-icon="inline-start" />
                          {t("contentManager.delete")}
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  )}
                </div>
              </div>
              {item && (
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
                  <StatusDot status={s} />
                  {item.live && item.published_at && (
                    <>
                      <span aria-hidden>·</span>
                      <span>{t("pollManager.wentLive", { date: formatDate(m, item.published_at, false) })}</span>
                    </>
                  )}
                  <span aria-hidden>·</span>
                  <span className="font-mono text-xs">{item.code}</span>
                  {languages.length > 1 && (
                    <>
                      <span aria-hidden>·</span>
                      <div className="inline-flex items-center">
                        <Label htmlFor={`${ids}-lang`} className="sr-only">
                          {t("contentManager.language")}
                        </Label>
                        <Select
                          value={language}
                          onValueChange={async (v) => {
                            if (dirty && !(await confirm({ title: t("contentManager.switchLanguage"), description: t("contentManager.discardLanguageEdits"), confirm: t("readerEditor.discard") }))) return;
                            setLanguage(String(v ?? ""));
                            setForm(item ? null : empty);
                          }}
                        >
                          <SelectTrigger id={`${ids}-lang`} size="sm">
                            <SelectValue>{(v: string) => languages.find((l) => l.value === v)?.label ?? v}</SelectValue>
                          </SelectTrigger>
                          <SelectContent>
                            {languages.map((l) => (
                              <SelectItem key={l.value} value={l.value}>
                                {l.label}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      </div>
                    </>
                  )}
                  {href && (
                    <>
                      <span aria-hidden>·</span>
                      <a
                        className={buttonVariants({ size: "sm", variant: "ghost" })}
                        href={href}
                        target="_blank"
                        rel="noreferrer"
                      >
                        <HugeiconsIcon icon={ViewIcon} strokeWidth={2} data-icon="inline-start" />
                        <span>{t("contentManager.view")}</span>
                        <HugeiconsIcon icon={LinkSquare02Icon} strokeWidth={2} data-icon="inline-end" />
                      </a>
                    </>
                  )}
                </div>
              )}
            </header>

            {/* Title and description */}
            <div className="rounded-lg border border-border bg-muted/30 p-4 grid gap-4">
              <h3 className="text-sm font-medium">
                {t("contentManager.titleAndDescription")}
              </h3>
              <div className="grid gap-4">
                <div className="grid gap-1.5">
                  <Label htmlFor={`${ids}-title`}>
                    {t("contentManager.itemTitle")}
                  </Label>
                  <Input
                    id={`${ids}-title`}
                    value={f.title}
                    required
                    maxLength={300}
                    onChange={(e) => set({ title: e.target.value })}
                  />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor={`${ids}-description`}>
                    {t(post ? "contentManager.postSummary" : "contentManager.description")}
                  </Label>
                  <Textarea
                    id={`${ids}-description`}
                    aria-describedby={post ? `${ids}-summary-help` : undefined}
                    rows={3}
                    value={f.description}
                    onChange={(e) => set({ description: e.target.value })}
                  />
                  {post && (
                    <p id={`${ids}-summary-help`} className="text-xs text-muted-foreground">
                      {t("contentManager.postSummaryHelp")}
                    </p>
                  )}
                </div>
                {!item?.live && (
                  <div className="grid gap-1.5">
                    <Label htmlFor={`${ids}-publish`}>
                      {t("contentManager.publishAt")}
                    </Label>
                    <Input
                      id={`${ids}-publish`}
                      type="datetime-local"
                      className="w-64 max-w-full"
                      value={f.publishAt}
                      onChange={(e) => set({ publishAt: e.target.value })}
                    />
                  </div>
                )}
              </div>
            </div>

            {/* Media / Cover */}
            {(hidden || !item) && (
              <Cover item={item} src={ed.coverSrc} saving={ed.saving} onFile={(file) => void ed.setCover(file).catch(fail)} />
            )}

            {/* Body */}
            <div className="rounded-lg border border-border bg-muted/30 p-4 grid gap-4">
              <div className="grid gap-1">
                <h3 className="text-sm font-medium">
                  {t("contentManager.body")}
                </h3>
                {!item && (
                  <p className="text-xs text-muted-foreground">
                    {t("contentManager.imagesAfterCreate")}
                  </p>
                )}
              </div>
              <div>
                {renderEditor ? (
                  renderEditor(editor)
                ) : (
                  <Textarea
                    aria-label={editor["aria-label"]}
                    rows={12}
                    value={f.body}
                    placeholder={editor.placeholder}
                    onChange={(e) => set({ body: e.target.value })}
                    className="w-full resize-y border-0 bg-transparent p-0 font-sans text-sm leading-relaxed placeholder:text-muted-foreground/50 focus-visible:ring-0"
                  />
                )}
              </div>
            </div>

            {error && <ErrorLine>{typeof error === "string" ? error : contentError(m, error)}</ErrorLine>}
            {notice && !error && (
              <p role="status" className="rounded-md bg-success/15 px-3 py-2 text-sm text-foreground ring-1 ring-inset ring-success/45">
                {notice}
              </p>
            )}
          </form>

          {/* Versions Container (outside form for HTML5 valid nesting) */}
          {item && (
            <div data-ckui="content-versions">
              <VersionsEditor content={item} client={client} onError={onError} labels={labels} />
            </div>
          )}
        </div>

        {/* Aside Column */}
        <aside className="grid gap-6">
          {item && <ContentDetailsEditor content={item} client={client} onError={onError} labels={labels} />}
          {item && offersAccessChoice(model?.kinds[kind]) && (
            <AccessEditor content={item} client={client} onError={onError} labels={labels} />
          )}
          {item && !!model?.kinds[kind]?.entities?.length && (
            <section data-ckui="content-groups">
              <MembershipEditor
                management={management}
                content={item.id}
                contentKind={item.kind}
                versions={model?.kinds[kind]?.versions.hidden ? undefined : versionChoices}
                labels={labels}
                client={client}
                onError={onError}
              />
            </section>
          )}
        </aside>
      </div>
      {dialog}
    </>
  );
}

function Cover({ item, src, saving, onFile }: { item: Content | null; src?: string; saving: boolean; onFile: (file: File | null) => void }) {
  const { t } = useMessages();
  const file = useRef<HTMLInputElement>(null);
  return (
    <div className="rounded-lg border border-border bg-muted/30 p-4 grid gap-4" data-ckui="content-cover">
      <h3 className="text-sm font-medium">{t("contentManager.cover")}</h3>
      {item ? (
        <div className="flex flex-wrap items-center gap-4">
          {src ? (
            <div className="relative group overflow-hidden rounded-md border border-border">
              <img src={src} alt="" className="aspect-video w-64 max-w-full object-cover" />
            </div>
          ) : (
            <div className="aspect-video w-64 max-w-full rounded-md border border-dashed border-border bg-muted flex flex-col items-center justify-center text-xs text-muted-foreground gap-1.5 p-4 text-center">
              <HugeiconsIcon icon={Image01Icon} className="size-6 text-muted-foreground/50" />
              <span>{t("contentManager.noCover")}</span>
            </div>
          )}
          <div className="flex flex-col gap-2">
            <Button type="button" size="sm" variant="outline" disabled={saving} onClick={() => file.current?.click()}>
              {t(src ? "pollEditor.replaceImage" : "contentManager.addCover")}
            </Button>
            {src && (
              <Button type="button" size="sm" variant="ghost" disabled={saving} onClick={() => onFile(null)} className="text-destructive hover:text-destructive">
                {t("pollEditor.removeImage")}
              </Button>
            )}
          </div>
          <input
            ref={file}
            type="file"
            accept="image/*"
            hidden
            aria-label={t("contentManager.cover")}
            onChange={(e) => {
              const f = e.target.files?.[0];
              e.target.value = "";
              if (f) onFile(f);
            }}
          />
        </div>
      ) : (
        <p className="text-xs text-muted-foreground">{t("contentManager.imagesAfterCreate")}</p>
      )}
    </div>
  );
}
