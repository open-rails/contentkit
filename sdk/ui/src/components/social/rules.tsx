import { cn } from "cn";
import { useId, useMemo, useState } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import { fromLocalInput } from "../../client/entity-fields.js";
import { useMessages } from "../../i18n/context.js";
import { useBannedWords, useIPBans } from "../../react/comments.js";
import type { ContentKitErrorHandler } from "../../react/context.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Switch } from "#ckui/ui/switch";
import { LoadFailed, Rows } from "../admin/shell.js";
import { ErrorLine, formatDate, useAction } from "./parts.js";

export interface ModerationRulesProps {
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Words split on commas and new lines, trimmed, without repeats. */
export function wordsOf(text: string): string[] {
  return [...new Set(text.split(/[,\n]/).map((w) => w.trim()).filter(Boolean))];
}

/**
 * Staff: the banned words every comment and free-text answer meets: add
 * several at once (comma-separated), mask them (the default) or refuse the
 * text, switch what one does, or unban it.
 */
export function BannedWords({ client, onError, className, appearance }: ModerationRulesProps) {
  const { t } = useMessages();
  const ids = useId();
  const rules = useBannedWords({ client });
  const save = useAction("rules.save", onError);
  const [text, setText] = useState("");
  const [reject, setReject] = useState(false);
  const [filter, setFilter] = useState("");
  const shown = useMemo(() => {
    const f = filter.trim().toLowerCase();
    return f ? rules.words.filter((w) => w.word.includes(f)) : rules.words;
  }, [rules.words, filter]);
  const add = async () => {
    const words = wordsOf(text);
    if (!words.length) return;
    const action = reject ? "reject" : "mask";
    if (await save.run(() => rules.ban(words.map((word) => ({ word, action }))))) setText("");
  };
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-4 text-sm", className)} data-ckui="banned-words">
      <form
        className="grid gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          void add();
        }}
      >
        <div className="flex flex-wrap items-end gap-2">
          <div className="grid min-w-48 flex-1 gap-1.5">
            <Label htmlFor={`${ids}-word`}>{t("moderationRules.word")}</Label>
            <Input id={`${ids}-word`} value={text} placeholder={t("moderationRules.wordPlaceholder")} onChange={(e) => setText(e.target.value)} />
          </div>
          <Button type="submit" variant="outline" disabled={save.pending || !wordsOf(text).length}>
            {t("moderationRules.ban")}
          </Button>
        </div>
        <label className="flex items-center gap-2">
          <Switch size="sm" checked={reject} onCheckedChange={setReject} />
          {t("moderationRules.rejectNew")}
        </label>
      </form>
      <p className="text-muted-foreground">{t("moderationRules.wordsHint")}</p>
      <ErrorLine error={save.error} />
      {rules.loading && !rules.words.length ? (
        <Rows />
      ) : rules.error && !rules.words.length ? (
        <LoadFailed error={rules.error} message={t("moderationRules.loadFailed")} retry={rules.reload} />
      ) : !rules.words.length ? (
        <p className="text-muted-foreground">{t("moderationRules.noWords")}</p>
      ) : (
        <>
          {rules.words.length > 10 && (
            <div className="grid max-w-xs gap-1.5">
              <Label htmlFor={`${ids}-filter`} className="sr-only">
                {t("moderationRules.filter")}
              </Label>
              <Input id={`${ids}-filter`} className="h-8" type="search" value={filter} placeholder={t("moderationRules.filter")} onChange={(e) => setFilter(e.target.value)} />
            </div>
          )}
          <ul className="grid divide-y divide-border rounded-lg border border-border" aria-busy={rules.loading}>
            {shown.map((w) => {
              const rejects = w.action === "reject";
              return (
                <li key={w.word} className="flex flex-wrap items-center gap-3 p-3" data-ckui="banned-word" data-action={w.action}>
                  <span className="min-w-0 flex-1 font-medium break-words">{w.word}</span>
                  <Badge variant={rejects ? "destructive" : "secondary"}>{t(rejects ? "moderationRules.rejects" : "moderationRules.masks")}</Badge>
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={save.pending}
                    aria-label={t(rejects ? "moderationRules.maskWord" : "moderationRules.rejectWord", { word: w.word })}
                    onClick={() => void save.run(() => rules.ban([{ word: w.word, action: rejects ? "mask" : "reject" }]))}
                  >
                    {t(rejects ? "moderationRules.mask" : "moderationRules.reject")}
                  </Button>
                  <Button size="sm" variant="destructive" disabled={save.pending} aria-label={t("moderationRules.unbanWord", { word: w.word })} onClick={() => void save.run(() => rules.unban([w.word]))}>
                    {t("moderationRules.unban")}
                  </Button>
                </li>
              );
            })}
          </ul>
        </>
      )}
    </ContentKitUiRoot>
  );
}

/**
 * Staff: the IP bans on commenting and answering polls, newest first:
 * ban an address or a network (CIDR) with a reason and an optional end, or
 * lift a ban. Holders of root:comments:moderate are never refused.
 */
export function IPBans({ client, onError, className, appearance }: ModerationRulesProps) {
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const rules = useIPBans({ client });
  const save = useAction("rules.save", onError);
  const [network, setNetwork] = useState("");
  const [reason, setReason] = useState("");
  const [until, setUntil] = useState("");
  const add = async () => {
    const net = network.trim();
    if (!net) return;
    const r = reason.trim();
    const end = fromLocalInput(until);
    if (await save.run(() => rules.ban([{ network: net, ...(r ? { reason: r } : {}), ...(end ? { until: end } : {}) }]))) {
      setNetwork("");
      setReason("");
      setUntil("");
    }
  };
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-4 text-sm", className)} data-ckui="ip-bans">
      <form
        className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto_auto] sm:items-end"
        onSubmit={(e) => {
          e.preventDefault();
          void add();
        }}
      >
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-network`}>{t("moderationRules.network")}</Label>
          <Input id={`${ids}-network`} value={network} placeholder="203.0.113.7, 203.0.113.0/24" onChange={(e) => setNetwork(e.target.value)} />
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-reason`}>{t("moderationRules.reason")}</Label>
          <Input id={`${ids}-reason`} value={reason} onChange={(e) => setReason(e.target.value)} />
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-until`}>{t("moderationRules.until")}</Label>
          <Input id={`${ids}-until`} type="datetime-local" value={until} onChange={(e) => setUntil(e.target.value)} />
        </div>
        <Button type="submit" variant="outline" disabled={save.pending || !network.trim()}>
          {t("moderationRules.ban")}
        </Button>
      </form>
      <ErrorLine error={save.error} />
      {rules.loading && !rules.bans.length ? (
        <Rows />
      ) : rules.error && !rules.bans.length ? (
        <LoadFailed error={rules.error} message={t("moderationRules.loadFailed")} retry={rules.reload} />
      ) : !rules.bans.length ? (
        <p className="text-muted-foreground">{t("moderationRules.noIPBans")}</p>
      ) : (
        <ul className="grid divide-y divide-border rounded-lg border border-border" aria-busy={rules.loading}>
          {rules.bans.map((b) => (
            <li key={b.network} className="flex flex-wrap items-start gap-3 p-3" data-ckui="ip-ban">
              <div className="grid min-w-0 flex-1 gap-0.5">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-medium break-all">{b.network}</span>
                  <Badge variant={b.expired ? "outline" : "destructive"}>{t(b.expired ? "bans.expired" : "bans.active")}</Badge>
                </div>
                {b.reason && <p className="break-words">{t("bans.reason", { reason: b.reason })}</p>}
                <p className="text-muted-foreground">
                  {b.until ? t("bans.until", { date: formatDate(m, b.until) }) : t("bans.untilLifted")} · {t("bans.bannedAt", { date: formatDate(m, b.banned_at, false) })}
                </p>
              </div>
              <Button size="sm" variant="destructive" disabled={save.pending} aria-label={t("moderationRules.liftNetwork", { network: b.network })} onClick={() => void save.run(() => rules.lift([b.network]))}>
                {t("bans.lift")}
              </Button>
            </li>
          ))}
        </ul>
      )}
    </ContentKitUiRoot>
  );
}
