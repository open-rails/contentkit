import { MoreHorizontalIcon, ThumbsDownIcon, ThumbsUpIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { createContext, useContext, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { BanScope, Reaction, Sort } from "../../client/content/types.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import type { BanNotice as BanNoticeData, Comment, CommentBan, CommentStanding, PublicUser, RefBody } from "../../client/generated/wire.js";
import type { ContentKitUiAppearance } from "../../appearance.js";
import { useMessages } from "../../i18n/context.js";
import { useCanComment, useCommentBans, useCommentReplies, useComments, type UseComments } from "../../react/comments.js";
import { ContentKitContext, useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "#ckui/ui/dropdown-menu";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { Textarea } from "#ckui/ui/textarea";
import { BanNotice, CommentBanDialog } from "./bans.js";
import { ConfirmDialog, contentError, ErrorLine, RelativeTime, UserAvatar, UserName } from "./parts.js";

export interface CommentsProps {
  /** The commented item. */
  item: RefBody;
  /** The item's comment count, for the heading; without it the heading has none. */
  count?: number;
  /** Initial order: newest (default) or best (top). */
  sort?: Sort;
  /** Top-level comments per page. Default 20. */
  pageSize?: number;
  /** Replies per page. Default 10. */
  repliesPageSize?: number;
  /** Longest body. Default 400. */
  maxLength?: number;
  /**
   * Asks a signed-out visitor to sign in (default the provider's). Without
   * one, signed-out visitors comment under a name and react anonymously.
   */
  onSignIn?: () => void;
  /** A body as the host formats it (links, spoilers); default plain text. */
  renderBody?: (body: string, comment: Comment) => ReactNode;
  /** A profile URL for an author. */
  userHref?: (user: PublicUser) => string | undefined;
  /** The heading; false hides it. */
  heading?: ReactNode | false;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

interface Thread {
  item: RefBody;
  comments: UseComments;
  standing: CommentStanding | null;
  /** undefined: not known yet. */
  signedIn?: boolean;
  /** Runs fn, or asks to sign in first. */
  gate: (fn: () => void) => void;
  maxLength: number;
  repliesPageSize: number;
  renderBody?: CommentsProps["renderBody"];
  userHref?: CommentsProps["userHref"];
  report: (e: unknown, op: "comment.post" | "comment.edit" | "comment.delete" | "comment.react" | "comment.restore") => void;
  /** Users banned in each scope the caller holds, with the ban. */
  banned: Map<string, { scope: BanScope; ban: CommentBan }[]>;
  openBan: (user: { id: string; name: string }, ban?: CommentBan) => void;
  liftBan: (user: { id: string; name: string }) => void;
  /** A post was refused by a ban: its notice. */
  onBanned: (ban: BanNoticeData) => void;
}

const ThreadContext = createContext<Thread | null>(null);
const useThread = () => useContext(ThreadContext)!;

const mac = typeof navigator !== "undefined" && /Mac|iP(hone|ad)/.test(navigator.platform);
const TRUNCATE = 300;

const authorName = (c: Comment, anonymous: string) => c.author?.username ?? c.anon_name ?? anonymous;

/**
 * An item's comment thread: top-level comments a page at a time with
 * one-level replies, tombstones, held and rejected states shown to their
 * author, reactions, edit, delete, ban and rate-limit notices, and a sign-in
 * prompt. Moderators (as the server says) edit, delete and restore any
 * comment; an item's owner and site staff ban commenters.
 */
export function Comments(p: CommentsProps) {
  const m = useMessages();
  const { t } = m;
  const ctx = useContext(ContentKitContext);
  const [sort, setSort] = useState<Sort>(p.sort ?? "newest");
  const comments = useComments(p.item, { sort, pageSize: p.pageSize ?? 20, client: p.client });
  const { standing, reload: reloadStanding } = useCanComment(p.item, { client: p.client });
  const report = useErrorReporter(p.onError);
  const signIn = p.onSignIn ?? ctx?.onSignIn;
  const viewer = ctx?.viewer;
  const signedIn = standing ? !!standing.user_id : viewer ? true : viewer === null ? false : undefined;
  const scopes = useMemo(() => standing?.ban_scopes ?? [], [standing]);
  const ownerBans = useCommentBans({ scope: "owner", pageSize: 100, enabled: scopes.includes("owner"), client: p.client });
  const globalBans = useCommentBans({ scope: "global", pageSize: 100, enabled: scopes.includes("global"), client: p.client });
  const [banning, setBanning] = useState<{ user: { id: string; name: string }; ban?: CommentBan } | null>(null);
  const [lifting, setLifting] = useState<{ id: string; name: string } | null>(null);
  const [refusal, setRefusal] = useState<BanNoticeData | null>(null);

  const banned = useMemo(() => {
    const out = new Map<string, { scope: BanScope; ban: CommentBan }[]>();
    const add = (scope: BanScope, list: CommentBan[]) => {
      for (const ban of list) if (!ban.expired) out.set(ban.user_id, [...(out.get(ban.user_id) ?? []), { scope, ban }]);
    };
    if (scopes.includes("owner")) add("owner", ownerBans.items);
    if (scopes.includes("global")) add("global", globalBans.items);
    return out;
  }, [scopes, ownerBans.items, globalBans.items]);

  const thread: Thread = {
    item: p.item,
    comments,
    standing,
    signedIn,
    gate: (fn) => (signedIn === false && signIn ? signIn() : fn()),
    maxLength: p.maxLength ?? 400,
    repliesPageSize: p.repliesPageSize ?? 10,
    renderBody: p.renderBody,
    userHref: p.userHref,
    report: (e, op) => report(e, op),
    banned,
    openBan: (user, ban) => setBanning({ user, ban }),
    liftBan: setLifting,
    onBanned: (ban) => {
      setRefusal(ban);
      reloadStanding();
    },
  };
  const ban = standing?.ban ?? refusal;
  const canWrite = standing ? standing.can_comment : true;
  const heading = p.heading === undefined ? (p.count === undefined ? t("comments.title") : m.plural("comments.count", p.count)) : p.heading;

  return (
    <ThreadContext.Provider value={thread}>
      <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-4 text-sm", p.className)} data-ckui="comments">
        {(heading !== false || comments.items.length > 1) && (
          <div className="flex flex-wrap items-center justify-between gap-2">
            {heading !== false && <h2 className="text-base font-semibold">{heading}</h2>}
            <Select value={sort} onValueChange={(v) => setSort(v as Sort)}>
              <SelectTrigger size="sm" aria-label={t("comments.sort")} className="ms-auto">
                <SelectValue>{(v: string) => t(v === "best" ? "comments.best" : "comments.newest")}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="newest">{t("comments.newest")}</SelectItem>
                <SelectItem value="best">{t("comments.best")}</SelectItem>
              </SelectContent>
            </Select>
          </div>
        )}
        {ban ? (
          <BanNotice ban={ban} />
        ) : !canWrite ? (
          <p className="rounded-lg border border-border p-3 text-muted-foreground">{t("comments.locked")}</p>
        ) : signedIn === false && signIn ? (
          <Button variant="outline" className="justify-self-start" onClick={signIn}>
            {t("comments.signIn")}
          </Button>
        ) : (
          <Composer
            placeholder={t("comments.placeholder")}
            submit={t("comments.post")}
            askName={signedIn === false}
            onSubmit={async (body, name) => (await comments.post(body, { anonName: name }), true)}
            operation="comment.post"
          />
        )}
        <CommentList />
        {banning && (
          <CommentBanDialog
            open
            onOpenChange={(o) => !o && setBanning(null)}
            user={banning.user}
            scopes={banning.ban ? [banning.ban.scope === "global" ? "global" : "owner"] : scopes}
            ban={banning.ban}
            client={p.client}
            onError={p.onError}
          />
        )}
        <ConfirmDialog
          open={!!lifting}
          onOpenChange={(o) => !o && setLifting(null)}
          title={t("bans.liftTitle", { name: lifting?.name ?? "" })}
          description={t("bans.liftBody")}
          confirm={t("bans.lift")}
          onConfirm={() => {
            if (!lifting) return;
            for (const b of banned.get(lifting.id) ?? []) {
              void (b.scope === "global" ? globalBans : ownerBans).lift(lifting.id).catch((e) => report(e, "ban.lift"));
            }
          }}
        />
      </ContentKitUiRoot>
    </ThreadContext.Provider>
  );
}

function CommentList() {
  const { t } = useMessages();
  const { comments } = useThread();
  if (comments.loading && !comments.items.length) {
    return (
      <div className="grid gap-4" aria-busy aria-label={t("comments.loading")}>
        {[0, 1, 2].map((i) => (
          <div key={i} className="flex gap-3">
            <Skeleton className="size-8 rounded-full" />
            <div className="grid flex-1 gap-2">
              <Skeleton className="h-3 w-32" />
              <Skeleton className="h-3 w-full" />
            </div>
          </div>
        ))}
      </div>
    );
  }
  if (comments.error && !comments.items.length) {
    return (
      <div className="grid justify-items-start gap-2" data-ckui="comments-error">
        <ErrorLine>{t("comments.loadFailed")}</ErrorLine>
        <Button size="sm" variant="outline" onClick={comments.reload}>
          {t("common.retry")}
        </Button>
      </div>
    );
  }
  if (!comments.items.length) return <p className="text-muted-foreground">{t("comments.empty")}</p>;
  return (
    <>
      <ul className="grid divide-y divide-border">
        {comments.items.map((c) => (
          <li key={c.id} className="py-3 first:pt-0">
            <TopLevel comment={c} />
          </li>
        ))}
      </ul>
      {comments.error && <ErrorLine error={comments.error} />}
      {comments.hasMore && (
        <Button variant="outline" className="justify-self-center" disabled={comments.loadingMore} onClick={() => void comments.loadMore()}>
          {comments.loadingMore ? t("comments.loading") : t("comments.loadMore")}
        </Button>
      )}
    </>
  );
}

function TopLevel({ comment }: { comment: Comment }) {
  const { t, plural } = useMessages();
  const thread = useThread();
  const [open, setOpen] = useState(false);
  const [replyTo, setReplyTo] = useState<string | null>(null);
  const replies = useCommentReplies(comment.id, { pageSize: thread.repliesPageSize, enabled: open });
  const startReply = (name: string) =>
    thread.gate(() => {
      setReplyTo(name);
      if (comment.reply_count > 0) setOpen(true);
    });
  return (
    <div className="grid gap-2">
      <Row comment={comment} onReply={comment.deleted || comment.moderation ? undefined : startReply} />
      {(comment.reply_count > 0 || (open && replies.items.length > 0)) && (
        <Button variant="link" size="xs" className="ms-11 justify-self-start px-0" aria-expanded={open} onClick={() => setOpen(!open)}>
          {open ? t("comments.hideReplies") : plural("comments.showReplies", comment.reply_count)}
        </Button>
      )}
      {open && (
        <ul className="ms-11 grid gap-3 border-s border-border ps-3" aria-busy={replies.loading}>
          {replies.loading && !replies.items.length && <li className="text-muted-foreground">{t("comments.loadingReplies")}</li>}
          {replies.items.map((r) => (
            <li key={r.id}>
              <Row comment={r} onReply={r.deleted || r.moderation ? undefined : startReply} />
            </li>
          ))}
          {replies.error && (
            <li>
              <ErrorLine error={replies.error} />
            </li>
          )}
          {replies.hasMore && (
            <li>
              <Button variant="link" size="xs" className="px-0" disabled={replies.loadingMore} onClick={() => void replies.loadMore()}>
                {t("comments.moreReplies")}
              </Button>
            </li>
          )}
        </ul>
      )}
      {replyTo !== null && (
        <div className="ms-11 grid gap-1">
          <p className="text-xs text-muted-foreground">{t("comments.replyingTo", { name: replyTo })}</p>
          <Composer
            autoFocus
            placeholder={t("comments.replyPlaceholder")}
            submit={t("comments.reply")}
            askName={thread.signedIn === false}
            onCancel={() => setReplyTo(null)}
            operation="comment.post"
            onSubmit={async (body, name) => {
              await thread.comments.post(body, { replyTo: comment.id, anonName: name });
              setReplyTo(null);
              setOpen(true);
              return true;
            }}
          />
        </div>
      )}
    </div>
  );
}

function Row({ comment: c, onReply }: { comment: Comment; onReply?: (name: string) => void }) {
  const m = useMessages();
  const { t } = m;
  const thread = useThread();
  const { standing } = thread;
  const name = authorName(c, t("comments.anonymous"));
  const own = !!standing?.user_id && c.user_id === standing.user_id;
  const canEdit = !c.deleted && (own || !!standing?.moderate);
  const canBan = !c.deleted && !!c.user_id && !own && (standing?.ban_scopes.length ?? 0) > 0;
  const bans = c.user_id ? thread.banned.get(c.user_id) : undefined;
  const [editing, setEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [error, setError] = useState<ContentKitError | null>(null);
  const act = async (op: "comment.delete" | "comment.react" | "comment.restore", fn: () => Promise<unknown>) => {
    setError(null);
    try {
      await fn();
    } catch (e) {
      const err = toContentKitError(e);
      setError(err);
      thread.report(err, op);
    }
  };
  const react = (v: Reaction) => thread.gate(() => void act("comment.react", () => thread.comments.react(c, c.mine === v ? 0 : v)));
  const long = !thread.renderBody && c.body.length > TRUNCATE + 40;
  const body = long && !expanded ? c.body.slice(0, TRUNCATE).trimEnd() + "…" : c.body;

  return (
    <article className="flex gap-3" data-ckui="comment" data-comment={c.id} aria-label={name}>
      <UserAvatar user={c.deleted ? undefined : c.author} name={c.deleted ? "?" : name} />
      <div className="grid min-w-0 flex-1 gap-1">
        <header className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
          {c.deleted ? <span className="text-muted-foreground">{t("comments.anonymous")}</span> : <UserName user={c.author} name={name} href={thread.userHref} />}
          <RelativeTime at={c.created_at} className="text-xs text-muted-foreground" />
          {c.moderation === "held" && <Badge variant="secondary">{t("comments.heldBadge")}</Badge>}
          {c.moderation === "rejected" && <Badge variant="destructive">{t("comments.rejectedBadge")}</Badge>}
          {(canEdit || canBan || (c.deleted && standing?.moderate)) && (
            <DropdownMenu>
              <DropdownMenuTrigger render={<Button variant="ghost" size="icon-xs" className="ms-auto" aria-label={t("comments.actions")} />}>
                <HugeiconsIcon icon={MoreHorizontalIcon} strokeWidth={2} />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                {canEdit && <DropdownMenuItem onClick={() => setEditing(true)}>{t("comments.edit")}</DropdownMenuItem>}
                {canEdit && (
                  <DropdownMenuItem className="text-destructive" onClick={() => setDeleting(true)}>
                    {t("comments.delete")}
                  </DropdownMenuItem>
                )}
                {c.deleted && standing?.moderate && <DropdownMenuItem onClick={() => void act("comment.restore", () => thread.comments.restore(c.id))}>{t("comments.restore")}</DropdownMenuItem>}
                {canBan && !bans && <DropdownMenuItem onClick={() => thread.openBan({ id: c.user_id!, name })}>{t("comments.ban")}</DropdownMenuItem>}
                {canBan && bans && <DropdownMenuItem onClick={() => thread.liftBan({ id: c.user_id!, name })}>{t("comments.unban")}</DropdownMenuItem>}
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </header>
        {editing ? (
          <Composer
            autoFocus
            initial={c.body}
            submit={t("common.save")}
            onCancel={() => setEditing(false)}
            operation="comment.edit"
            onSubmit={async (next) => {
              if (next !== c.body) await thread.comments.edit(c.id, next);
              setEditing(false);
              return true;
            }}
          />
        ) : c.deleted ? (
          <p className="text-muted-foreground italic">{t("comments.deleted")}</p>
        ) : (
          <div className="break-words whitespace-pre-wrap">
            {thread.renderBody ? thread.renderBody(c.body, c) : body}
            {long && (
              <Button variant="link" size="xs" className="ms-1 h-auto px-0" onClick={() => setExpanded(!expanded)}>
                {expanded ? t("comments.showLess") : t("comments.showMore")}
              </Button>
            )}
          </div>
        )}
        {c.moderation === "held" && <p className="text-xs text-muted-foreground">{t("comments.held")}</p>}
        {c.moderation === "rejected" && (
          <p className="text-xs text-destructive">{c.moderation_reason ? t("comments.rejected", { reason: c.moderation_reason }) : t("comments.rejectedNoReason")}</p>
        )}
        {!c.deleted && !c.moderation && !editing && (
          <div className="-ms-2 flex flex-wrap items-center gap-1">
            <Button variant={c.mine === 1 ? "secondary" : "ghost"} size="xs" aria-pressed={c.mine === 1} aria-label={t("comments.like")} onClick={() => react(1)}>
              <HugeiconsIcon icon={ThumbsUpIcon} strokeWidth={2} />
              <span>{c.likes}</span>
            </Button>
            <Button variant={c.mine === -1 ? "secondary" : "ghost"} size="xs" aria-pressed={c.mine === -1} aria-label={t("comments.dislike")} onClick={() => react(-1)}>
              <HugeiconsIcon icon={ThumbsDownIcon} strokeWidth={2} />
              <span>{c.dislikes}</span>
            </Button>
            {onReply && (
              <Button variant="ghost" size="xs" onClick={() => onReply(name)}>
                {t("comments.reply")}
              </Button>
            )}
          </div>
        )}
        <ErrorLine error={error} />
      </div>
      <ConfirmDialog
        open={deleting}
        onOpenChange={setDeleting}
        title={t("comments.deleteTitle")}
        description={t("comments.deleteBody")}
        confirm={t("comments.delete")}
        onConfirm={() => void act("comment.delete", () => thread.comments.remove(c.id))}
      />
    </article>
  );
}

interface ComposerProps {
  initial?: string;
  placeholder?: string;
  submit: string;
  /** A signed-out visitor gives a name. */
  askName?: boolean;
  autoFocus?: boolean;
  onCancel?: () => void;
  operation: "comment.post" | "comment.edit";
  /** Resolves true to clear the field. */
  onSubmit: (body: string, name?: string) => Promise<boolean>;
}

function Composer(p: ComposerProps) {
  const m = useMessages();
  const { t } = m;
  const thread = useThread();
  const ids = useId();
  const [body, setBody] = useState(p.initial ?? "");
  const [name, setName] = useState("");
  const [pending, setPending] = useState(false);
  const [invalid, setInvalid] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const area = useRef<HTMLTextAreaElement>(null);
  const max = thread.maxLength;
  const left = max - body.length;

  const send = async () => {
    const text = body.trim();
    if (!text) return setInvalid(t("comments.required"));
    if (p.askName && !name.trim()) return setInvalid(t("comments.nameRequired"));
    setInvalid(null);
    setError(null);
    setPending(true);
    try {
      if (await p.onSubmit(text, p.askName ? name.trim() : undefined)) setBody("");
    } catch (e) {
      const err = toContentKitError(e);
      thread.report(err, p.operation);
      if (err.code === "comment_banned" && err.ban) thread.onBanned(err.ban);
      else if (err.code === "rate_limited") setError(err.retryAfter ? t("comments.rateLimited", { seconds: err.retryAfter }) : t("comments.rateLimitedSoon"));
      else setError(contentError(m, err));
    } finally {
      setPending(false);
    }
  };
  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void send();
    } else if (e.key === "Escape" && p.onCancel) p.onCancel();
  };
  return (
    <form
      className="grid gap-2"
      data-ckui="comment-composer"
      onSubmit={(e) => {
        e.preventDefault();
        void send();
      }}
    >
      {p.askName && (
        <div className="grid gap-1.5 sm:max-w-xs">
          <Label htmlFor={`${ids}-name`}>{t("comments.name")}</Label>
          <Input id={`${ids}-name`} value={name} maxLength={60} placeholder={t("comments.namePlaceholder")} onChange={(e) => setName(e.target.value)} autoComplete="nickname" />
        </div>
      )}
      <Textarea
        ref={area}
        value={body}
        rows={p.initial === undefined ? 2 : 3}
        maxLength={max}
        autoFocus={p.autoFocus}
        placeholder={p.placeholder}
        aria-label={p.placeholder ?? p.submit}
        aria-invalid={!!invalid}
        aria-describedby={`${ids}-hint`}
        disabled={pending}
        onChange={(e) => {
          setBody(e.target.value);
          if (invalid) setInvalid(null);
        }}
        onKeyDown={onKey}
      />
      <div className="flex flex-wrap items-center gap-2">
        <span id={`${ids}-hint`} className="me-auto text-xs text-muted-foreground" aria-live="polite">
          {invalid ? (
            <span className="text-destructive">{invalid}</span>
          ) : left <= max * 0.2 ? (
            m.plural("comments.charsLeft", left)
          ) : (
            <span className="pointer-coarse:hidden">{t("comments.submitHint", { key: mac ? "⌘" : "Ctrl" })}</span>
          )}
        </span>
        {p.onCancel && (
          <Button type="button" size="sm" variant="ghost" onClick={p.onCancel} disabled={pending}>
            {t("common.cancel")}
          </Button>
        )}
        <Button type="submit" size="sm" disabled={pending}>
          {pending ? t("comments.posting") : p.submit}
        </Button>
      </div>
      {error && <ErrorLine>{error}</ErrorLine>}
    </form>
  );
}
