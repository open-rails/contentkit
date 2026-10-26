import { Add01Icon, ArrowLeft02Icon, Delete02Icon, MoreHorizontalIcon, Search01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useId, useState, type CSSProperties, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { PollState } from "../../client/content/polls.js";
import type { Poll } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import { useAdminLocation, useFlowClient, useStaffAccess, type AdminLocation, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { keyOf, useContentScope, usePaged } from "../../react/use-resource.js";
import { Button } from "#ckui/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "#ckui/ui/dropdown-menu";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { PollEditor, PollStateDot, pollState } from "../social/poll-editor.js";
import { ErrorLine, formatDate, useAction } from "../social/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Pager, Rows, pageOf, useConfirm, type AdminFlowProps, type AdminRouteProps } from "./shell.js";

export { pollState };

type Choice = string | { value: string; label: string };

export interface PollManagerProps extends AdminFlowProps, AdminRouteProps {
  /** Languages to filter by and write in: codes, or codes with labels; the first is a new poll's. */
  languages?: readonly Choice[];
  /** Most options per poll. Default 10. */
  maxOptions?: number;
  pageSize?: number;
}

/** Whether the caller may manage polls (root:polls:write). */
export function usePollManagerAccess(o: FlowClientOptions = {}): StaffAccess<"polls"> {
  return useStaffAccess(["polls"] as const, o);
}

const STATES: readonly PollState[] = ["live", "scheduled", "closed", "inactive"];
const choices = (list?: readonly Choice[], name: (code: string) => string = (c) => c) => (list ?? []).map((c) => (typeof c === "string" ? { value: c, label: name(c) } : c));

/**
 * Staff: every poll, by language, month or day, state and question, to
 * create, edit (PollEditor) and delete. The list is at basePath, a new poll
 * at basePath/new and each poll at basePath/{id}, filters in the query.
 */
export function PollManager({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: PollManagerProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="poll-manager" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Polls {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<PollManagerProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };

function Polls({ basePath, navigation, ...p }: Inner) {
  const { t } = useMessages();
  const loc = useAdminLocation(basePath, navigation);
  const access = usePollManagerAccess({ client: p.client });
  const languageName = useLanguageName();
  // The new poll's editor replaces the create form, so the manager says it was created.
  const [created, setCreated] = useState<string | null>(null);
  useEffect(() => {
    if (created && loc.view !== created && loc.view !== "new") setCreated(null);
  }, [created, loc.view]);
  if (access.loading) return <Rows />;
  if (!access.allowed.polls) return <NoAccess>{t("pollManager.noAccess")}</NoAccess>;
  if (!loc.view) return <List {...p} loc={loc} />;
  const languages = choices(p.languages, languageName);
  const isNew = loc.view === "new";
  return (
    <div className="grid gap-4">
      <nav aria-label={t("entityEditor.breadcrumb")} className="flex items-center gap-1.5 text-sm text-muted-foreground" data-ckui="poll-breadcrumb">
        <HugeiconsIcon icon={ArrowLeft02Icon} strokeWidth={2} className="size-4" />
        <button type="button" className="underline-offset-4 hover:text-foreground hover:underline" onClick={() => loc.go("")}>
          {t("pollManager.title")}
        </button>
        <span aria-hidden>/</span>
        <span aria-current="page" className="text-foreground">
          {t(isNew ? "pollEditor.newTitle" : "pollEditor.editTitle")}
        </span>
      </nav>
      {isNew ? (
        <PollEditor
          languages={languages.length ? languages : undefined}
          maxOptions={p.maxOptions}
          client={p.client}
          onError={p.onError}
          onCancel={() => loc.go("")}
          onCreated={(poll) => {
            setCreated(poll.id);
            loc.go(poll.id, undefined, { replace: true });
          }}
        />
      ) : (
        <>
          {created === loc.view && (
            <p role="status" className="text-sm text-muted-foreground">
              {t("pollEditor.created")}
            </p>
          )}
          <PollEditor key={loc.view} poll={loc.view} maxOptions={p.maxOptions} client={p.client} onError={p.onError} onCancel={() => loc.go("")} onDeleted={() => loc.go("", undefined, { replace: true })} />
        </>
      )}
    </div>
  );
}

// The question, its state, votes, when it went live or closes, and its actions; one column each from md up.
const COLUMNS = "minmax(0,1fr) 7rem 5rem 10rem 5.5rem";

function List({ loc, client, languages: offered, pageSize = 20, onError }: Inner & { loc: AdminLocation }) {
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const languageName = useLanguageName();
  const languages = choices(offered, languageName);
  const query = loc.query.get("q") ?? "";
  const language = loc.query.get("language") ?? "";
  const stateParam = loc.query.get("state") ?? "";
  const state = (STATES as readonly string[]).includes(stateParam) ? (stateParam as PollState) : undefined;
  const month = loc.query.get("month") ?? "";
  const date = loc.query.get("date") ?? "";
  const page = pageOf(loc.query);
  const filters = { q: query, language, state, month, date };
  const filtered = !!(month || date || query || language || state);
  const filter = (next: Partial<typeof filters>) => loc.go("", { ...filters, ...next });
  const [text, setText] = useState(query);
  useEffect(() => setText(query), [query]);
  const { store, scope } = useContentScope(client);
  const list = usePaged(store, keyOf("polls-page", scope, query, language, state, month, date, pageSize), { type: "polls" }, page, pageSize, (q, signal) =>
    client.polls.adminList({ ...q, q: query || undefined, language: language || undefined, state, month: month || undefined, date: date || undefined }, signal),
  );
  const del = useAction("poll.save", onError);
  const { confirm, dialog } = useConfirm();
  const remove = async (poll: Poll) => {
    const ok = await confirm({ title: t("pollEditor.deleteTitle"), description: t("pollEditor.deleteBody"), confirm: t("pollEditor.delete"), typed: t("admin.deleteWord") });
    if (ok) await del.run(() => client.polls.delete(poll.id));
  };
  const count = new Intl.NumberFormat(m.language);
  return (
    <div className="grid gap-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 className="text-xl font-semibold tracking-tight">{t("pollManager.title")}</h2>
        <Button size="sm" onClick={() => loc.go("new")}>
          <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
          {t("pollManager.new")}
        </Button>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <form
          className="relative min-w-48 flex-1"
          role="search"
          onSubmit={(e) => {
            e.preventDefault();
            filter({ q: text.trim() });
          }}
        >
          <Label htmlFor={`${ids}-q`} className="sr-only">
            {t("pollManager.search")}
          </Label>
          <HugeiconsIcon icon={Search01Icon} strokeWidth={2} aria-hidden className="pointer-events-none absolute start-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input id={`${ids}-q`} className="h-8 ps-8" type="search" value={text} placeholder={t("pollManager.search")} onChange={(e) => setText(e.target.value)} />
        </form>
        {languages.length > 0 && (
          <Select value={language} onValueChange={(v) => filter({ language: String(v ?? "") })}>
            <SelectTrigger size="sm" aria-label={t("pollEditor.language")}>
              <SelectValue>{(v: string) => (v ? (languages.find((l) => l.value === v)?.label ?? v) : t("pollManager.allLanguages"))}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="">{t("pollManager.allLanguages")}</SelectItem>
              {languages.map((l) => (
                <SelectItem key={l.value} value={l.value}>
                  {l.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
        <Select value={state ?? ""} onValueChange={(v) => filter({ state: (String(v ?? "") || undefined) as PollState | undefined })}>
          <SelectTrigger size="sm" aria-label={t("pollManager.state")}>
            <SelectValue>{(v: string) => (v ? t(`pollManager.${v as PollState}`) : t("pollManager.allStates"))}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="">{t("pollManager.allStates")}</SelectItem>
            {STATES.map((s) => (
              <SelectItem key={s} value={s}>
                {t(`pollManager.${s}`)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <DateFilter label={t("pollManager.month")}>
          <input id={`${ids}-month`} className={cn(DATE_INPUT, !month && "text-muted-foreground")} type="month" value={month} onChange={(e) => filter({ month: e.target.value, date: "" })} />
        </DateFilter>
        <DateFilter label={t("pollManager.date")}>
          <input id={`${ids}-date`} className={cn(DATE_INPUT, !date && "text-muted-foreground")} type="date" value={date} onChange={(e) => filter({ date: e.target.value, month: "" })} />
        </DateFilter>
        {filtered && (
          <Button size="sm" variant="ghost" onClick={() => loc.go("")}>
            {t("pollManager.clear")}
          </Button>
        )}
      </div>
      <ErrorLine error={del.error} />
      {list.loading && !list.items.length ? (
        <Rows />
      ) : list.error && !list.items.length ? (
        <LoadFailed error={list.error} message={t("pollManager.loadFailed")} retry={list.reload} />
      ) : !list.items.length ? (
        <div className="grid justify-items-center gap-3 rounded-lg border border-dashed border-border px-6 py-12 text-center">
          <p className="text-muted-foreground">{filtered ? t("pollManager.empty") : t("pollManager.none")}</p>
          {filtered ? (
            <Button size="sm" variant="outline" onClick={() => loc.go("")}>
              {t("pollManager.clear")}
            </Button>
          ) : (
            <Button size="sm" onClick={() => loc.go("new")}>
              <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
              {t("pollManager.new")}
            </Button>
          )}
        </div>
      ) : (
        <ul className="overflow-hidden rounded-lg border border-border" style={{ "--ck-cols": COLUMNS } as CSSProperties} aria-busy={list.loading}>
          <li aria-hidden className="hidden min-h-10 items-center gap-4 border-b border-border bg-muted/40 px-3 py-1.5 text-xs font-medium text-muted-foreground md:grid md:grid-cols-(--ck-cols)" data-ckui="list-head">
            <span>{t("pollManager.question")}</span>
            <span>{t("pollManager.state")}</span>
            <span>{t("pollManager.votes")}</span>
            <span>{t("contentManager.date")}</span>
            <span />
          </li>
          {list.items.map((poll) => {
            const s = pollState(poll);
            return (
              <li
                key={poll.id}
                className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-3 py-3 transition-colors last:border-b-0 hover:bg-muted/30 md:grid md:grid-cols-(--ck-cols)"
                data-ckui="poll-row"
                data-state={s}
              >
                <div className="grid min-w-[min(100%,16rem)] flex-1 gap-0.5">
                  <button type="button" className="text-start font-medium break-words underline-offset-4 hover:underline" onClick={() => loc.go(poll.id)}>
                    {poll.question}
                  </button>
                  <span className="text-xs text-muted-foreground">
                    {languageName(poll.language)}
                  </span>
                </div>
                <div>
                  <PollStateDot state={s} />
                </div>
                <div className="text-muted-foreground tabular-nums" title={m.plural("poll.votes", poll.total_votes)}>
                  {count.format(poll.total_votes)}
                </div>
                <div className="grid text-muted-foreground">
                  <span>{s === "closed" && poll.closes_at ? t("pollManager.closedOn", { date: formatDate(m, poll.closes_at, false) }) : t(s === "scheduled" ? "pollManager.goesLive" : "pollManager.wentLive", { date: formatDate(m, poll.live_at, false) })}</span>
                  {s !== "closed" && poll.closes_at && <span className="text-xs">{t("poll.closes", { date: formatDate(m, poll.closes_at, false) })}</span>}
                </div>
                <div className="ms-auto flex items-center justify-end gap-1">
                  <Button size="sm" variant="outline" onClick={() => loc.go(poll.id)}>
                    {t("comments.edit")}
                  </Button>
                  <DropdownMenu>
                    <DropdownMenuTrigger render={<Button size="icon-sm" variant="ghost" disabled={del.pending} aria-label={t("contentManager.moreActions", { title: poll.question })} />}>
                      <HugeiconsIcon icon={MoreHorizontalIcon} strokeWidth={2} />
                    </DropdownMenuTrigger>
                    <DropdownMenuContent align="end" className="min-w-40">
                      <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => void remove(poll)}>
                        <HugeiconsIcon icon={Delete02Icon} strokeWidth={2} />
                        {t("pollEditor.delete")}
                      </DropdownMenuItem>
                    </DropdownMenuContent>
                  </DropdownMenu>
                </div>
              </li>
            );
          })}
        </ul>
      )}
      <Pager page={page} hasNext={list.hasNext} disabled={list.loading} onPage={(n) => loc.go("", { ...filters, page: n > 1 ? n : undefined })} />
      {dialog}
    </div>
  );
}

const DATE_INPUT = "min-w-0 bg-transparent px-2 text-sm outline-none";

/** A month or day filter that reads as one control: its name, then the picker. */
function DateFilter({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="flex h-8 items-stretch overflow-hidden rounded-md border border-input text-sm focus-within:border-ring focus-within:ring-3 focus-within:ring-ring/50 dark:bg-input/30">
      <span className="flex items-center border-e border-input bg-muted/50 px-2.5 text-muted-foreground">{label}</span>
      {children}
    </label>
  );
}
