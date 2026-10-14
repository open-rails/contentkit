import { useMemo, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { LETTERS } from "../../client/group-fields.js";
import type { ContentModel, Membership } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useAdminLocation, useFlowClient, useStaffAccess, type AdminLocation, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { useGroups, useKinds, type GroupSort } from "../../react/groups.js";
import { Button } from "#ckui/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Tabs, TabsList, TabsTrigger } from "#ckui/ui/tabs";
import { GroupEditor } from "../groups/editor.js";
import { GroupMembers } from "../groups/memberships.js";
import { labelOf, useLanguageName, type Labels } from "../groups/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Rows, type AdminFlowProps, type AdminRouteProps } from "./shell.js";

export interface GroupManagerProps extends AdminFlowProps, AdminRouteProps {
  /** The group kinds managed, in tab order; default the content model's site-owned kinds. */
  groupKinds?: readonly string[];
  /** Display names of kinds, attributes, roles and values by identifier. */
  labels?: Labels;
  /** Counts of this content kind; default every kind. */
  contentKind?: string;
  /** One member, e.g. a link with its title; default its content id. */
  renderContent?: (member: Membership) => ReactNode;
  pageSize?: number;
}

/** Whether the caller may manage groups (root:groups:write). */
export function useGroupManagerAccess(o: FlowClientOptions = {}): StaffAccess<"groups"> {
  return useStaffAccess(["groups"] as const, o);
}

const SORTS: readonly GroupSort[] = ["name", "count", "newest"];

/**
 * Staff: groups per kind. A kind's groups by letter, order and language; a
 * new group; a group's editor (GroupEditor) and its members (GroupMembers).
 * Kinds are at basePath/{kind}, a group at basePath/{kind}/{id}, filters in the query.
 */
export function GroupManager({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: GroupManagerProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="group-manager" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Manager {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<GroupManagerProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };
type Ctx = Inner & { loc: AdminLocation; kind: string; kinds: ContentModel };

function Manager({ basePath, navigation, ...p }: Inner) {
  const { t } = useMessages();
  const loc = useAdminLocation(basePath, navigation);
  const access = useGroupManagerAccess({ client: p.client });
  const model = useKinds({ client: p.client });
  const kinds = model.kinds;
  const list = useMemo(
    () =>
      p.groupKinds ??
      Object.entries(kinds?.group_kinds ?? {})
        .filter(([, k]) => k.owner !== "user")
        .map(([name]) => name)
        .sort(),
    [p.groupKinds, kinds],
  );
  if (access.loading || model.loading) return <Rows />;
  if (!access.allowed.groups) return <NoAccess>{t("groupManager.noAccess")}</NoAccess>;
  if (!kinds) return <LoadFailed error={model.error} message={t("groupManager.kindsFailed")} retry={model.reload} />;
  if (!list.length) return <p className="text-muted-foreground">{t("groupManager.noKinds")}</p>;
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
      {t("groupManager.back")}
    </Button>
  );
}

function List({ loc, kind, kinds, client, contentKind, labels, pageSize = 50 }: Ctx) {
  const m = useMessages();
  const { t } = m;
  const languageName = useLanguageName();
  const languages = kinds.languages;
  const letterParam = loc.query.get("letter") ?? "";
  const letter = LETTERS.includes(letterParam) ? letterParam : undefined;
  const sortParam = loc.query.get("sort") ?? "";
  const sort = (SORTS as readonly string[]).includes(sortParam) ? (sortParam as GroupSort) : "name";
  const languageParam = loc.query.get("language") ?? "";
  const language = languages.includes(languageParam) ? languageParam : languages[0];
  const filters: { letter?: string; sort?: GroupSort; language?: string } = { letter, sort: sort === "name" ? undefined : sort, language: language === languages[0] ? undefined : language };
  const filter = (next: Partial<typeof filters>) => loc.go(kind, { ...filters, ...next });
  const groups = useGroups({ kind, letter, sort, language, contentKind, pageSize }, { client });
  const label = labelOf(labels, kind);
  return (
    <div className="grid gap-4" data-ckui="group-list" data-kind={kind}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold">{label}</h2>
        <Button size="sm" onClick={() => loc.go(`${kind}/new`)}>
          {t("groupManager.new", { kind: label })}
        </Button>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Select value={sort} onValueChange={(v) => filter({ sort: v === "name" ? undefined : (String(v) as GroupSort) })}>
          <SelectTrigger size="sm" aria-label={t("groupManager.sort")}>
            <SelectValue>{(v: string) => t(`groupManager.sort_${v as GroupSort}`)}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {SORTS.map((s) => (
              <SelectItem key={s} value={s}>
                {t(`groupManager.sort_${s}`)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {languages.length > 1 && (
          <Select value={language} onValueChange={(v) => filter({ language: v === languages[0] ? undefined : String(v) })}>
            <SelectTrigger size="sm" aria-label={t("groupManager.language")}>
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
      <nav aria-label={t("groups.byLetter")} className="flex flex-wrap gap-1" data-ckui="letters">
        {[undefined, ...LETTERS].map((l) => (
          <Button
            key={l ?? "all"}
            size="xs"
            variant={letter === l ? "default" : "ghost"}
            aria-pressed={letter === l}
            aria-label={l === undefined ? undefined : l === "#" ? t("groups.otherLetters") : l.toUpperCase()}
            className="min-w-7"
            onClick={() => filter({ letter: l })}
          >
            {l === undefined ? t("groups.all") : l.toUpperCase()}
          </Button>
        ))}
      </nav>
      {groups.loading && !groups.items.length ? (
        <Rows />
      ) : groups.error && !groups.items.length ? (
        <LoadFailed error={groups.error} message={t("groupManager.loadFailed")} retry={groups.reload} />
      ) : !groups.items.length ? (
        <p className="text-muted-foreground">{t("groups.empty")}</p>
      ) : (
        <ul className="grid divide-y divide-border rounded-lg border border-border" aria-busy={groups.loading}>
          {groups.items.map((g) => (
            <li key={g.id} className="flex flex-wrap items-center gap-3 p-3" data-ckui="group-row" data-slug={g.slug}>
              <div className="grid min-w-0 flex-1 gap-0.5">
                <button type="button" className="text-left font-medium break-words hover:underline" onClick={() => loc.go(`${kind}/${g.id}`)}>
                  {g.name || g.slug}
                </button>
                <span className="text-xs text-muted-foreground">{g.slug}</span>
              </div>
              <span className="text-sm text-muted-foreground tabular-nums" title={t("groupManager.count")}>
                {new Intl.NumberFormat(m.language).format(g.count)}
              </span>
            </li>
          ))}
        </ul>
      )}
      {groups.hasMore && (
        <Button variant="outline" size="sm" className="justify-self-center" disabled={groups.loadingMore} onClick={() => void groups.loadMore()}>
          {groups.loadingMore ? t("groups.loading") : t("groups.loadMore")}
        </Button>
      )}
    </div>
  );
}

function Create({ loc, kind, client, labels, onError }: Ctx) {
  return (
    <div className="grid gap-4">
      <Back loc={loc} kind={kind} />
      <GroupEditor
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

function Edit({ loc, kind, client, labels, onError, renderContent, id }: Ctx & { id: string }) {
  return (
    <div className="grid gap-4">
      <Back loc={loc} kind={kind} />
      <GroupEditor
        id={id}
        kind={kind}
        labels={labels}
        client={client}
        onError={onError}
        onMerged={(r) => loc.go(`${kind}/${r.into}`, undefined, { replace: true })}
      >
        {(group) => (
          <GroupMembers
            group={group}
                labels={labels}
            client={client}
            onError={onError}
            renderContent={renderContent}
            onUseCover={(content) => client.groups.update(group.id, { cover: { content_id: content } })}
          />
        )}
      </GroupEditor>
    </div>
  );
}
