import { cn } from "cn";
import { useEffect, useId, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { BanScope } from "../../client/content/types.js";
import type { BanNotice as BanNoticeData, CommentBan, PublicUser } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitErrorHandler } from "../../react/context.js";
import { useCommentBans } from "../../react/comments.js";
import { useContentKitClient } from "../../react/context.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Alert, AlertDescription } from "#ckui/ui/alert";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "#ckui/ui/dialog";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Textarea } from "#ckui/ui/textarea";
import { ConfirmDialog, ErrorLine, formatDate, UserAvatar, UserName, useAction } from "./parts.js";

/** What a banned user is told: where, why and until when (never who). */
export function BanNotice({ ban, className }: { ban: BanNoticeData; className?: string }) {
  const m = useMessages();
  const { t } = m;
  return (
    <Alert variant="destructive" className={className} data-ckui="ban-notice">
      <AlertDescription className="grid gap-1">
        <p className="font-medium">{t(ban.scope === "global" ? "bans.globalNotice" : "bans.ownerNotice")}</p>
        {ban.reason && <p>{t("bans.reason", { reason: ban.reason })}</p>}
        <p>{ban.until ? t("bans.until", { date: formatDate(m, ban.until) }) : t("bans.untilLifted")}</p>
      </AlertDescription>
    </Alert>
  );
}

const DAY = 86_400_000;
type Duration = "day" | "week" | "month" | "forever" | "custom";
const span: Record<Duration, number> = { day: DAY, week: 7 * DAY, month: 30 * DAY, forever: 0, custom: 0 };

/** "YYYY-MM-DDTHH:mm" in local time, for a datetime-local input. */
export function localInput(d: Date): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

export interface CommentBanDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Who to ban: their id and the name to show. */
  user: { id: string; name?: string };
  /** The scopes the caller may ban in (CommentStanding.ban_scopes); the first is preselected. */
  scopes: readonly BanScope[];
  /** The current ban, when updating one. */
  ban?: CommentBan | null;
  onSaved?: (ban: CommentBan) => void;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
}

/** Bans a user from commenting, or replaces their ban: scope, duration presets or an end date, and a reason they see. */
export function CommentBanDialog({ open, onOpenChange, user, scopes, ban, onSaved, client: own, onError }: CommentBanDialogProps) {
  const m = useMessages();
  const { t } = m;
  const client = useContentKitClient(own);
  const ids = useId();
  const initialScope: BanScope = ban ? (ban.scope === "global" ? "global" : "owner") : (scopes[0] ?? "owner");
  const [scope, setScope] = useState<BanScope>(initialScope);
  const [duration, setDuration] = useState<Duration>(ban ? (ban.until ? "custom" : "forever") : "week");
  const [until, setUntil] = useState(ban?.until ? localInput(new Date(ban.until)) : localInput(new Date(Date.now() + 7 * DAY)));
  const [reason, setReason] = useState(ban?.reason ?? "");
  const { run, pending, error, setError } = useAction("ban.save", onError);
  useEffect(() => {
    if (!open) return;
    setScope(initialScope);
    setDuration(ban ? (ban.until ? "custom" : "forever") : "week");
    setUntil(ban?.until ? localInput(new Date(ban.until)) : localInput(new Date(Date.now() + 7 * DAY)));
    setReason(ban?.reason ?? "");
    setError(null);
    // Reset when it opens.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);
  const name = user.name ?? user.id;
  const save = async () => {
    const end = duration === "forever" ? undefined : duration === "custom" ? new Date(until) : new Date(Date.now() + span[duration]);
    const saved = await run(() => client.bans.ban(scope, user.id, { reason: reason.trim() || undefined, until: end?.toISOString() }));
    if (saved) {
      onSaved?.(saved);
      onOpenChange(false);
    }
  };
  const durations: Duration[] = ["day", "week", "month", "forever", "custom"];
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent data-ckui="comment-ban-dialog">
        <DialogHeader>
          <DialogTitle>{t("bans.title", { name })}</DialogTitle>
          <DialogDescription>{t(scope === "global" ? "bans.globalHelp" : "bans.ownerHelp")}</DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            void save();
          }}
        >
          {scopes.length > 1 && !ban && (
            <fieldset className="grid gap-2">
              <legend className="mb-1 text-sm font-medium">{t("bans.scope")}</legend>
              <div className="flex flex-wrap gap-2">
                {scopes.map((s) => (
                  <Button key={s} type="button" size="sm" variant={scope === s ? "default" : "outline"} aria-pressed={scope === s} onClick={() => setScope(s)}>
                    {t(s === "global" ? "bans.scopeGlobal" : "bans.scopeOwner")}
                  </Button>
                ))}
              </div>
            </fieldset>
          )}
          <fieldset className="grid gap-2">
            <legend className="mb-1 text-sm font-medium">{t("bans.duration")}</legend>
            <div className="flex flex-wrap gap-2">
              {durations.map((d) => (
                <Button key={d} type="button" size="sm" variant={duration === d ? "default" : "outline"} aria-pressed={duration === d} onClick={() => setDuration(d)}>
                  {t(`bans.${d}`)}
                </Button>
              ))}
            </div>
            {duration === "custom" && (
              <div className="grid gap-1.5">
                <Label htmlFor={`${ids}-until`}>{t("bans.endsAt")}</Label>
                <Input id={`${ids}-until`} type="datetime-local" required value={until} min={localInput(new Date())} onChange={(e) => setUntil(e.target.value)} />
              </div>
            )}
          </fieldset>
          <div className="grid gap-1.5">
            <Label htmlFor={`${ids}-reason`}>{t("bans.reasonLabel")}</Label>
            <Textarea id={`${ids}-reason`} maxLength={500} rows={3} placeholder={t("bans.reasonPlaceholder")} value={reason} onChange={(e) => setReason(e.target.value)} />
          </div>
          <ErrorLine error={error} />
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              {t("common.cancel")}
            </Button>
            <Button type="submit" variant="destructive" disabled={pending}>
              {pending ? t("common.saving") : t(ban ? "bans.update" : "bans.save")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export interface CommentBansProps {
  /** "global": the site's bans (CommentBan); "owner": the caller's own. Default "global". */
  scope?: BanScope;
  pageSize?: number;
  userHref?: (user: PublicUser) => string | undefined;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** A scope's comment bans, newest first: who, why, until when; ban a user by id, update or lift a ban. */
export function CommentBans({ scope = "global", pageSize = 25, userHref, client, onError, className, appearance }: CommentBansProps) {
  const m = useMessages();
  const { t } = m;
  const bans = useCommentBans({ scope, pageSize, client });
  const ids = useId();
  const [editing, setEditing] = useState<{ user: { id: string; name?: string }; ban?: CommentBan } | null>(null);
  const [lifting, setLifting] = useState<CommentBan | null>(null);
  const [userId, setUserId] = useState("");
  const lift = useAction("ban.lift", onError);
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-4 text-sm", className)} data-ckui="comment-bans">
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (userId.trim()) setEditing({ user: { id: userId.trim() } });
        }}
      >
        <div className="grid min-w-48 flex-1 gap-1.5">
          <Label htmlFor={`${ids}-user`}>{t("bans.user")}</Label>
          <Input id={`${ids}-user`} value={userId} placeholder={t("bans.userPlaceholder")} onChange={(e) => setUserId(e.target.value)} />
        </div>
        <Button type="submit" variant="outline" disabled={!userId.trim()}>
          {t("bans.add")}
        </Button>
      </form>
      <ErrorLine error={lift.error} />
      {bans.error && !bans.items.length ? (
        <div className="grid justify-items-start gap-2">
          <ErrorLine>{t("bans.loadFailed")}</ErrorLine>
          <Button size="sm" variant="outline" onClick={bans.reload}>
            {t("common.retry")}
          </Button>
        </div>
      ) : !bans.loading && !bans.items.length ? (
        <p className="text-muted-foreground">{t("bans.empty")}</p>
      ) : (
        <ul className="grid divide-y divide-border rounded-lg border border-border" aria-busy={bans.loading}>
          {bans.items.map((b) => {
            const name = b.user?.username ?? b.user_id;
            return (
              <li key={b.user_id} className="flex flex-wrap items-start gap-3 p-3" data-ckui="comment-ban">
                <UserAvatar user={b.user} name={name} />
                <div className="grid min-w-0 flex-1 gap-0.5">
                  <div className="flex flex-wrap items-center gap-2">
                    <UserName user={b.user} name={name} href={userHref} />
                    <Badge variant={b.expired ? "outline" : "destructive"}>{t(b.expired ? "bans.expired" : "bans.active")}</Badge>
                  </div>
                  {b.reason && <p className="break-words">{t("bans.reason", { reason: b.reason })}</p>}
                  <p className="text-muted-foreground">
                    {b.until ? t("bans.until", { date: formatDate(m, b.until) }) : t("bans.untilLifted")} · {t("bans.bannedAt", { date: formatDate(m, b.banned_at, false) })}
                  </p>
                </div>
                <div className="flex gap-2">
                  <Button size="sm" variant="outline" onClick={() => setEditing({ user: { id: b.user_id, name }, ban: b })}>
                    {t("bans.update")}
                  </Button>
                  <Button size="sm" variant="destructive" onClick={() => setLifting(b)}>
                    {t("bans.lift")}
                  </Button>
                </div>
              </li>
            );
          })}
        </ul>
      )}
      {bans.hasMore && (
        <Button variant="outline" className="justify-self-center" disabled={bans.loadingMore} onClick={() => void bans.loadMore()}>
          {t("moderation.loadMore")}
        </Button>
      )}
      {editing && (
        <CommentBanDialog
          open
          onOpenChange={(o) => !o && setEditing(null)}
          user={editing.user}
          scopes={[scope]}
          ban={editing.ban}
          client={client}
          onError={onError}
          onSaved={() => setUserId("")}
        />
      )}
      <ConfirmDialog
        open={!!lifting}
        onOpenChange={(o) => !o && setLifting(null)}
        title={t("bans.liftTitle", { name: lifting?.user?.username ?? lifting?.user_id ?? "" })}
        description={t("bans.liftBody")}
        confirm={t("bans.lift")}
        onConfirm={() => lifting && void lift.run(() => bans.lift(lifting.user_id))}
      />
    </ContentKitUiRoot>
  );
}
