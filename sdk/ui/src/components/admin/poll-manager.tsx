import { useEffect, useId, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { PollState } from "../../client/content/polls.js";
import type { Poll } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import { useAdminLocation, useFlowClient, useStaffAccess, type AdminLocation, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { keyOf, useContentScope, usePaged } from "../../react/use-resource.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { PollEditor } from "../social/poll-editor.js";
import { ErrorLine, formatDate, useAction } from "../social/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Pager, Rows, pageOf, useConfirm, type AdminFlowProps, type AdminRouteProps } from "./shell.js";

type Choice = string | { value: string; label: string };

export interface PollManagerProps extends AdminFlowProps, AdminRouteProps {
  /** Languages to filter by and write in: codes, or codes with labels; the first is a new poll's. */
  languages?: readonly Choice[];
  /** Offer free-text polls (the host registered an answer classifier). */
  freeText?: boolean;
  /** Most options per poll. Default 10. */
  maxOptions?: number;
  pageSize?: number;
}

/** Whether the caller may manage polls (root:polls:write). */
export function usePollManagerAccess(o: FlowClientOptions = {}): StaffAccess<"polls"> {
  return useStaffAccess(["polls"] as const, o);
}

/** A poll's state as the staff list names it. */
export function pollState(p: Poll, now = Date.now()): PollState {
  if (!p.is_active) return "inactive";
  if (Date.parse(p.live_at) > now) return "scheduled";
  if (p.closes_at && Date.parse(p.closes_at) <= now) return "closed";
  return "live";
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
  const back = (
    <Button variant="ghost" size="sm" className="justify-self-start" onClick={() => loc.go("")}>
      {t("pollManager.back")}
    </Button>
  );
  const languages = choices(p.languages, languageName);
  return (
    <>
      {back}
      {loc.view === "new" ? (
        <PollEditor
          languages={languages.length ? languages : undefined}
          freeText={p.freeText}
          maxOptions={p.maxOptions}
          client={p.client}
          onError={p.onError}
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
          <PollEditor key={loc.view} poll={loc.view} maxOptions={p.maxOptions} client={p.client} onError={p.onError} onDeleted={() => loc.go("", undefined, { replace: true })} />
        </>
      )}
    </>
  );
}

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
  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold">{t("pollManager.title")}</h2>
        <Button size="sm" onClick={() => loc.go("new")}>
          {t("pollManager.new")}
        </Button>
      </div>
      <div className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_auto]">
        <form
          className="flex gap-2"
          role="search"
          onSubmit={(e) => {
            e.preventDefault();
            filter({ q: text.trim() });
          }}
        >
          <Label htmlFor={`${ids}-q`} className="sr-only">
            {t("pollManager.search")}
          </Label>
          <Input id={`${ids}-q`} className="h-8" type="search" value={text} placeholder={t("pollManager.search")} onChange={(e) => setText(e.target.value)} />
          <Button type="submit" size="sm" variant="outline">
            {t("moderation.filter")}
          </Button>
        </form>
        <div className="flex flex-wrap gap-2">
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
        </div>
      </div>
      <div className="flex flex-wrap items-end gap-3">
        <div className="grid gap-1">
          <Label htmlFor={`${ids}-month`}>{t("pollManager.month")}</Label>
          <Input id={`${ids}-month`} className="h-8 w-44" type="month" value={month} onChange={(e) => filter({ month: e.target.value, date: "" })} />
        </div>
        <div className="grid gap-1">
          <Label htmlFor={`${ids}-date`}>{t("pollManager.date")}</Label>
          <Input id={`${ids}-date`} className="h-8 w-44" type="date" value={date} onChange={(e) => filter({ date: e.target.value, month: "" })} />
        </div>
        {(month || date || query || language || state) && (
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
        <p className="text-muted-foreground">{t("pollManager.empty")}</p>
      ) : (
        <ul className="grid divide-y divide-border rounded-lg border border-border" aria-busy={list.loading}>
          {list.items.map((poll) => {
            const s = pollState(poll);
            return (
              <li key={poll.id} className="flex flex-wrap items-start gap-3 p-3" data-ckui="poll-row" data-state={s}>
                <div className="grid min-w-0 flex-1 gap-1">
                  <button type="button" className="text-left font-medium break-words hover:underline" onClick={() => loc.go(poll.id)}>
                    {poll.question}
                  </button>
                  <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
                    <Badge variant={s === "live" || s === "inactive" ? "outline" : "secondary"} className={s === "live" ? "border-success/45 bg-success/15 text-foreground" : undefined}>{t(`pollManager.${s}`)}</Badge>
                    <span>{languageName(poll.language)}</span>
                    {poll.kind === "free_text" && <span>{t("pollEditor.freeText")}</span>}
                    {s === "closed" && poll.closes_at ? (
                      <span>{t("pollManager.closedOn", { date: formatDate(m, poll.closes_at) })}</span>
                    ) : (
                      <>
                        <span>{t(s === "scheduled" ? "pollManager.goesLive" : "pollManager.wentLive", { date: formatDate(m, poll.live_at) })}</span>
                        {poll.closes_at && <span>{t("poll.closes", { date: formatDate(m, poll.closes_at) })}</span>}
                      </>
                    )}
                    <span>{poll.kind === "free_text" ? m.plural("poll.answers", poll.answer_count ?? 0) : m.plural("poll.votes", poll.total_votes)}</span>
                  </div>
                </div>
                <div className="flex gap-2">
                  <Button size="sm" variant="outline" onClick={() => loc.go(poll.id)}>
                    {t("comments.edit")}
                  </Button>
                  <Button size="sm" variant="ghost" className="text-destructive hover:bg-destructive/10 hover:text-destructive" disabled={del.pending} onClick={() => void remove(poll)}>
                    {t("comments.delete")}
                  </Button>
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
