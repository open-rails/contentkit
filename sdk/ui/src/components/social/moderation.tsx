import { cn } from "cn";
import { useEffect, useId, useMemo, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { Decision, HeldKind } from "../../client/content/types.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import type { AdminComment, HeldItem, PublicUser } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useAdminLocation, useFlowClient, useStaffAccess, type AdminLocation, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { useModerationQueue } from "../../react/comments.js";
import { useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { keyOf, useContentScope, usePaged } from "../../react/use-resource.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "#ckui/ui/tabs";
import { AdminRoot, NoAccess, Pager, Rows, pageOf, useGuarded, type AdminFlowProps, type AdminRouteProps } from "../admin/shell.js";
import { CommentBanDialog, CommentBans } from "./bans.js";
import { contentError, ErrorLine, RelativeTime, UserAvatar, UserName } from "./parts.js";

export type ModerationSection = "comments" | "queue" | "bans";

export interface CommentModerationProps extends AdminFlowProps, AdminRouteProps {
  /** Content kinds offered as a filter of the comment list: codes, or codes with labels. */
  contentKinds?: readonly (string | { value: string; label: string })[];
  /** A link to a commented item. */
  itemHref?: (item: { kind: string; id: string }) => string | undefined;
  userHref?: (user: PublicUser) => string | undefined;
  /** The sections, in order; the first is at basePath, the others at basePath/{section}. Default all. */
  sections?: readonly ModerationSection[];
  pageSize?: number;
}

const ALL_SECTIONS: readonly ModerationSection[] = ["comments", "queue", "bans"];

/** Which moderation sections the caller may use: every comment, the review queue, the site's bans. */
export function useCommentModerationAccess(o: FlowClientOptions = {}): StaffAccess<ModerationSection> {
  return useStaffAccess(ALL_SECTIONS, o);
}

/**
 * Staff: every comment (deleted, held and rejected ones with their text) to
 * delete, restore or ban its author, by content kind or author; the review
 * queue of held comments and posts to approve or reject; and the site's
 * comment bans. Sections, filters and pages live in the URL under basePath;
 * the caller sees only the sections its permissions allow.
 */
export function CommentModeration({ client: own, baseUrl, auth, guard, appearance, className, language, messages, sections = ALL_SECTIONS, ...p }: CommentModerationProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="comment-moderation" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Moderation {...p} client={client} sections={sections} />
    </AdminRoot>
  );
}

type Rest = Omit<CommentModerationProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages" | "sections" | "basePath" | "navigation"> & {
  client: ContentKitClient;
};

function Moderation({ sections, basePath, navigation, ...p }: Rest & { sections: readonly ModerationSection[]; basePath?: string; navigation?: CommentModerationProps["navigation"] }) {
  const { t } = useMessages();
  const loc = useAdminLocation(basePath, navigation);
  const access = useCommentModerationAccess({ client: p.client });
  const allowed = sections.filter((s) => access.allowed[s]);
  const label: Record<ModerationSection, string> = { comments: t("moderation.all"), queue: t("moderation.queue"), bans: t("moderation.bans") };
  const title = <h2 className="text-base font-semibold">{t("moderation.title")}</h2>;
  if (access.loading)
    return (
      <>
        {title}
        <Rows />
      </>
    );
  if (!allowed.length)
    return (
      <>
        {title}
        <NoAccess>{t("moderation.noAccess")}</NoAccess>
      </>
    );
  const viewOf = (s: ModerationSection) => (s === sections[0] ? "" : s);
  const asked = (loc.view || sections[0]) as ModerationSection;
  const section = allowed.includes(asked) ? asked : allowed[0]!;
  const view = viewOf(section);
  return (
    <>
      {title}
      <Tabs value={section} onValueChange={(v) => loc.go(viewOf(v as ModerationSection))}>
        {allowed.length > 1 && (
          <TabsList>
            {allowed.map((s) => (
              <TabsTrigger key={s} value={s}>
                {label[s]}
              </TabsTrigger>
            ))}
          </TabsList>
        )}
        <TabsContent value={section} className="pt-3">
          {section === "comments" && <AllComments {...p} loc={loc} view={view} />}
          {section === "queue" && <Queue {...p} loc={loc} view={view} />}
          {section === "bans" && (
            <CommentBans
              scope="global"
              pageSize={p.pageSize}
              page={pageOf(loc.query)}
              onPageChange={(n) => loc.go(view, { page: n > 1 ? n : undefined })}
              userHref={p.userHref}
              client={p.client}
              onError={p.onError}
            />
          )}
        </TabsContent>
      </Tabs>
    </>
  );
}

type SectionProps = Rest & { loc: AdminLocation; view: string };

function status(c: AdminComment): "deleted" | "held" | "rejected" | "published" {
  if (c.deleted) return "deleted";
  return c.moderation === "held" ? "held" : c.moderation === "rejected" ? "rejected" : "published";
}

function AllComments({ loc, view, client, ...p }: SectionProps) {
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const kinds = useMemo(() => (p.contentKinds ?? []).map((k) => (typeof k === "string" ? { value: k, label: k } : k)), [p.contentKinds]);
  const kind = loc.query.get("kind") ?? "";
  const author = loc.query.get("author") ?? "";
  const page = pageOf(loc.query);
  const size = p.pageSize ?? 25;
  const filter = (next: { kind?: string; author?: string }) => loc.go(view, { kind, author, ...next });
  const [authorInput, setAuthorInput] = useState(author);
  useEffect(() => setAuthorInput(author), [author]);
  const { store, scope } = useContentScope(client);
  const list = usePaged(store, keyOf("admin-comments-page", scope, kind, author, size), { type: "admin-comments" }, page, size, (q, signal) =>
    client.comments.adminList({ ...q, contentKind: kind || undefined, author: author || undefined }, signal),
  );
  const report = useErrorReporter(p.onError);
  const guarded = useGuarded();
  const [error, setError] = useState<ContentKitError | null>(null);
  const [banning, setBanning] = useState<{ id: string; name: string } | null>(null);
  const act = (op: "comment.delete" | "comment.restore", fn: () => Promise<unknown>) => {
    setError(null);
    guarded(fn).catch((e) => {
      const err = toContentKitError(e);
      setError(err);
      report(err, op);
    });
  };
  const byName = author ? (list.items.find((c) => c.user_id === author)?.author?.username ?? author) : "";
  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-end gap-2">
        {kinds.length > 0 && (
          <Select value={kind} onValueChange={(v) => filter({ kind: String(v ?? "") })}>
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
        <form
          className="flex min-w-56 flex-1 items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            filter({ author: authorInput.trim() });
          }}
        >
          <div className="grid flex-1 gap-1">
            <Label htmlFor={`${ids}-author`} className="sr-only">
              {t("moderation.author")}
            </Label>
            <Input id={`${ids}-author`} className="h-8" value={authorInput} placeholder={t("moderation.authorPlaceholder")} onChange={(e) => setAuthorInput(e.target.value)} />
          </div>
          <Button type="submit" size="sm" variant="outline">
            {t("moderation.filter")}
          </Button>
        </form>
      </div>
      {author && (
        <div className="flex flex-wrap items-center gap-2" data-ckui="author-filter">
          <span>{t("moderation.byAuthor", { name: byName })}</span>
          <Button size="sm" variant="ghost" onClick={() => filter({ author: "" })}>
            {t("moderation.clearAuthor")}
          </Button>
        </div>
      )}
      <ErrorLine error={error} />
      <Listing loading={list.loading} error={list.error} empty={t("moderation.empty")} count={list.items.length} reload={list.reload}>
        {list.items.map((c) => {
          const name = c.author?.username ?? c.anon_name ?? c.user_id ?? t("comments.anonymous");
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
                  <Button size="sm" variant="outline" onClick={() => act("comment.restore", () => client.comments.restore(c.id))}>
                    {t("moderation.restore")}
                  </Button>
                ) : (
                  <Button size="sm" variant="destructive" onClick={() => act("comment.delete", () => client.comments.delete(c.id))}>
                    {t("moderation.delete")}
                  </Button>
                )}
                {c.user_id && c.user_id !== author && (
                  <Button size="sm" variant="ghost" onClick={() => filter({ author: c.user_id! })}>
                    {t("moderation.theirComments")}
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
      <Pager page={page} hasNext={list.hasNext} disabled={list.loading} onPage={(n) => loc.go(view, { kind, author, page: n > 1 ? n : undefined })} />
      {banning && <CommentBanDialog open onOpenChange={(o) => !o && setBanning(null)} user={banning} scopes={["global"]} client={client} onError={p.onError} />}
    </div>
  );
}

function Queue({ loc, view, ...p }: SectionProps) {
  const m = useMessages();
  const { t } = m;
  const kind: HeldKind = loc.query.get("kind") === "post" ? "post" : "comment";
  const queue = useModerationQueue({ kind, pageSize: p.pageSize, client: p.client });
  return (
    <div className="grid gap-3">
      <div className="flex gap-2" role="group" aria-label={t("moderation.queue")}>
        {(["comment", "post"] as const).map((k) => (
          <Button key={k} size="sm" variant={kind === k ? "default" : "outline"} aria-pressed={kind === k} onClick={() => loc.go(view, { kind: k === "comment" ? undefined : k })}>
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
  const guarded = useGuarded();
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
      await guarded(() => p.onResolve(it, d, d === "reject" ? reason.trim() || undefined : undefined));
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
