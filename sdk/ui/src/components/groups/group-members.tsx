import { Cancel01Icon, Image01Icon, Tick02Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useCallback, useEffect, useId, useMemo, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { TypeaheadQuery } from "../../client/discovery.js";
import { allowsEntityScope } from "../../client/entity-scopes.js";
import type { Card, Content, GroupDetail, GroupKind, Membership } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useContentKitClient, type ContentKitErrorHandler } from "../../react/context.js";
import { useTypeahead } from "../../react/discovery.js";
import { useGroupMembers, useKinds, type Member } from "../../react/groups.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Button, buttonVariants } from "#ckui/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { accessLabel, contentStatus } from "../admin/content-manager.js";
import { SortableList } from "../sortable-list.js";
import { contentError, ErrorLine, formatDate, useAction } from "../social/parts.js";
import { WindowFields } from "./entity-chips.js";
import { labelOf, type Labels } from "./parts.js";

const NONE = "__none__";
// The managed listing hydrates at most this many ids per request.
const BATCH = 100;

export interface GroupMembersProps {
  group: Pick<GroupDetail, "id" | "kind" | "name" | "slug" | "cover" | "code">;
  labels?: Labels;
  /** Where a member's card links, e.g. its admin editor; none: not a link. */
  contentHref?: (content: Content) => string | undefined;
  /** Where a new item of a content kind starts already in this group, e.g. the host's editor with `?with=<code>`; none: no action. */
  newContentHref?: (contentKind: string, group: GroupMembersProps["group"]) => string | undefined;
  /** Replaces a member's card body; default its cover and title. */
  renderContent?: (member: Membership, content?: Content) => ReactNode;
  /** Select a member's cover as the group's. */
  onUseCover?: (contentId: string) => Promise<unknown>;
  pageSize?: number;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

const refKey = (x: Membership) => `${x.content_id}|${x.version_id ?? ""}`;

/** Members' contents by id through the managed listing: drafts and deleted ones too. */
function useMemberContents(ids: readonly string[], client: ContentKitClient) {
  const [byId, setById] = useState<ReadonlyMap<string, Content>>(new Map());
  const wanted = useMemo(() => [...new Set(ids)].sort(), [ids]);
  useEffect(() => {
    const missing = wanted.filter((id) => !byId.has(id));
    if (!missing.length) return;
    const ctl = new AbortController();
    const batches: string[][] = [];
    for (let i = 0; i < missing.length; i += BATCH) batches.push(missing.slice(i, i + BATCH));
    // A failed lookup leaves the member's id as its label; the list stays usable.
    void Promise.all(batches.map((b) => client.contents.manage({ ids: b, limit: b.length }, ctl.signal).catch(() => [] as Content[]))).then((pages) => {
      if (ctl.signal.aborted) return;
      setById((cur) => {
        const next = new Map(cur);
        for (const c of pages.flat()) next.set(c.id, c);
        return next;
      });
    });
    return () => ctl.abort();
    // byId is read, not a trigger: only new ids fetch.
  }, [wanted, client]); // eslint-disable-line react-hooks/exhaustive-deps
  return byId;
}

const coverOf = (c?: Content) => c?.cover?.url ?? c?.versions.find((v) => v.cover?.url)?.cover?.url;

/**
 * Staff: an entity's contents as cover cards grouped by content kind (an
 * ordered kind as a reorderable list), each with its access, state, role and
 * version; add one by searching titles, remove or use its cover on hover.
 */
export function GroupMembers(p: GroupMembersProps) {
  const m = useMessages();
  const { t } = m;
  const client = useContentKitClient(p.client);
  const { kinds } = useKinds({ client: p.client });
  const spec: GroupKind = kinds?.entity_kinds?.[p.group.kind] ?? {};
  const members = useGroupMembers(p.group.id, { pageSize: p.pageSize, client: p.client });
  const contents = useMemberContents(
    useMemo(() => members.items.map((x) => x.content_id!).filter(Boolean), [members.items]),
    client,
  );
  const save = useAction("membership.save", p.onError);
  const [editing, setEditing] = useState<string | null>(null);
  const [win, setWin] = useState<{ from?: string; until?: string }>({});
  const title = (x: Membership) => contents.get(x.content_id ?? "")?.title || x.content_id || "";
  const spoken = (x: Member) => `${title(x)}${x.version_id ? ` (${t("memberships.version")})` : ""}`;
  // Every role of one member shares its place: order unique content and version pairs.
  const order = (items: Member[]) => [...new Map(items.map((x) => [refKey(x), { content_id: x.content_id!, version_id: x.version_id }])).values()];
  const strip = ({ id: _id, ...x }: Member): Membership => x;
  const badges = (x: Member, c?: Content) => (
    <>
      {c && c.access.level !== "public" && <Badge variant="secondary">{accessLabel(t, c.access, p.labels)}</Badge>}
      {c && (c.state === "deleted" || contentStatus(c) !== "published") && (
        <Badge variant={c.state === "deleted" ? "destructive" : "outline"}>{t(c.state === "deleted" ? "contentManager.deleted" : `contentManager.${contentStatus(c)}`)}</Badge>
      )}
      {x.version_id && <Badge variant="outline">{t("memberships.version")}</Badge>}
      {x.role && <Badge variant="secondary">{labelOf(p.labels, x.role)}</Badge>}
      {x.state === "proposed" && <Badge variant="secondary">{t("memberships.proposed")}</Badge>}
    </>
  );
  const actions = (x: Member) => (
    <>
      {x.state === "proposed" && (
        <Button size="icon-xs" variant="secondary" disabled={save.pending} aria-label={`${t("memberships.accept")}: ${spoken(x)}`} title={t("memberships.accept")}
          onClick={() => void save.run(() => members.add([{ ...strip(x), state: "active" }]))}>
          <HugeiconsIcon icon={Tick02Icon} strokeWidth={2.5} />
        </Button>
      )}
      {p.onUseCover && !x.version_id && (
        <Button size="icon-xs" variant="secondary" aria-label={`${t("memberships.useCover")}: ${spoken(x)}`} title={t("memberships.useCover")}
          disabled={save.pending || (p.group.cover?.content_id === x.content_id && !p.group.cover?.auto)}
          onClick={() => void save.run(() => p.onUseCover!(x.content_id!))}>
          <HugeiconsIcon icon={Image01Icon} strokeWidth={2} />
        </Button>
      )}
      <Button size="icon-xs" variant="secondary" disabled={save.pending} aria-label={t("memberships.remove", { name: spoken(x) })} title={t("common.remove")}
        onClick={() => void save.run(() => members.remove([strip(x)]))}>
        <HugeiconsIcon icon={Cancel01Icon} strokeWidth={2.5} />
      </Button>
    </>
  );
  const dates = (x: Member) =>
    spec.windows &&
    (editing === x.id ? (
      <span className="flex flex-wrap items-center gap-1">
        <WindowFields from={win.from} until={win.until} name={spoken(x)} onChange={(w) => setWin((cur) => ({ from: "active_from" in w ? w.active_from : cur.from, until: "active_until" in w ? w.active_until : cur.until }))} />
        <Button size="xs" disabled={save.pending} onClick={() => void save.run(() => members.add([{ ...strip(x), active_from: win.from, active_until: win.until }])).then((d) => d !== undefined && setEditing(null))}>
          {t("common.save")}
        </Button>
        <Button size="xs" variant="ghost" onClick={() => setEditing(null)}>
          {t("common.cancel")}
        </Button>
      </span>
    ) : (
      <button type="button" className="text-start text-xs text-muted-foreground hover:underline" data-ckui="member-window"
        onClick={() => {
          setWin({ from: x.active_from, until: x.active_until });
          setEditing(x.id);
        }}>
        {x.active_from || x.active_until
          ? t("memberships.window", { from: x.active_from ? formatDate(m, x.active_from) : "…", until: x.active_until ? formatDate(m, x.active_until) : "…" })
          : t("memberships.always")}
      </button>
    ));
  const thumb = (c: Content | undefined, className: string) => {
    const url = coverOf(c);
    return url ? <img src={url} alt="" loading="lazy" className={cn("object-cover", className)} /> : <span aria-hidden className={cn("bg-muted", className)} />;
  };
  const linked = (x: Member, body: ReactNode, className?: string) => {
    const c = contents.get(x.content_id ?? "");
    const href = c && p.contentHref?.(c);
    return href ? <a href={href} className={cn("hover:underline", className)}>{body}</a> : <span className={className}>{body}</span>;
  };
  const card = (x: Member) => {
    const c = contents.get(x.content_id ?? "");
    return (
      <li key={x.id} className="group/member relative grid content-start gap-1.5" data-ckui="group-member" data-content={x.content_id} data-state={x.state ?? "active"}>
        {p.renderContent ? (
          p.renderContent(x, c)
        ) : (
          <>
            {linked(x, thumb(c, "aspect-[7/10] w-full rounded-md"), "block")}
            {linked(x, title(x), cn("line-clamp-2 text-sm font-medium break-words", !c && "font-mono text-xs"))}
          </>
        )}
        <span className="flex flex-wrap gap-1">{badges(x, c)}</span>
        {dates(x)}
        <span className="absolute end-1 top-1 flex gap-1 opacity-0 transition-opacity group-hover/member:opacity-100 focus-within:opacity-100">{actions(x)}</span>
      </li>
    );
  };
  const row = (x: Member, handle?: ReactNode) => {
    const c = contents.get(x.content_id ?? "");
    return (
      <div className="group/member flex flex-wrap items-center gap-2" data-ckui="group-member" data-content={x.content_id} data-state={x.state ?? "active"}>
        {handle}
        {x.position !== undefined && <span className="w-8 text-xs text-muted-foreground tabular-nums">{x.position}</span>}
        {p.renderContent ? (
          <span className="min-w-0 flex-1">{p.renderContent(x, c)}</span>
        ) : (
          <>
            {linked(x, thumb(c, "aspect-[7/10] w-24 rounded-md sm:w-28"), "shrink-0")}
            {linked(x, title(x), cn("min-w-0 flex-1 break-words", !c && "font-mono text-xs"))}
          </>
        )}
        {badges(x, c)}
        {dates(x)}
        <span className="flex gap-1 opacity-60 transition-opacity group-hover/member:opacity-100 focus-within:opacity-100">{actions(x)}</span>
      </div>
    );
  };
  // Unordered kinds read best as covers per content kind; unknown kinds last.
  const byKind = useMemo(() => {
    const out = new Map<string, Member[]>();
    for (const x of members.items) {
      const kind = contents.get(x.content_id ?? "")?.kind ?? "";
      out.set(kind, [...(out.get(kind) ?? []), x]);
    }
    return [...out].sort(([a], [b]) => (a === "" ? 1 : b === "" ? -1 : a.localeCompare(b)));
  }, [members.items, contents]);
  // Version-level kinds link a version, which a new item does not have yet.
  const starts = !allowsEntityScope(spec, "content")
    ? []
    : Object.entries(kinds?.kinds ?? {}).flatMap(([kind, k]) => {
        const href = k.entities?.includes(p.group.kind) ? p.newContentHref?.(kind, p.group) : undefined;
        return href ? [{ kind, href }] : [];
      });
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-3 text-sm", p.className)} data-ckui="group-members">
      <h3 className="font-medium">{t("memberships.members")}</h3>
      {starts.length > 0 && (
        <div className="flex flex-wrap gap-2">
          {starts.map((x) => (
            <a key={x.kind} href={x.href} className={buttonVariants({ variant: "outline", size: "sm" })} data-ckui="group-member-new">
              {t("memberships.newContent", { kind: labelOf(p.labels, x.kind), name: p.group.name })}
            </a>
          ))}
        </div>
      )}
      <AddContent spec={spec} labels={p.labels} client={client} onAdd={(x) => save.run(() => members.add([x]))} />
      {members.loading && !members.items.length ? (
        <Skeleton className="h-40 w-full" />
      ) : members.error && !members.items.length ? (
        <div role="alert" className="grid justify-items-start gap-2">
          <p className="text-destructive">{contentError(m, members.error)}</p>
          <Button size="sm" variant="outline" onClick={members.reload}>
            {t("common.retry")}
          </Button>
        </div>
      ) : !members.items.length ? (
        <p className="text-muted-foreground">{t("memberships.noMembers")}</p>
      ) : spec.ordered ? (
        <SortableList
          items={members.items}
          id={(x) => x.id}
          name={spoken}
          label={t("memberships.members")}
          disabled={save.pending}
          onMove={(from, to) => {
            const items = [...members.items];
            const [moved] = items.splice(from, 1);
            items.splice(to, 0, moved!);
            void save.run(() => members.reorder(order(items)));
          }}
        >
          {(x, handle) => row(x, handle)}
        </SortableList>
      ) : (
        byKind.map(([kind, items]) => (
          <section key={kind || "other"} className="grid gap-2" data-ckui="group-members-kind" data-kind={kind}>
            {byKind.length > 1 && kind && <h4 className="text-xs font-medium text-muted-foreground uppercase">{labelOf(p.labels, kind)}</h4>}
            <ul className="grid grid-cols-[repeat(auto-fill,minmax(7.5rem,1fr))] gap-3">{items.map(card)}</ul>
          </section>
        ))
      )}
      {members.hasMore && (
        <div className="grid justify-items-center gap-2">
          {members.error && <p role="alert" className="text-destructive">{contentError(m, members.error)}</p>}
          <Button variant="outline" size="sm" disabled={members.loadingMore} onClick={() => void members.loadMore()}>
            {members.loadingMore ? t("groups.loading") : members.error ? t("common.retry") : t("groups.loadMore")}
          </Button>
        </div>
      )}
      <ErrorLine error={save.error} />
    </ContentKitUiRoot>
  );
}

/** A title search over contents the caller manages; picking one adds it, after its role, version or dates when the kind has them. */
function AddContent({ spec, labels, client, onAdd }: { spec: GroupKind; labels?: Labels; client: ContentKitClient; onAdd: (m: Membership) => Promise<unknown> }) {
  const { t } = useMessages();
  const id = useId();
  const [text, setText] = useState("");
  const [active, setActive] = useState(0);
  const [picked, setPicked] = useState<{ card: Card; versions?: Content["versions"] } | null>(null);
  const [version, setVersion] = useState("");
  const [role, setRole] = useState("");
  const [from, setFrom] = useState<string>();
  const [until, setUntil] = useState<string>();
  const request = useCallback(
    (q: TypeaheadQuery, signal: AbortSignal) =>
      client.discovery.adminSuggestions({ q: q.q, display_language: q.display_language, include_contents: true, include_entities: false, limit: 8 }, signal),
    [client],
  );
  const found = useTypeahead(text, { request, client, limit: 8 });
  const results = found.contents;
  const listOpen = text.trim() !== "" && !picked && results.length > 0;
  useEffect(() => setActive(0), [text]);
  const detailed = allowsEntityScope(spec, "version") || !!spec.roles?.length || !!spec.windows;
  const reset = () => {
    setText("");
    setPicked(null);
    setVersion("");
  };
  const add = (card: Card, v?: string) =>
    void onAdd({ content_id: card.id, version_id: v || undefined, role: role || undefined, active_from: from, active_until: until }).then((d) => d !== undefined && reset());
  const pick = (card: Card) => {
    if (!detailed) return add(card);
    setPicked({ card });
    setText(card.title);
    // A version-level kind links one version: list the picked content's.
    if (allowsEntityScope(spec, "version"))
      void client.contents.manage({ ids: [card.id], limit: 1 }).then(
        (r) => setPicked((cur) => (cur?.card.id === card.id ? { card, versions: r[0]?.versions } : cur)),
        () => undefined,
      );
  };
  return (
    <div className="grid gap-2" data-ckui="add-member">
      <div className="relative">
        <input
          id={`${id}-q`}
          role="combobox"
          aria-label={t("memberships.searchContent")}
          aria-expanded={listOpen}
          aria-controls={`${id}-list`}
          aria-activedescendant={listOpen ? `${id}-${active}` : undefined}
          aria-autocomplete="list"
          autoComplete="off"
          className="h-8 w-full rounded-md border border-input bg-transparent px-2.5 text-sm outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/40"
          placeholder={t("memberships.searchContent")}
          value={text}
          onChange={(e) => {
            setText(e.target.value);
            setPicked(null);
          }}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown" && listOpen) {
              e.preventDefault();
              setActive((a) => Math.min(results.length - 1, a + 1));
            } else if (e.key === "ArrowUp" && listOpen) {
              e.preventDefault();
              setActive((a) => Math.max(0, a - 1));
            } else if (e.key === "Enter" && listOpen) {
              e.preventDefault();
              pick(results[active]!);
            } else if (e.key === "Escape") reset();
          }}
        />
        {listOpen && (
          <ul id={`${id}-list`} role="listbox" aria-label={t("memberships.searchContent")} className="absolute start-0 top-full z-10 mt-1 grid w-full overflow-hidden rounded-md border border-border bg-popover text-popover-foreground shadow-md">
            {results.map((c, i) => (
              <li
                key={c.id}
                id={`${id}-${i}`}
                role="option"
                aria-selected={i === active}
                className={cn("flex cursor-pointer items-center gap-2 px-2 py-1.5 text-sm", i === active && "bg-accent text-accent-foreground")}
                onMouseEnter={() => setActive(i)}
                onMouseDown={(e) => {
                  e.preventDefault();
                  pick(c);
                }}
              >
                {c.cover[0]?.url ? <img src={c.cover[0].url} alt="" className="h-9 w-6 shrink-0 rounded-sm object-cover" /> : <span aria-hidden className="h-9 w-6 shrink-0 rounded-sm bg-muted" />}
                <span className="min-w-0 flex-1 truncate">{c.title}</span>
                <span className="text-xs text-muted-foreground">{labelOf(labels, c.kind)}</span>
              </li>
            ))}
          </ul>
        )}
        {text.trim() !== "" && !picked && !found.loading && found.query === text.trim() && !results.length && (
          <p className="mt-1 text-xs text-muted-foreground">{t("memberships.noMatches")}</p>
        )}
      </div>
      {picked && (
        <div className="flex flex-wrap items-end gap-2" data-ckui="add-member-details">
          {allowsEntityScope(spec, "version") && (
            <Select value={version || NONE} onValueChange={(v) => setVersion(v === NONE ? "" : String(v))}>
              <SelectTrigger size="sm" aria-label={t("memberships.version")}>
                <SelectValue>{(v: string) => (v === NONE ? t("memberships.wholeContent") : (picked.versions?.find((x) => x.id === v)?.title ?? v))}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {allowsEntityScope(spec, "content") && <SelectItem value={NONE}>{t("memberships.wholeContent")}</SelectItem>}
                {(picked.versions ?? []).map((v, i) => (
                  <SelectItem key={v.id} value={v.id}>
                    {v.title || `#${i + 1}`}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          {!!spec.roles?.length && (
            <Select value={role || NONE} onValueChange={(v) => setRole(v === NONE ? "" : String(v))}>
              <SelectTrigger size="sm" aria-label={t("memberships.role")}>
                <SelectValue>{(v: string) => (v === NONE ? t("memberships.noRole") : labelOf(labels, v))}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={NONE}>{t("memberships.noRole")}</SelectItem>
                {spec.roles.map((r) => (
                  <SelectItem key={r} value={r}>
                    {labelOf(labels, r)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          {spec.windows && (
            <WindowFields
              from={from}
              until={until}
              name={t("memberships.newMember")}
              onChange={(w) => {
                if ("active_from" in w) setFrom(w.active_from);
                if ("active_until" in w) setUntil(w.active_until);
              }}
            />
          )}
          <Button size="sm" disabled={!allowsEntityScope(spec, "content") && !version} onClick={() => add(picked.card, version)}>
            {t("memberships.addMember")}
          </Button>
          <Button size="sm" variant="ghost" onClick={reset}>
            {t("common.cancel")}
          </Button>
        </div>
      )}
    </div>
  );
}
