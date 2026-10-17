import { cn } from "cn";
import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentMemberships, GroupCard, GroupDetail, GroupKind, Membership } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import type { ContentKitErrorHandler } from "../../react/context.js";
import { useContentGroups, useGroupMembers, useKinds, type Member } from "../../react/groups.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { SortableList } from "../sortable-list.js";
import { contentError, ErrorLine, formatDate, useAction } from "../social/parts.js";
import { EntityChips, WindowFields, type VersionChoice } from "./entity-chips.js";
import { labelOf, type Labels } from "./parts.js";

const NONE = "__none__";

export type { VersionChoice } from "./entity-chips.js";

export interface MembershipEditorProps {
  /** The content whose memberships are edited, its versions' included. */
  content: string;
  /** Its kind: the group kinds it joins. */
  contentKind: string;
  /** Staff lookup, including internal classifications. */
  management?: boolean;
  /** Its versions, for version-level memberships (voice actors, version-only tags). */
  versions?: readonly VersionChoice[];
  /** Only these group kinds; default every kind its kind joins. */
  groupKinds?: readonly string[];
  labels?: Labels;
  /** Group names in this language; default the provider's. */
  language?: string;
  onSaved?: (saved: ContentMemberships) => void;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

type Draft = Membership & { key: number };

const strip = ({ key: _key, ...m }: Draft): Membership => m;
const same = (a: Membership[], b: Membership[]) => JSON.stringify(a) === JSON.stringify(b);

/**
 * Staff: a content's groups, per group kind: add and remove groups, and per
 * membership the version it targets, its role, its position in an ordered
 * group, its window and acceptance of a proposed one. Saved in one write per
 * content for the kinds shown.
 */
export function MembershipEditor(p: MembershipEditorProps) {
  const m = useMessages();
  const { t } = m;
  const { kinds, loading: kindsLoading } = useKinds({ client: p.client });
  const cg = useContentGroups(p.content, { language: p.language, client: p.client });
  const groupKinds = useMemo(() => p.groupKinds ?? kinds?.kinds[p.contentKind]?.entities ?? [], [p.groupKinds, kinds, p.contentKind]);
  const [picked, setPicked] = useState<ReadonlyMap<string, GroupCard>>(new Map());
  const cards = useMemo(() => new Map([...cg.entities, ...picked]), [cg.entities, picked]);
  const seq = useRef(0);
  const initial = useMemo(
    () => cg.memberships.filter((x) => groupKinds.includes(cg.entities.get(x.group_id ?? "")?.kind ?? "")).map((x) => ({ ...x, content_id: undefined })),
    [cg.memberships, cg.entities, groupKinds],
  );
  const [draft, setDraft] = useState<Draft[]>([]);
  const dirty = !same(draft.map(strip), initial);
  const dirtyRef = useRef(dirty);
  dirtyRef.current = dirty;
  useEffect(() => {
    if (!dirtyRef.current || !draft.length) setDraft(initial.map((x) => ({ ...x, key: ++seq.current })));
    // Server data replaces an untouched draft only.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initial]);
  const save = useAction("membership.save", p.onError);
  const [saved, setSaved] = useState(false);
  const change = (key: number, patch: Partial<Membership>) => {
    setSaved(false);
    setDraft((d) => d.map((x) => (x.key === key ? { ...x, ...patch } : x)));
  };
  const root = (children: ReactNode) => (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="membership-editor">
      {children}
    </ContentKitUiRoot>
  );
  if (kindsLoading || (cg.loading && !cg.memberships.length)) return root(<Skeleton className="h-32 w-full" />);
  if (cg.error && !cg.memberships.length)
    return root(
      <div role="alert" className="grid justify-items-start gap-2">
        <p className="text-destructive">{contentError(m, cg.error)}</p>
        <Button size="sm" variant="outline" onClick={cg.reload}>
          {t("common.retry")}
        </Button>
      </div>,
    );
  const versionLabel = (id: string) => p.versions?.find((v) => v.id === id)?.label ?? id.slice(0, 8);
  return root(
    <form
      className="grid gap-5"
      onSubmit={(e) => {
        e.preventDefault();
        setSaved(false);
        void save.run(() => cg.set(draft.map(strip), [...groupKinds])).then((out) => {
          if (!out) return;
          setSaved(true);
          setDraft(out.memberships.filter((x) => groupKinds.includes(out.entities.find((g) => g.id === x.group_id)?.kind ?? "")).map((x) => ({ ...x, content_id: undefined, key: ++seq.current })));
          p.onSaved?.(out);
        });
      }}
    >
      {groupKinds.map((kind) => {
        const spec: GroupKind = kinds?.entity_kinds?.[kind] ?? {};
        const rows = draft.filter((x) => cards.get(x.group_id ?? "")?.kind === kind);
        const label = labelOf(p.labels, kind);
        return (
          <div key={kind} data-ckui="membership-kind" data-group-kind={kind}>
            <EntityChips
              kind={kind}
              label={label}
              memberships={rows}
              cards={cards}
              spec={spec}
              labels={p.labels}
              versions={p.versions}
              versionLabel={versionLabel}
              language={p.language}
              management={p.management}
              client={p.client}
              onChange={(i, patch) => change(rows[i]!.key, patch)}
              onRemove={(i) => {
                setSaved(false);
                setDraft((d) => d.filter((y) => y.key !== rows[i]!.key));
              }}
              onAdd={(g) => {
                setSaved(false);
                setPicked((all) => new Map(all).set(g.id, g));
                setDraft((d) => [...d, { group_id: g.id, key: ++seq.current }]);
              }}
            />
          </div>
        );
      })}
      {!groupKinds.length && <p className="text-muted-foreground">{t("memberships.noKinds")}</p>}
      <ErrorLine error={save.error} />
      <div className="flex items-center gap-3">
        <Button type="submit" size="sm" disabled={save.pending || !dirty}>
          {save.pending ? t("common.saving") : t("common.save")}
        </Button>
        {dirty && (
          <Button type="button" size="sm" variant="ghost" disabled={save.pending} onClick={() => setDraft(initial.map((x) => ({ ...x, key: ++seq.current })))}>
            {t("memberships.discard")}
          </Button>
        )}
        {saved && !dirty && (
          <span role="status" className="text-muted-foreground">
            {t("memberships.saved")}
          </span>
        )}
      </div>
    </form>,
  );
}



export interface GroupMembersProps {
  group: Pick<GroupDetail, "id" | "kind" | "name" | "slug" | "cover">;
  labels?: Labels;
  /** One member, e.g. a link with its title; default its content id. */
  renderContent?: (member: Membership) => ReactNode;
  /** Select a member's cover as the group's. */
  onUseCover?: (contentId: string) => Promise<unknown>;
  pageSize?: number;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

const refKey = (x: Membership) => `${x.content_id}|${x.version_id ?? ""}`;

/**
 * Staff: a group's members in member order: add by content (and version),
 * remove, reorder an ordered group, edit windows, accept proposed members,
 * and use a member's cover as the group's.
 */
export function GroupMembers(p: GroupMembersProps) {
  const m = useMessages();
  const { t } = m;
  const { kinds } = useKinds({ client: p.client });
  const spec: GroupKind = kinds?.entity_kinds?.[p.group.kind] ?? {};
  const members = useGroupMembers(p.group.id, { pageSize: p.pageSize, client: p.client });
  const save = useAction("membership.save", p.onError);
  const [editing, setEditing] = useState<string | null>(null);
  const [win, setWin] = useState<{ from?: string; until?: string }>({});
  const describe = (x: Membership) => (p.renderContent ? p.renderContent(x) : <span className="font-mono text-xs break-all">{x.content_id}</span>);
  const spoken = (x: Member) => `${x.content_id}${x.version_id ? ` ${x.version_id}` : ""}`;
  // Every role of one member shares its place: order unique content and version pairs.
  const order = (items: Member[]) => [...new Map(items.map((x) => [refKey(x), { content_id: x.content_id!, version_id: x.version_id }])).values()];
  const strip = ({ id: _id, ...x }: Member): Membership => x;
  const row = (x: Member, handle?: ReactNode) => (
    <div className="flex flex-wrap items-center gap-2" data-ckui="group-member" data-content={x.content_id} data-state={x.state ?? "active"}>
      {handle}
      {spec.ordered && x.position !== undefined && <span className="w-8 text-xs text-muted-foreground tabular-nums">{x.position}</span>}
      <span className="min-w-0 flex-1">{describe(x)}</span>
      {x.version_id && <Badge variant="outline">{t("memberships.version")}</Badge>}
      {x.role && <Badge variant="secondary">{labelOf(p.labels, x.role)}</Badge>}
      {x.state === "proposed" && (
        <>
          <Badge variant="secondary">{t("memberships.proposed")}</Badge>
          <Button size="xs" variant="outline" disabled={save.pending} onClick={() => void save.run(() => members.add([{ ...strip(x), state: "active" }]))}>
            {t("memberships.accept")}
          </Button>
        </>
      )}
      {spec.windows &&
        (editing === x.id ? (
          <span className="flex flex-wrap items-center gap-1">
            <WindowFields from={win.from} until={win.until} name={spoken(x)} onChange={(w) => setWin((cur) => ({ from: "active_from" in w ? w.active_from : cur.from, until: "active_until" in w ? w.active_until : cur.until }))} />
            <Button
              size="xs"
              disabled={save.pending}
              onClick={() =>
                void save.run(() => members.add([{ ...strip(x), active_from: win.from, active_until: win.until }])).then((d) => d !== undefined && setEditing(null))
              }
            >
              {t("common.save")}
            </Button>
            <Button size="xs" variant="ghost" onClick={() => setEditing(null)}>
              {t("common.cancel")}
            </Button>
          </span>
        ) : (
          <>
            <span className="text-xs text-muted-foreground" data-ckui="member-window">
              {x.active_from || x.active_until
                ? t("memberships.window", { from: x.active_from ? formatDate(m, x.active_from) : "…", until: x.active_until ? formatDate(m, x.active_until) : "…" })
                : t("memberships.always")}
            </span>
            <Button
              size="xs"
              variant="ghost"
              onClick={() => {
                setWin({ from: x.active_from, until: x.active_until });
                setEditing(x.id);
              }}
            >
              {t("memberships.editWindow")}
            </Button>
          </>
        ))}
      {p.onUseCover && !x.version_id && (
        <Button size="xs" variant="ghost" disabled={save.pending || (p.group.cover?.content_id === x.content_id && !p.group.cover?.auto)} onClick={() => void save.run(() => p.onUseCover!(x.content_id!))}>
          {t("memberships.useCover")}
        </Button>
      )}
      <Button size="xs" variant="ghost" disabled={save.pending} aria-label={t("memberships.remove", { name: spoken(x) })} onClick={() => void save.run(() => members.remove([strip(x)]))}>
        {t("common.remove")}
      </Button>
    </div>
  );
  return (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-3 text-sm", p.className)} data-ckui="group-members">
      <h3 className="font-medium">{t("memberships.members")}</h3>
      {members.loading && !members.items.length ? (
        <Skeleton className="h-20 w-full" />
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
        <ul className="grid divide-y divide-border rounded-lg border border-border">
          {members.items.map((x) => (
            <li key={x.id} className="p-2">
              {row(x)}
            </li>
          ))}
        </ul>
      )}
      {members.hasMore && (
        <div className="grid justify-items-center gap-2">
          {members.error && <p role="alert" className="text-destructive">{contentError(m, members.error)}</p>}
          <Button variant="outline" size="sm" disabled={members.loadingMore} onClick={() => void members.loadMore()}>
            {members.loadingMore ? t("groups.loading") : members.error ? t("common.retry") : t("groups.loadMore")}
          </Button>
        </div>
      )}
      <AddMember spec={spec} labels={p.labels} onAdd={(x) => save.run(() => members.add([x]))} />
      <ErrorLine error={save.error} />
    </ContentKitUiRoot>
  );
}

const ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

function AddMember({ spec, labels, onAdd }: { spec: GroupKind; labels?: Labels; onAdd: (m: Membership) => Promise<unknown> }) {
  const { t } = useMessages();
  const id = useId();
  const [content, setContent] = useState("");
  const [version, setVersion] = useState("");
  const [role, setRole] = useState("");
  const [from, setFrom] = useState<string>();
  const [until, setUntil] = useState<string>();
  const ok = ID.test(content.trim()) && (!version.trim() || ID.test(version.trim()));
  return (
    <form
      className="flex flex-wrap items-end gap-2"
      data-ckui="add-member"
      onSubmit={(e) => {
        e.preventDefault();
        if (!ok) return;
        void onAdd({ content_id: content.trim(), version_id: version.trim() || undefined, role: role || undefined, active_from: from, active_until: until }).then((d) => {
          if (d === undefined) return;
          setContent("");
          setVersion("");
        });
      }}
    >
      <div className="grid gap-1">
        <Label htmlFor={`${id}-c`} className="text-xs text-muted-foreground">
          {t("memberships.contentId")}
        </Label>
        <Input id={`${id}-c`} className="h-8 w-80 max-w-full font-mono" value={content} onChange={(e) => setContent(e.target.value)} />
      </div>
      {spec.version_level && (
        <div className="grid gap-1">
          <Label htmlFor={`${id}-v`} className="text-xs text-muted-foreground">
            {t("memberships.versionId")}
          </Label>
          <Input id={`${id}-v`} className="h-8 w-80 max-w-full font-mono" value={version} onChange={(e) => setVersion(e.target.value)} />
        </div>
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
      <Button type="submit" size="sm" variant="outline" disabled={!ok}>
        {t("memberships.addMember")}
      </Button>
    </form>
  );
}
