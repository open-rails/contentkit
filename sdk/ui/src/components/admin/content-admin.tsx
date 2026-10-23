import { useId, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentModel } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useAdminLocation, useFlowClient, type AdminLocation, type AdminNavigation } from "../../react/admin.js";
import { useKinds } from "../../react/entities.js";
import { useReadScope } from "../../react/context.js";
import { Button } from "#ckui/ui/button";
import { labelOf, type Labels } from "../entities/parts.js";
import { CommentModeration, type CommentModerationProps } from "../social/moderation.js";
import { ContentManager, type ContentManagerProps } from "./content-manager.js";
import { EntityManager, type EntityManagerProps } from "./entity-manager.js";
import { PollManager, type PollManagerProps } from "./poll-manager.js";
import { AdminRoot, LoadFailed, Rows, type AdminFlowProps, type AdminRouteProps } from "./shell.js";
import { StatsDashboard } from "./stats-dashboard.js";

export type ContentAdminSection = "contents" | "entities" | "moderation" | "polls" | "statistics";

/** An optional reference workbench; each section remains usable as its own component. */
export interface ContentAdminProps extends AdminFlowProps, AdminRouteProps {
  /** Sections in navigation order. Default all. Navigation does not grant access. */
  sections?: readonly ContentAdminSection[];
  /** Content kinds in picker order; default all configured kinds. Unknown kinds are omitted. */
  contentKinds?: readonly string[];
  /** Entity kinds in picker order; default the configured site-owned kinds. Unknown kinds are omitted. */
  entityKinds?: readonly string[];
  labels?: Labels;
  content?: Pick<ContentManagerProps, "renderEditor" | "contentHref">;
  entities?: Pick<EntityManagerProps, "renderContent" | "contentHref">;
  moderation?: Pick<CommentModerationProps, "itemHref" | "userHref" | "sections">;
  polls?: Pick<PollManagerProps, "freeText" | "maxOptions">;
}

const SECTIONS: readonly ContentAdminSection[] = ["contents", "entities", "moderation", "polls", "statistics"];
const MODERATION: readonly string[] = ["comments", "queue", "bans", "words", "ips"];

/**
 * Model-driven content, group, moderation, poll and statistics screens, with
 * the host's transport, router and theme. Only the selected manager mounts;
 * its native permissions and refusals remain authoritative. This component
 * does not mount a server portal, sign users in or define a publishing workflow.
 */
export function ContentAdmin({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: ContentAdminProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  const scope = useReadScope();
  const [boundary, setBoundary] = useState({ client, scope, revision: 0 });
  if (boundary.client !== client || boundary.scope !== scope) {
    setBoundary({ client, scope, revision: boundary.revision + 1 });
  }
  return (
    <AdminRoot flow="content-admin" guard={guard} appearance={appearance} className={className} language={language} messages={messages}>
      <Workbench key={boundary.revision} {...p} client={client} guard={guard} />
    </AdminRoot>
  );
}

type WorkbenchProps = Omit<ContentAdminProps, "client" | "baseUrl" | "auth" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };

function knownKinds(wanted: readonly string[] | undefined, configured: object): string[] {
  return [...new Set(wanted ?? Object.keys(configured).sort())].filter((kind) => Object.hasOwn(configured, kind));
}

function entityKinds(wanted: readonly string[] | undefined, model: ContentModel): string[] {
  return knownKinds(wanted ?? Object.keys(model.entity_kinds).filter((kind) => model.entity_kinds[kind]?.owner !== "user").sort(), model.entity_kinds);
}

function decode(part: string | undefined): string | undefined {
  if (part === undefined) return undefined;
  try { return decodeURIComponent(part); } catch { return undefined; }
}

/** Routes nested managers through the same outer history, including memory-only use. */
function childNavigation(base: string, view: string, loc: AdminLocation): AdminNavigation {
  return {
    pathname: `${base}/${view}`,
    search: loc.query.size ? `?${loc.query}` : "",
    navigate(to, options) {
      const target = new URL(to, "https://contentkit.invalid");
      if (target.origin !== "https://contentkit.invalid") return;
      const prefix = `${base}/`;
      if (target.pathname !== base && !target.pathname.startsWith(prefix)) return;
      loc.go(target.pathname.slice(prefix.length), Object.fromEntries(target.searchParams), options);
    },
  };
}

function Workbench({ client, basePath, navigation, sections: wanted, contentKinds: requestedContents, entityKinds: requestedEntities, labels, guard, onError, content, entities, moderation, polls }: WorkbenchProps) {
  const { t } = useMessages();
  const id = useId();
  const loc = useAdminLocation(basePath, navigation);
  const model = useKinds({ client });
  if (model.loading) return <Rows />;
  if (!model.kinds) return <LoadFailed error={model.error} message={t("contentAdmin.modelFailed")} retry={model.reload} />;

  const kinds = knownKinds(requestedContents, model.kinds.kinds);
  const managedEntities = entityKinds(requestedEntities, model.kinds);
  const sections = [...new Set(wanted ?? SECTIONS)].filter((section) =>
    SECTIONS.includes(section) &&
    (section !== "contents" && section !== "statistics" || kinds.length > 0) &&
    (section !== "entities" || managedEntities.length > 0) &&
    (section !== "moderation" || moderation?.sections?.length !== 0),
  );
  if (!sections.length) return <p role="status" className="text-muted-foreground">{t("contentAdmin.empty")}</p>;

  const parts = loc.view.split("/");
  const section = (loc.view ? parts[0] : sections[0]) as ContentAdminSection;
  const requestedKind = parts[1] === undefined ? undefined : decode(parts[1]);
  const kind = requestedKind ?? kinds[0]!;
  const moderationSections: readonly string[] = moderation?.sections ?? MODERATION;
  const valid = sections.includes(section) &&
    (section !== "contents" || (parts[1] === undefined || requestedKind !== undefined) && kinds.includes(kind)) &&
    (section !== "entities" || parts[1] === undefined || requestedKind !== undefined && managedEntities.includes(requestedKind)) &&
    (section !== "moderation" || !parts[1] || moderationSections.includes(parts[1])) &&
    (section !== "statistics" || parts.length === 1 || !loc.view);
  const base = basePath?.replace(/\/+$/, "") ?? "";
  const prefix = section === "contents" ? `contents/${encodeURIComponent(kind)}` : section;
  const view = loc.view || prefix;
  // A contents root selects the first configured kind without changing the URL.
  const effectiveView = section === "contents" && parts[1] === undefined ? prefix : view;
  const nav = childNavigation(base, effectiveView, loc);
  const shared = { client, guard, onError };
  const names: Record<ContentAdminSection, string> = {
    contents: t("contentManager.title"), entities: t("contentAdmin.entities"), moderation: t("moderation.title"), polls: t("pollManager.title"), statistics: t("stats.title"),
  };
  const destination = (s: ContentAdminSection) => s === "contents" ? `contents/${encodeURIComponent(kinds[0]!)}` : s;

  return (
    <>
      <h1 className="text-lg font-semibold">{t("contentAdmin.title")}</h1>
      <div className="grid min-w-0 gap-4 sm:grid-cols-[12rem_minmax(0,1fr)]">
        <nav aria-label={t("contentAdmin.navigation")} className="flex gap-1 overflow-x-auto sm:grid sm:content-start">
          {sections.map((s) => (
            <Button key={s} variant={section === s ? "secondary" : "ghost"} className="justify-start whitespace-nowrap" aria-current={section === s ? "page" : undefined} onClick={() => loc.go(destination(s))}>
              {names[s]}
            </Button>
          ))}
        </nav>
        <div className="grid min-w-0 content-start gap-4" data-ckui="content-admin-view">
          {!valid ? (
            <div className="grid justify-items-start gap-2">
              <p role="status">{t("contentAdmin.unavailable")}</p>
              <Button variant="outline" onClick={() => loc.go("")}>{t("contentAdmin.back")}</Button>
            </div>
          ) : section === "contents" ? (
            <>
              <label className="grid justify-items-start gap-1 text-sm" htmlFor={`${id}-kind`}>
                {t("contentAdmin.kind")}
                <select id={`${id}-kind`} className="h-8 rounded-md border border-input bg-background px-2 text-sm" value={kind} onChange={(event) => loc.go(`contents/${encodeURIComponent(event.target.value)}`)}>
                  {kinds.map((k) => <option key={k} value={k}>{labelOf(labels, k)}</option>)}
                </select>
              </label>
              <ContentManager key={kind} {...shared} {...content} kind={kind} labels={labels} languages={model.kinds.languages} basePath={`${base}/${prefix}`} navigation={nav} />
            </>
          ) : section === "entities" ? (
            <EntityManager {...shared} {...entities} labels={labels} entityKinds={managedEntities} basePath={`${base}/entities`} navigation={nav} />
          ) : section === "moderation" ? (
            <CommentModeration {...shared} {...moderation} contentKinds={kinds.map((value) => ({ value, label: labelOf(labels, value) }))} basePath={`${base}/moderation`} navigation={nav} />
          ) : section === "polls" ? (
            <PollManager {...shared} {...polls} languages={model.kinds.languages} basePath={`${base}/polls`} navigation={nav} />
          ) : (
            <StatsDashboard {...shared} kinds={kinds.map((value) => ({ value, label: labelOf(labels, value) }))} />
          )}
        </div>
      </div>
    </>
  );
}
