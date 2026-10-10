import { cn } from "cn";
import { useMemo, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { Decision, HeldKind } from "../../client/content/types.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import type { AdminComment, HeldItem, PublicUser } from "../../client/generated/wire.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { useMessages } from "../../i18n/context.js";
import { useAdminComments, useModerationQueue } from "../../react/comments.js";
import { useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "#ckui/ui/tabs";
import { CommentBanDialog, CommentBans } from "./bans.js";
import { contentError, ErrorLine, RelativeTime, UserAvatar, UserName } from "./parts.js";

export type ModerationSection = "comments" | "queue" | "bans";

export interface CommentModerationProps {
  /** Content kinds offered as a filter of the comment list: codes, or codes with labels. */
  contentKinds?: readonly (string | { value: string; label: string })[];
  /** A link to a commented item. */
  itemHref?: (item: { kind: string; id: string }) => string | undefined;
  userHref?: (user: PublicUser) => string | undefined;
  /** The sections, in order. Default all: every comment, the review queue, the site's bans. */
  sections?: readonly ModerationSection[];
  pageSize?: number;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/**
 * Staff: every comment (deleted, held and rejected ones with their text) to
 * delete, restore or ban its author; the review queue of held comments and
 * posts to approve or reject; and the site's comment bans. Each section
 * shows what the caller's permissions allow and says so when they don't.
 */
export function CommentModeration({ sections = ["comments", "queue", "bans"], className, appearance, ...p }: CommentModerationProps) {
  const { t } = useMessages();
  const [tab, setTab] = useState<ModerationSection>(sections[0] ?? "comments");
  const label: Record<ModerationSection, string> = { comments: t("moderation.all"), queue: t("moderation.queue"), bans: t("moderation.bans") };
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-4 text-sm", className)} data-ckui="comment-moderation">
      <h2 className="text-base font-semibold">{t("moderation.title")}</h2>
      <Tabs value={tab} onValueChange={(v) => setTab(v as ModerationSection)}>
        {sections.length > 1 && (
          <TabsList>
            {sections.map((s) => (
              <TabsTrigger key={s} value={s}>
                {label[s]}
              </TabsTrigger>
            ))}
          </TabsList>
        )}
        {sections.includes("comments") && (
          <TabsContent value="comments" className="pt-3">
            <AllComments {...p} />
          </TabsContent>
        )}
        {sections.includes("queue") && (
          <TabsContent value="queue" className="pt-3">
            <Queue {...p} />
          </TabsContent>
        )}
        {sections.includes("bans") && (
          <TabsContent value="bans" className="pt-3">
            <CommentBans scope="global" pageSize={p.pageSize} userHref={p.userHref} client={p.client} onError={p.onError} />
          </TabsContent>
        )}
      </Tabs>
    </ContentKitUiRoot>
  );
}

type Rest = Omit<CommentModerationProps, "sections" | "className" | "appearance">;

function status(c: AdminComment): "deleted" | "held" | "rejected" | "published" {
  if (c.deleted) return "deleted";
  return c.moderation === "held" ? "held" : c.moderation === "rejected" ? "rejected" : "published";
}

function AllComments(p: Rest) {
  const m = useMessages();
  const { t } = m;
  const kinds = useMemo(() => (p.contentKinds ?? []).map((k) => (typeof k === "string" ? { value: k, label: k } : k)), [p.contentKinds]);
  const [kind, setKind] = useState("");
  const list = useAdminComments({ contentKind: kind, pageSize: p.pageSize, client: p.client });
  const report = useErrorReporter(p.onError);
  const [error, setError] = useState<ContentKitError | null>(null);
  const [banning, setBanning] = useState<{ id: string; name: string } | null>(null);
  const act = (op: "comment.delete" | "comment.restore", fn: () => Promise<unknown>) => {
    setError(null);
    fn().catch((e) => {
      const err = toContentKitError(e);
      setError(err);
      report(err, op);
    });
  };
  return (
    <div className="grid gap-3">
      {kinds.length > 0 && (
        <Select value={kind} onValueChange={(v) => setKind(String(v ?? ""))}>
          <SelectTrigger size="sm" aria-label={t("moderation.kind")}>
            <SelectValue>{(v: string) => (v ? (kinds.find((k) => k.value === v)?.label ?? v) : t("moderation.allKinds"))}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="">{t("moderation.allKinds")}</SelectItem>
            {kinds.map((k) => (
              <SelectItem key={k.value} value={k.value}>
                {k.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
      <ErrorLine error={error} />
      <Listing loading={list.loading} error={list.error} empty={t("moderation.empty")} count={list.items.length} reload={list.reload}>
        {list.items.map((c) => {
          const name = c.author?.username ?? c.anon_name ?? t("comments.anonymous");
          const s = status(c);
          const href = p.itemHref?.({ kind: c.content_kind, id: c.content_id });
          return (
            <li key={c.id} className="grid gap-2 p-3" data-ckui="admin-comment" data-status={s}>
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                <UserAvatar user={c.author} name={name} size="sm" />
                <UserName user={c.author} name={name} href={p.userHref} />
                <RelativeTime at={c.created_at} className="text-xs text-muted-foreground" />
                <Badge variant={s === "published" ? "outline" : s === "held" ? "secondary" : "destructive"}>{t(`moderation.${s}`)}</Badge>
                {c.reply_to_id && <Badge variant="outline">{t("moderation.reply")}</Badge>}
                <span className="text-xs text-muted-foreground">
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
              <p className={cn("line-clamp-4 break-words whitespace-pre-wrap", c.deleted && "text-muted-foreground line-through")}>{c.body}</p>
              {c.moderation_reason && <p className="text-xs text-muted-foreground">{t("moderation.heldBecause", { reason: c.moderation_reason })}</p>}
              <div className="flex flex-wrap gap-2">
                {c.deleted ? (
                  <Button size="sm" variant="outline" onClick={() => act("comment.restore", () => list.restore(c.id))}>
                    {t("moderation.restore")}
                  </Button>
                ) : (
                  <Button size="sm" variant="destructive" onClick={() => act("comment.delete", () => list.remove(c.id))}>
                    {t("moderation.delete")}
                  </Button>
                )}
                {c.user_id && (
                  <Button size="sm" variant="ghost" onClick={() => setBanning({ id: c.user_id!, name })}>
                    {t("comments.ban")}
                  </Button>
                )}
              </div>
            </li>
          );
        })}
      </Listing>
      {list.hasMore && (
        <Button variant="outline" className="justify-self-center" disabled={list.loadingMore} onClick={() => void list.loadMore()}>
          {t("moderation.loadMore")}
        </Button>
      )}
      {banning && <CommentBanDialog open onOpenChange={(o) => !o && setBanning(null)} user={banning} scopes={["global"]} client={p.client} onError={p.onError} />}
    </div>
  );
}

function Queue(p: Rest) {
  const m = useMessages();
  const { t } = m;
  const [kind, setKind] = useState<HeldKind>("comment");
  const queue = useModerationQueue({ kind, pageSize: p.pageSize, client: p.client });
  return (
    <div className="grid gap-3">
      <div className="flex gap-2" role="group" aria-label={t("moderation.queue")}>
        {(["comment", "post"] as const).map((k) => (
          <Button key={k} size="sm" variant={kind === k ? "default" : "outline"} aria-pressed={kind === k} onClick={() => setKind(k)}>
            {t(k === "post" ? "moderation.queuePosts" : "moderation.queueComments")}
          </Button>
        ))}
      </div>
      <Listing loading={queue.loading} error={queue.error} empty={t("moderation.queueEmpty")} count={queue.items.length} reload={queue.reload}>
        {queue.items.map((it) => (
          <HeldRow key={it.id} item={it} itemHref={p.itemHref} userHref={p.userHref} onResolve={queue.resolve} reload={queue.reload} onError={p.onError} />
        ))}
      </Listing>
      {queue.hasMore && (
        <Button variant="outline" className="justify-self-center" disabled={queue.loadingMore} onClick={() => void queue.loadMore()}>
          {t("moderation.loadMore")}
        </Button>
      )}
    </div>
  );
}

function HeldRow(p: {
  item: HeldItem;
  itemHref?: Rest["itemHref"];
  userHref?: Rest["userHref"];
  onResolve: (item: HeldItem, d: Decision, reason?: string) => Promise<void>;
  reload: () => void;
  onError?: ContentKitErrorHandler;
}) {
  const m = useMessages();
  const { t } = m;
  const report = useErrorReporter(p.onError);
  const { item: it } = p;
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const name = it.author?.username ?? it.anon_name ?? it.author_id ?? t("comments.anonymous");
  const href = p.itemHref?.({ kind: it.ref.content_kind, id: it.ref.content_id });
  const resolve = async (d: Decision) => {
    setPending(true);
    setError(null);
    try {
      await p.onResolve(it, d, d === "reject" ? reason.trim() || undefined : undefined);
    } catch (e) {
      const err = toContentKitError(e);
      report(err, "moderation.resolve");
      // A stale revision: the text changed since it was listed.
      if (err.code === "not_found" || err.code === "conflict") {
        setError(t("moderation.stale"));
        p.reload();
      } else setError(contentError(m, err));
    } finally {
      setPending(false);
    }
  };
  return (
    <li className="grid gap-2 p-3" data-ckui="held-item">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <UserAvatar user={it.author} name={name} size="sm" />
        <UserName user={it.author} name={name} href={p.userHref} />
        <RelativeTime at={it.held_at} className="text-xs text-muted-foreground" />
        {href && (
          <a className="text-xs text-muted-foreground hover:underline" href={href}>
            {t("moderation.on")} {it.ref.content_kind}/{it.ref.content_id}
          </a>
        )}
      </div>
      {it.title && <p className="font-medium">{it.title}</p>}
      <p className="line-clamp-6 break-words whitespace-pre-wrap">{it.body}</p>
      {(it.reason || it.error) && <p className="text-xs text-muted-foreground">{t("moderation.heldBecause", { reason: it.reason || it.error || "" })}</p>}
      {rejecting && (
        <Input value={reason} maxLength={500} placeholder={t("moderation.rejectReason")} aria-label={t("moderation.rejectReason")} onChange={(e) => setReason(e.target.value)} autoFocus />
      )}
      <div className="flex flex-wrap gap-2">
        <Button size="sm" disabled={pending} onClick={() => void resolve("approve")}>
          {t("moderation.approve")}
        </Button>
        <Button size="sm" variant="destructive" disabled={pending} onClick={() => (rejecting ? void resolve("reject") : setRejecting(true))}>
          {t("moderation.reject")}
        </Button>
        {rejecting && (
          <Button size="sm" variant="ghost" onClick={() => setRejecting(false)}>
            {t("common.cancel")}
          </Button>
        )}
      </div>
      {error && <ErrorLine>{error}</ErrorLine>}
    </li>
  );
}

function Listing(p: { loading: boolean; error?: ContentKitError; empty: string; count: number; reload: () => void; children: React.ReactNode }) {
  const m = useMessages();
  const { t } = m;
  if (p.loading && !p.count) {
    return (
      <div className="grid gap-3" aria-busy>
        <Skeleton className="h-16 w-full" />
        <Skeleton className="h-16 w-full" />
      </div>
    );
  }
  if (p.error && !p.count) {
    return (
      <div className="grid justify-items-start gap-2">
        <ErrorLine>{p.error.code === "forbidden" || p.error.code === "unauthorized" ? contentError(m, p.error) : t("moderation.loadFailed")}</ErrorLine>
        <Button size="sm" variant="outline" onClick={p.reload}>
          {t("common.retry")}
        </Button>
      </div>
    );
  }
  if (!p.count) return <p className="text-muted-foreground">{p.empty}</p>;
  return <ul className="grid divide-y divide-border rounded-lg border border-border">{p.children}</ul>;
}
