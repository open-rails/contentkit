import { cn } from "cn";
import { useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { AdminComment } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useFlowClient, useStaffAccess, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { useAdminComments, useGlobalCommentBan } from "../../react/comments.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { CommentBanDialog } from "../social/bans.js";
import { ConfirmDialog, ErrorLine, formatDate, RelativeTime, useAction } from "../social/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Rows, type AdminFlowProps } from "./shell.js";

export interface UserCommentStandingProps extends AdminFlowProps {
  /** The user's id. */
  user: string;
  /** Their name in headings and dialogs; else their username once known. */
  name?: string;
  /** A link to a commented item. */
  itemHref?: (item: { kind: string; id: string }) => string | undefined;
  /** Comments per page. Default 10. */
  pageSize?: number;
}

type StandingArea = "bans" | "comments";
const AREAS: readonly StandingArea[] = ["bans", "comments"];

/** Whether the caller may manage site-wide comment bans and moderate comments: what UserCommentStanding shows. */
export function useUserCommentStandingAccess(o: FlowClientOptions = {}): StaffAccess<StandingArea> {
  return useStaffAccess(AREAS, o);
}

/**
 * A user page's comment panel: the person's site-wide comment ban (ban,
 * replace, lift) and their recent comments (delete, restore), each as the
 * caller's permissions allow.
 */
export function UserCommentStanding({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: UserCommentStandingProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="user-comment-standing" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Standing {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<UserCommentStandingProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };

function Standing(p: Inner) {
  const { t } = useMessages();
  const access = useUserCommentStandingAccess({ client: p.client });
  const title = <h3 className="text-base font-semibold">{t("standing.title")}</h3>;
  if (access.loading)
    return (
      <>
        {title}
        <Rows count={2} />
      </>
    );
  if (!access.any)
    return (
      <>
        {title}
        <NoAccess>{t("standing.noAccess")}</NoAccess>
      </>
    );
  return (
    <>
      {title}
      {access.allowed.bans && <BanCard {...p} />}
      {access.allowed.comments && <Recent {...p} />}
    </>
  );
}

function BanCard({ user, name, client, onError }: Inner) {
  const m = useMessages();
  const { t } = m;
  const g = useGlobalCommentBan(user, { client });
  const [editing, setEditing] = useState(false);
  const [lifting, setLifting] = useState(false);
  const lift = useAction("ban.lift", onError);
  const who = name ?? g.ban?.user?.username ?? user;
  const ban = g.ban;
  return (
    <section className="grid gap-2 rounded-lg border border-border p-3" data-ckui="standing-ban" aria-busy={g.loading}>
      <h4 className="font-medium">{t("standing.ban")}</h4>
      {g.loading && !ban ? (
        <Skeleton className="h-10 w-full" />
      ) : g.error ? (
        <LoadFailed error={g.error} message={t("standing.banLoadFailed")} retry={g.reload} />
      ) : !ban ? (
        <div className="flex flex-wrap items-center justify-between gap-2">
          <p className="text-muted-foreground">{t("standing.notBanned")}</p>
          <Button size="sm" variant="destructive" onClick={() => setEditing(true)}>
            {t("comments.ban")}
          </Button>
        </div>
      ) : (
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="grid min-w-0 gap-0.5">
            <div>
              <Badge variant={ban.expired ? "outline" : "destructive"}>{t(ban.expired ? "bans.expired" : "bans.active")}</Badge>
            </div>
            {ban.reason && <p className="break-words">{t("bans.reason", { reason: ban.reason })}</p>}
            <p className="text-muted-foreground">
              {ban.until ? t("bans.until", { date: formatDate(m, ban.until) }) : t("bans.untilLifted")} · {t("bans.bannedAt", { date: formatDate(m, ban.banned_at, false) })}
            </p>
          </div>
          <div className="flex gap-2">
            <Button size="sm" variant="outline" onClick={() => setEditing(true)}>
              {t("bans.update")}
            </Button>
            <Button size="sm" variant="destructive" onClick={() => setLifting(true)}>
              {t("bans.lift")}
            </Button>
          </div>
        </div>
      )}
      <ErrorLine error={lift.error} />
      {editing && <CommentBanDialog open onOpenChange={setEditing} user={{ id: user, name: who }} scopes={["global"]} ban={ban} client={client} onError={onError} />}
      <ConfirmDialog
        open={lifting}
        onOpenChange={setLifting}
        title={t("bans.liftTitle", { name: who })}
        description={t("bans.liftBody")}
        confirm={t("bans.lift")}
        onConfirm={() => void lift.run(g.lift)}
      />
    </section>
  );
}

function status(c: AdminComment): "deleted" | "held" | "rejected" | "published" {
  if (c.deleted) return "deleted";
  return c.moderation === "held" ? "held" : c.moderation === "rejected" ? "rejected" : "published";
}

function Recent({ user, client, itemHref, pageSize = 10, onError }: Inner) {
  const { t } = useMessages();
  const list = useAdminComments({ author: user, pageSize, client });
  const del = useAction("comment.delete", onError);
  const restore = useAction("comment.restore", onError);
  return (
    <section className="grid gap-2" data-ckui="standing-comments">
      <h4 className="font-medium">{t("standing.comments")}</h4>
      <ErrorLine error={del.error ?? restore.error} />
      {list.loading && !list.items.length ? (
        <Rows count={2} />
      ) : list.error && !list.items.length ? (
        <LoadFailed error={list.error} message={t("moderation.loadFailed")} retry={list.reload} />
      ) : !list.items.length ? (
        <p className="text-muted-foreground">{t("standing.noComments")}</p>
      ) : (
        <ul className="grid divide-y divide-border rounded-lg border border-border">
          {list.items.map((c) => {
            const s = status(c);
            const href = itemHref?.({ kind: c.content_kind, id: c.content_id });
            return (
              <li key={c.id} className="grid gap-1.5 p-3" data-ckui="admin-comment" data-status={s}>
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
                  <RelativeTime at={c.created_at} />
                  <Badge variant={s === "published" ? "outline" : s === "held" ? "secondary" : "destructive"}>{t(`moderation.${s}`)}</Badge>
                  <span>
                    {t("moderation.on")}{" "}
                    {href ? (
                      <a className="hover:underline" href={href}>
                        {c.content_kind}/{c.content_id}
                      </a>
                    ) : (
                      `${c.content_kind}/${c.content_id}`
                    )}
                  </span>
                </div>
                <p className={cn("line-clamp-3 break-words whitespace-pre-wrap", c.deleted && "text-muted-foreground line-through")}>{c.body}</p>
                <div>
                  {c.deleted ? (
                    <Button size="sm" variant="outline" disabled={restore.pending} onClick={() => void restore.run(() => client.comments.restore(c.id))}>
                      {t("moderation.restore")}
                    </Button>
                  ) : (
                    <Button size="sm" variant="destructive" disabled={del.pending} onClick={() => void del.run(() => client.comments.delete(c.id))}>
                      {t("moderation.delete")}
                    </Button>
                  )}
                </div>
              </li>
            );
          })}
        </ul>
      )}
      {list.hasMore && (
        <div className="grid justify-items-center gap-2">
          <ErrorLine error={list.error} />
          <Button variant="outline" disabled={list.loadingMore} onClick={() => void list.loadMore()}>
            {list.error ? t("common.retry") : t("moderation.loadMore")}
          </Button>
        </div>
      )}
    </section>
  );
}

