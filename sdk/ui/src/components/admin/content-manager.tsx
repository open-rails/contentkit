import { Image01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { useCallback, useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import { imageRefs } from "../../client/contents.js";
import { allowsEntityScope } from "../../client/entity-scopes.js";
import type { Content, GroupCard, GroupDetail, ManageCount, Rule, Version } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import type { Translator } from "../../i18n/messages.js";
import { useAdminLocation, useFlowClient, useStaffAccess, type AdminLocation, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { useContentKitClient, useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { useContent } from "../../react/contents.js";
import { useKinds } from "../../react/groups.js";
import { keyOf, useContentScope, usePaged, useResource } from "../../react/use-resource.js";
import { Alert, AlertDescription } from "#ckui/ui/alert";
import { Badge } from "#ckui/ui/badge";
import { Button, buttonVariants } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { Textarea } from "#ckui/ui/textarea";
import { localInput } from "../social/bans.js";
import { contentError, ErrorLine, formatDate, RelativeTime, useAction } from "../social/parts.js";
import { ReaderContentEditor } from "./reader-content-editor.js";
import { createDraft, uploadTarget } from "./bulk-drafts.js";
import { UploadQueueBody } from "./upload-queue.js";
import { ContentSections, allowsIndividualSales } from "../content/editors.js";
import { labelOf, type Labels } from "../groups/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Pager, Rows, pageOf, useConfirm, type AdminFlowProps, type AdminRouteProps } from "./shell.js";

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
  /** Row checkboxes with bulk access, delete and restore (default true). */
  selectable?: boolean;
  /** The kind's name, localized by the host and as it reads mid-sentence (e.g. gallery, galleries): the list heading, New button and empty state. */
  noun?: { one: string; other: string };
}

/** Whether the caller manages content: root:content:write holders, creators and owner-group editors. */
export function useContentManagerAccess(o: FlowClientOptions = {}): StaffAccess<"contents"> {
  return useStaffAccess(["contents"] as const, o);
}

export type ContentStatus = "draft" | "scheduled" | "published" | "held" | "rejected";

/** An item's status as the manager names it. */
export function contentStatus(c: Content): ContentStatus {
  if (c.moderation === "rejected") return "rejected";
  if (c.moderation === "held") return "held";
  if (c.state === "draft") return "draft";
  if (!c.live) return "scheduled";
  return "published";
}

const choices = (list?: readonly Choice[]) => (list ?? []).map((c) => (typeof c === "string" ? { value: c, label: c } : c));
const badge: Record<ContentStatus, "default" | "secondary" | "outline" | "destructive"> = { published: "default", scheduled: "secondary", draft: "outline", held: "secondary", rejected: "destructive" };
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
      <Button variant="ghost" size="sm" className="justify-self-start" onClick={() => void (async () => {
        if ((await leaveGuard.current?.()) ?? true) loc.go("");
      })()}>
        {t("contentManager.back")}
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

const asCard = (g: GroupDetail): GroupCard => ({ ...g, count: 0 });

/** A new item's starting entities from `?with=CODE[,CODE]`, each with its scope (a character's series); unresolvable codes are skipped. */
function useStartingEntities(client: ContentKitClient, codes: string): readonly GroupCard[] | undefined {
  const [found, setFound] = useState<{ codes: string; entities: GroupCard[] } | null>(null);
  useEffect(() => {
    if (!codes) return;
    const ctl = new AbortController();
    const one = async (code: string) => {
      const r = await client.codes.resolve(code, { signal: ctl.signal });
      const g = await client.entities.getForManagement(r.content_id, undefined, ctl.signal);
      return g.scope ? [asCard(g), g.scope] : [asCard(g)];
    };
    void Promise.all(codes.split(",").filter(Boolean).map((c) => one(c).catch(() => [] as GroupCard[]))).then((all) => {
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

// Filter values: "" every rule, a level, or "tier:{name}" for one tier.
const accessQuery = (v: string): { access?: string; tier?: string } =>
  !v ? {} : v.startsWith("tier:") ? { access: "tier", tier: v.slice(5) } : { access: v };

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
  const accessFilter = spec?.access_scope === "version" ? "" : loc.query.get("access") ?? "";
  const deleted = loc.query.get("deleted") === "1";
  const page = pageOf(loc.query);
  const sizeParam = Number(loc.query.get("size"));
  const size = PAGE_SIZES.find((n) => n === sizeParam) ?? defaultSize;
  const filters = { q: query, status, access: accessFilter || undefined, deleted: deleted ? "1" : undefined, size: size !== defaultSize ? size : undefined };
  const filter = (next: Partial<typeof filters>) => loc.go("", { ...filters, ...next });
  const filtered = !!(query || status || accessFilter);
  const [text, setText] = useState(query);
  useEffect(() => setText(query), [query]);
  // Searches as the admin types; Enter applies at once.
  useEffect(() => {
    if (text.trim() === query) return;
    const id = setTimeout(() => filter({ q: text.trim() }), 300);
    return () => clearTimeout(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [text]);
  const tiers = model?.tiers ?? [];
  // Reader works take one ZIP per draft, players one video per draft.
  const presentation = spec?.presentation;
  const canDeleteParent = !!spec?.versions.hidden;
  const canChangeParentAccess = !!spec && spec.access_scope !== "version";
  const canSelect = selectable && (deleted ? canDeleteParent : canDeleteParent || (canChangeParentAccess && tiers.length > 0));
  const bulkable = presentation === "reader" || presentation === "player";
  const [bulkOpen, setBulkOpen] = useState(false);
  const drafts = useRef(new Map<string, Content>()).current;
  const report = useErrorReporter(onError);
  const createItem = async (file: File) => {
    const { content, ref } = await createDraft(client, kind, language ?? model?.languages[0] ?? "en", file, t("readerEditor.untitled"));
    drafts.set(content.id, content);
    return ref;
  };
  const openDraft = (ref: { id: string }) => {
    const known = drafts.get(ref.id);
    if (known) return open(known);
    // Resumed after a reload: the draft is no longer in memory.
    void client.contents.manage({ kind, ids: [ref.id], limit: 1 }).then(
      ([c]) => {
        if (c) open(c);
      },
      (e) => report(e, "upload"),
    );
  };
  const { store, scope } = useContentScope(client);
  const state = deleted ? "deleted" : status || undefined;
  const tag = { type: "contents", scope: deleted ? "deleted" : undefined } as const;
  const filterQuery = { q: query || undefined, state, ...accessQuery(accessFilter) };
  const listing = usePaged(store, keyOf("contents-page", scope, kind, query, state, accessFilter, language, size), tag, page, size, (q, signal) =>
    client.contents.manage({ ...q, ...filterQuery, kind, language }, signal),
  );
  const counted = useResource<ManageCount>(store, keyOf("contents-count", scope, kind, query, state, accessFilter), tag, (signal) =>
    client.contents.manageCount({ ...filterQuery, kind }, signal),
  );
  const items = listing.items;
  const total = counted.data?.total;
  const loading = listing.loading;
  const del = useAction("content.save", onError);
  const { confirm, dialog } = useConfirm();
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  useEffect(() => setSelected(new Set()), [kind, query, state, accessFilter, page, size]);
  const [busy, setBusy] = useState<ReadonlySet<string>>(new Set());
  const [rowErrors, setRowErrors] = useState<Readonly<Record<string, ContentKitError>>>({});
  const [bulk, setBulk] = useState<{ pending: boolean; summary?: string }>({ pending: false });
  const premium = tiers[0];
  const toggleable = (c: Content) => canChangeParentAccess && !!premium && (c.access.level === "public" || c.access.level === "tier");
  const nextRule = (c: Content, toPremium: boolean): Rule => (toPremium ? { level: "tier", tiers: [premium!] } : { level: "public" });
  const setAccess = async (c: Content, toPremium: boolean) => {
    setBusy((b) => new Set(b).add(c.id));
    setRowErrors(({ [c.id]: _, ...rest }) => rest);
    try {
      await client.contents.update(c.id, { access: nextRule(c, toPremium) });
    } catch (e) {
      const err = toContentKitError(e);
      setRowErrors((r) => ({ ...r, [c.id]: err }));
    } finally {
      setBusy((b) => {
        const n = new Set(b);
        n.delete(c.id);
        return n;
      });
    }
  };
  const remove = async (c: Content) => {
    if (await confirm({ title: t("contentManager.deleteTitle", { title: c.title }), description: t("contentManager.deleteBody"), confirm: t("contentManager.delete") })) {
      await del.run(() => client.contents.remove(c.id));
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
      await runBulk(chosen, (c) => client.contents.remove(c.id));
    }
  };
  const allChosen = items.length > 0 && chosen.length === items.length;
  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold first-letter:uppercase">{noun?.other ?? t("contentManager.title")}</h2>
        <div className="flex gap-2">
          {bulkable && !deleted && (
            <Button size="sm" variant="outline" aria-expanded={bulkOpen} onClick={() => setBulkOpen((o) => !o)}>
              {t("contentManager.bulkUpload")}
            </Button>
          )}
          <Button size="sm" onClick={() => loc.go("new")}>
            {noun ? t("contentManager.newNamed", { one: noun.one }) : t("contentManager.new")}
          </Button>
        </div>
      </div>
      {bulkable && bulkOpen && !deleted && (
        <section className="grid gap-3 rounded-lg border border-border p-3" aria-label={t("contentManager.bulkUpload")} data-ckui="bulk-upload">
          <p className="text-sm text-muted-foreground">{t(presentation === "reader" ? "contentManager.bulkHintArchive" : "contentManager.bulkHintFile")}</p>
          <UploadQueueBody
            client={client}
            createItem={createItem}
            target={(ref, file, signal) => uploadTarget(client, ref, file, signal)}
            accept={presentation === "reader" ? ".zip,application/zip" : "video/*"}
            concurrency={2}
            storageKey={`ckui.bulk.${kind}`}
            onOpen={openDraft}
            onError={onError}
          />
        </section>
      )}
      <div className="flex gap-2" role="group" aria-label={noun?.other ?? t("contentManager.title")}>
        <Button size="sm" variant={deleted ? "outline" : "default"} aria-pressed={!deleted} onClick={() => loc.go("", { q: query })}>
          {t("contentManager.items")}
        </Button>
        <Button size="sm" variant={deleted ? "default" : "outline"} aria-pressed={deleted} onClick={() => loc.go("", { q: query, deleted: "1" })}>
          {t("contentManager.deleted")}
        </Button>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <form
          className="min-w-48 flex-1"
          role="search"
          onSubmit={(e) => {
            e.preventDefault();
            filter({ q: text.trim() });
          }}
        >
          <Label htmlFor={`${ids}-q`} className="sr-only">
            {t("contentManager.search")}
          </Label>
          <Input id={`${ids}-q`} className="h-8" type="search" value={text} placeholder={t("contentManager.search")} onChange={(e) => setText(e.target.value)} />
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
        {canChangeParentAccess && <Select value={accessFilter} onValueChange={(v) => filter({ access: String(v ?? "") || undefined })}>
          <SelectTrigger size="sm" aria-label={t("contentManager.access")}>
            <SelectValue>
              {(v: string) =>
                !v
                  ? t("contentManager.anyAccess")
                  : v.startsWith("tier:")
                    ? labelOf(labels, v.slice(5))
                    : accessLabel(t, { level: v as Rule["level"] }, labels)
              }
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="">{t("contentManager.anyAccess")}</SelectItem>
            <SelectItem value="public">{t("contentManager.free")}</SelectItem>
            {tiers.length > 0 && <SelectItem value="tier">{t("contentManager.premium")}</SelectItem>}
            {tiers.length > 1 &&
              tiers.map((x) => (
                <SelectItem key={x} value={`tier:${x}`}>
                  {labelOf(labels, x)}
                </SelectItem>
              ))}
            {allowsIndividualSales(model, kind) && <SelectItem value="ppv">{t("contentManager.ppv")}</SelectItem>}
          </SelectContent>
        </Select>}
      </div>
      <ErrorLine error={del.error} />
      {bulk.summary && (
        <p role="status" className="text-sm text-muted-foreground">
          {bulk.summary}
        </p>
      )}
      {canSelect && chosen.length > 0 && (
        <div className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-muted/40 p-2 text-sm" data-ckui="bulk-bar">
          <span className="font-medium">{t("contentManager.selected", { count: chosen.length })}</span>
          {deleted ? (
            <Button size="sm" variant="outline" disabled={bulk.pending} onClick={() => void runBulk(chosen, (c) => client.contents.restore(c.id))}>
              {t("moderation.restore")}
            </Button>
          ) : (
            <>
              {canChangeParentAccess && premium && (
                <Button size="sm" variant="outline" disabled={bulk.pending} onClick={() => void runBulk(chosen.filter(toggleable), (c) => client.contents.update(c.id, { access: nextRule(c, true) }))}>
                  {t("contentManager.makePremium")}
                </Button>
              )}
              {canChangeParentAccess && premium && (
                <Button size="sm" variant="outline" disabled={bulk.pending} onClick={() => void runBulk(chosen.filter(toggleable), (c) => client.contents.update(c.id, { access: nextRule(c, false) }))}>
                  {t("contentManager.makeFree")}
                </Button>
              )}
              {canDeleteParent && <Button size="sm" variant="destructive" disabled={bulk.pending} onClick={() => void bulkDelete()}>
                {t("contentManager.delete")}
              </Button>}
            </>
          )}
          <Button size="sm" variant="ghost" disabled={bulk.pending} onClick={() => setSelected(new Set())}>
            {t("contentManager.clearSelection")}
          </Button>
        </div>
      )}
      {loading && !items.length ? (
        <Rows />
      ) : listing.error && !items.length ? (
        <LoadFailed error={listing.error} message={t("contentManager.loadFailed")} retry={listing.reload} />
      ) : !items.length ? (
        <div className="grid justify-items-start gap-2">
          <p className="text-muted-foreground">{filtered ? t("contentManager.noMatches") : deleted ? t("contentManager.noDeleted") : noun ? t("contentManager.emptyNamed", { other: noun.other }) : t("contentManager.emptyAll")}</p>
          {filtered ? (
            <Button size="sm" variant="outline" onClick={() => loc.go("", { deleted: deleted ? "1" : undefined })}>
              {t("contentManager.clearFilters")}
            </Button>
          ) : (
            !deleted && (
              <Button size="sm" onClick={() => loc.go("new")}>
                {noun ? t("contentManager.newNamed", { one: noun.one }) : t("contentManager.new")}
              </Button>
            )
          )}
        </div>
      ) : (
        <ul className="grid divide-y divide-border rounded-lg border border-border" aria-busy={loading}>
          {canSelect && (
            <li className="flex items-center gap-3 px-3 py-2 text-xs text-muted-foreground">
              <input
                type="checkbox"
                className="size-4 shrink-0 accent-primary"
                checked={allChosen}
                aria-label={t("contentManager.selectPage")}
                onChange={(e) => setSelected(e.target.checked ? new Set(items.map((c) => c.id)) : new Set())}
              />
              <span>{t("contentManager.selectPage")}</span>
            </li>
          )}
          {items.map((c) => {
            const s = contentStatus(c);
            const href = s === "published" ? contentHref?.(c) : undefined;
            const pending = busy.has(c.id);
            const isPremium = c.access.level === "tier";
            return (
              <li key={c.id} className="flex flex-wrap items-start gap-3 p-3" data-ckui="content-row" data-status={deleted ? "deleted" : s}>
                {canSelect && (
                  <input
                    type="checkbox"
                    className="mt-1 size-4 shrink-0 accent-primary"
                    checked={selected.has(c.id)}
                    aria-label={t("contentManager.select", { title: c.title })}
                    onChange={(e) =>
                      setSelected((cur) => {
                        const n = new Set(cur);
                        if (e.target.checked) n.add(c.id);
                        else n.delete(c.id);
                        return n;
                      })
                    }
                  />
                )}
                <RowCover url={c.cover?.url} />
                <div className="grid min-w-0 flex-1 gap-1">
                  {deleted && canDeleteParent ? (
                    <span className="font-medium break-words line-through">{c.title}</span>
                  ) : (
                    <button type="button" className="text-left font-medium break-words hover:underline" onClick={() => open(c)}>
                      {c.title}
                    </button>
                  )}
                  <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
                    {!deleted && <Badge variant={badge[s]}>{t(`contentManager.${s}`)}</Badge>}
                    {canChangeParentAccess && <Badge variant={isPremium ? "secondary" : "outline"} data-ckui="access-badge">
                      {accessLabel(t, c.access, labels)}
                    </Badge>}
                    <span className="font-mono">{c.code}</span>
                    <span>{c.language}</span>
                    {c.published_at && s === "published" ? (
                      <time dateTime={c.published_at} title={formatDate(m, c.published_at)}>
                        {t("contentManager.liveOn", { date: formatDate(m, c.published_at, false) })}
                      </time>
                    ) : (
                      <RelativeTime at={deleted && c.deleted_at ? c.deleted_at : c.updated_at} />
                    )}
                    {s === "scheduled" && c.publish_at && <span>{t("pollManager.goesLive", { date: formatDate(m, c.publish_at) })}</span>}
                  </div>
                  <ErrorLine error={rowErrors[c.id] ?? null} />
                </div>
                <div className="flex flex-wrap items-center gap-2">
                  {deleted ? canDeleteParent ? (
                    <Button size="sm" variant="outline" disabled={del.pending} onClick={() => void del.run(() => client.contents.restore(c.id))}>
                      {t("moderation.restore")}
                    </Button>
                  ) : (
                    <Button size="sm" variant="outline" onClick={() => open(c)}>{t("contentManager.manageVersions")}</Button>
                  ) : (
                    <>
                      {toggleable(c) && (
                        <Button size="sm" variant="outline" disabled={pending} aria-busy={pending} onClick={() => void setAccess(c, !isPremium)}>
                          {pending && <Spinner />}
                          {t(isPremium ? "contentManager.makeFree" : "contentManager.makePremium")}
                        </Button>
                      )}
                      {href && (
                        <a className={buttonVariants({ size: "sm", variant: "ghost" })} href={href}>
                          {t("contentManager.view")}
                        </a>
                      )}
                      <Button size="sm" variant="outline" onClick={() => open(c)}>
                        {t("comments.edit")}
                      </Button>
                      {canDeleteParent && <Button size="sm" variant="destructive" disabled={del.pending} onClick={() => void remove(c)}>
                        {t("comments.delete")}
                      </Button>}
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

/** A row's portrait cover, or a quiet placeholder. */
function RowCover({ url }: { url?: string }) {
  return url ? (
    <img src={url} alt="" loading="lazy" className="aspect-[7/10] w-12 shrink-0 rounded-sm bg-muted object-cover" />
  ) : (
    <span aria-hidden className="grid aspect-[7/10] w-12 shrink-0 place-items-center rounded-sm bg-muted text-muted-foreground">
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
  ownerGroup?: string;
  /** Entities a new item starts linked to, e.g. the series it was created from; ignored for an existing item. */
  entities?: readonly GroupCard[];
  /** After create: show the new item (the editor goes on editing it). */
  onCreated?: (content: Content) => void;
  onDeleted?: (content: Content) => void;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
}

/**
 * One content item for its creators and staff: title, description,
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

function StandardContentEditor({ id, client: own, kind, languages: offered, renderEditor, contentHref, labels, ownerGroup, entities, onCreated, onDeleted, onError, management }: ContentEditorProps) {
  const client = useContentKitClient(own);
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const languages = choices(offered);
  const [language, setLanguage] = useState(languages[0]?.value ?? "en");
  const ed = useContent(id, kind, language, { client });
  // A hidden version's cover is edited here; a visible version's in its own form.
  const model = useKinds({ client }).kinds;
  const hidden = !!model?.kinds[kind]?.versions.hidden;
  const report = useErrorReporter(onError);
  // The body is kept as stored (image references); the editor shows it with URLs.
  const [form, setForm] = useState<Form | null>(id ? null : empty);
  const [error, setError] = useState<ContentKitError | string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const { confirm, dialog } = useConfirm();
  const item = ed.content;
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
  const save = async () => {
    setError(null);
    setNotice(null);
    if (!f.title.trim()) return setError(t("contentManager.titleRequired"));
    try {
      if (!item) {
        const created = await ed.create({ kind, text: { [language]: text() }, owner_group_id: ownerGroup });
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
      await ed.putText(text());
      setNotice(t("pollEditor.saved"));
    } catch (e) {
      fail(e);
    }
  };
  const publish = async (live: boolean) => {
    setError(null);
    setNotice(null);
    try {
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
      await ed.remove();
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
  return (
    <>
    <form
      className="grid gap-4"
      data-ckui="content-form"
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="text-base font-semibold">{t(item ? "contentManager.editTitle" : "contentManager.newTitle")}</h2>
        {item && <Badge variant={badge[s]}>{t(`contentManager.${s}`)}</Badge>}
        {href && (
          <a className="text-xs text-muted-foreground hover:underline" href={href}>
            {t("contentManager.view")}
          </a>
        )}
      </div>
      {item?.moderation && (
        <Alert variant={item.moderation === "rejected" ? "destructive" : "default"}>
          <AlertDescription>
            {item.moderation_reason
              ? t(`contentManager.${item.moderation === "rejected" ? "rejectedBecause" : "heldBecause"}`, { reason: item.moderation_reason })
              : t(`contentManager.${item.moderation === "rejected" ? "rejectedNote" : "heldNote"}`)}
          </AlertDescription>
        </Alert>
      )}
      {languages.length > 1 && (
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-lang`}>{t("contentManager.language")}</Label>
          <Select
            value={language}
            onValueChange={(v) => {
              setLanguage(String(v ?? ""));
              setForm(item ? null : empty);
            }}
          >
            <SelectTrigger id={`${ids}-lang`} className="w-full">
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
      )}
      <div className="grid gap-1.5">
        <Label htmlFor={`${ids}-title`}>{t("contentManager.itemTitle")}</Label>
        <Input id={`${ids}-title`} value={f.title} required maxLength={300} onChange={(e) => set({ title: e.target.value })} />
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-publish`}>{t("contentManager.publishAt")}</Label>
          <Input id={`${ids}-publish`} type="datetime-local" value={f.publishAt} onChange={(e) => set({ publishAt: e.target.value })} />
        </div>
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor={`${ids}-description`}>{t("contentManager.description")}</Label>
        <Textarea id={`${ids}-description`} rows={2} value={f.description} onChange={(e) => set({ description: e.target.value })} />
      </div>
      {(hidden || !item) && <Cover item={item} src={ed.coverSrc} saving={ed.saving} onFile={(file) => void ed.setCover(file).catch(fail)} />}
      <div className="grid gap-1.5">
        <span className="text-sm font-medium">{t("contentManager.body")}</span>
        {renderEditor ? renderEditor(editor) : <Textarea aria-label={editor["aria-label"]} rows={12} value={f.body} placeholder={editor.placeholder} onChange={(e) => set({ body: e.target.value })} />}
        {!item && <p className="text-xs text-muted-foreground">{t("contentManager.imagesAfterCreate")}</p>}
      </div>
      {error && <ErrorLine>{typeof error === "string" ? error : contentError(m, error)}</ErrorLine>}
      {notice && !error && (
        <p role="status" className="text-sm text-muted-foreground">
          {notice}
        </p>
      )}
      <div className="flex flex-wrap gap-2 border-t border-border pt-4">
        <Button type="submit" disabled={ed.saving || item?.state === "deleted"}>
          {ed.saving ? t("common.saving") : t(item ? "common.save" : "contentManager.createDraft")}
        </Button>
        {item && item.state !== "deleted" && !item.live && (
          <Button type="button" variant="outline" disabled={ed.saving} onClick={() => void publish(true)}>
            {t("contentManager.publish")}
          </Button>
        )}
        {item?.state === "published" && (
          <Button type="button" variant="outline" disabled={ed.saving} onClick={() => void publish(false)}>
            {t("contentManager.unpublish")}
          </Button>
        )}
        {item && hidden && (
          <Button type="button" variant="destructive" className="ms-auto" disabled={ed.saving} onClick={() => void remove()}>
            {t("contentManager.delete")}
          </Button>
        )}
      </div>
      {dialog}
    </form>
    {item && <ContentSections management={management} content={item} client={client} onError={onError} labels={labels} />}
    </>
  );
}

function Cover({ item, src, saving, onFile }: { item: Content | null; src?: string; saving: boolean; onFile: (file: File | null) => void }) {
  const { t } = useMessages();
  const file = useRef<HTMLInputElement>(null);
  return (
    <div className="grid gap-1.5" data-ckui="content-cover">
      <span className="text-sm font-medium">{t("contentManager.cover")}</span>
      {item ? (
        <div className="flex flex-wrap items-center gap-3">
          {src ? <img src={src} alt="" className="h-20 w-36 rounded-md border border-border object-cover" /> : <span className="text-muted-foreground">{t("contentManager.noCover")}</span>}
          <Button type="button" size="sm" variant="outline" disabled={saving} onClick={() => file.current?.click()}>
            {t(src ? "pollEditor.replaceImage" : "contentManager.addCover")}
          </Button>
          {src && (
            <Button type="button" size="sm" variant="ghost" disabled={saving} onClick={() => onFile(null)}>
              {t("pollEditor.removeImage")}
            </Button>
          )}
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
