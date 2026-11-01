import { cn } from "cn";
import { useCallback, useEffect, useId, useState, type ReactNode } from "react";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import type { ContentKitClient } from "../../client/client.js";
import type { PublicUser } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import type { Translator } from "../../i18n/messages.js";
import { useErrorReporter, type ContentKitErrorHandler, type SocialOperation } from "../../react/context.js";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "#ckui/ui/alert-dialog";
import { Avatar, AvatarFallback, AvatarImage } from "#ckui/ui/avatar";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { useGuarded } from "../../react/context.js";

const WORDED = ["forbidden", "unauthorized", "not_found", "conflict", "comments_locked", "not_joinable"] as const;
const worded = (code: string): code is (typeof WORDED)[number] => (WORDED as readonly string[]).includes(code);

/** A content refusal or fault in words: the content wording for shared codes, else the code's. */
export function contentError(m: Translator, e: unknown): string {
  const err = toContentKitError(e);
  if (err.code === "rate_limited") return m.t("contentErrors.rate_limited", { seconds: err.retryAfter ?? 60 });
  if (worded(err.code)) return m.t(`contentErrors.${err.code}`);
  if (err.code === "moderation_rejected") return err.message ? m.t("comments.rejected", { reason: err.message }) : m.t("comments.rejectedNoReason");
  return m.error(err);
}

/** Runs one async action at a time (through the flow's guard): its pending state and last error, reported once. */
export function useAction(operation: SocialOperation, onError?: ContentKitErrorHandler) {
  const report = useErrorReporter(onError);
  const guarded = useGuarded();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<ContentKitError | null>(null);
  const run = useCallback(
    async <T,>(fn: () => Promise<T>): Promise<T | undefined> => {
      setPending(true);
      setError(null);
      try {
        return await guarded(fn);
      } catch (e) {
        const err = toContentKitError(e);
        if (err.code !== "aborted") {
          setError(err);
          report(err, operation);
        }
        return undefined;
      } finally {
        setPending(false);
      }
    },
    [operation, report, guarded],
  );
  return { run, pending, error, setError };
}

/** "3 minutes ago", in the UI's language. */
export function relativeTime(m: Translator, iso: string, now = Date.now()): string {
  const s = (Date.parse(iso) - now) / 1000;
  const a = Math.abs(s);
  if (a < 45) return m.t("time.justNow");
  const f = new Intl.RelativeTimeFormat(m.language, { numeric: "auto" });
  if (a < 2700) return f.format(Math.round(s / 60), "minute");
  if (a < 79200) return f.format(Math.round(s / 3600), "hour");
  if (a < 2246400) return f.format(Math.round(s / 86400), "day");
  if (a < 28512000) return f.format(Math.round(s / 2592000), "month");
  return f.format(Math.round(s / 31536000), "year");
}

export function formatDate(m: Translator, iso: string, time = true): string {
  return new Intl.DateTimeFormat(m.language, time ? { dateStyle: "medium", timeStyle: "short" } : { dateStyle: "medium" }).format(new Date(iso));
}

/** A relative time with the full date as its title. */
export function RelativeTime({ at, className }: { at: string; className?: string }) {
  const m = useMessages();
  return (
    <time dateTime={at} title={formatDate(m, at)} className={className}>
      {relativeTime(m, at)}
    </time>
  );
}

/** A user's avatar, or their initial. */
export function UserAvatar({ user, name, size = "default" }: { user?: PublicUser; name: string; size?: "default" | "sm" | "lg" }) {
  return (
    <Avatar size={size}>
      {!user?.deleted && user?.avatar && <AvatarImage src={user.avatar} srcSet={user.avatar_srcset} sizes={size === "lg" ? "40px" : size === "sm" ? "24px" : "32px"} alt="" loading="lazy" />}
      <AvatarFallback aria-hidden>{(name.trim()[0] ?? "?").toUpperCase()}</AvatarFallback>
    </Avatar>
  );
}

/** A user's name, linked when the host gives a profile URL. */
export function UserName({ user, name, href, className }: { user?: PublicUser; name: string; href?: (u: PublicUser) => string | undefined; className?: string }) {
  if (user?.deleted) name = "<Deleted>";
  const url = user && !user.deleted && href?.(user);
  const cls = cn("font-medium text-foreground", className);
  return url ? (
    <a href={url} className={cn(cls, "hover:underline")}>
      {name}
    </a>
  ) : (
    <span className={cls}>{name}</span>
  );
}

/** A confirmation for a destructive action; with typed, the word must be typed first (acts that cannot be undone). */
export function ConfirmDialog(p: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  confirm: ReactNode;
  typed?: string;
  onConfirm: () => void;
}) {
  const { t } = useMessages();
  const id = useId();
  const [word, setWord] = useState("");
  useEffect(() => {
    if (p.open) setWord("");
  }, [p.open]);
  const ready = !p.typed || word.trim().toLowerCase() === p.typed.toLowerCase();
  return (
    <AlertDialog open={p.open} onOpenChange={p.onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{p.title}</AlertDialogTitle>
          {p.description && <AlertDialogDescription>{p.description}</AlertDialogDescription>}
        </AlertDialogHeader>
        {p.typed && (
          <div className="grid gap-1.5">
            <Label htmlFor={`${id}-typed`}>{t("admin.typeToConfirm", { word: p.typed })}</Label>
            <Input id={`${id}-typed`} value={word} autoComplete="off" onChange={(e) => setWord(e.target.value)} />
          </div>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={!ready}
            onClick={() => {
              if (!ready) return;
              p.onConfirm();
              p.onOpenChange(false);
            }}
          >
            {p.confirm}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

/** A failure line, announced politely. */
export function ErrorLine({ error, children, className }: { error?: ContentKitError | null; children?: ReactNode; className?: string }) {
  const m = useMessages();
  if (!error && !children) return null;
  return (
    <p role="alert" className={cn("text-sm text-destructive", className)}>
      {children ?? contentError(m, error)}
    </p>
  );
}

/**
 * Titles of the contents comments are on, from one staff lookup per set of
 * ids; a content the caller can't manage keeps its raw reference.
 */
export function useContentTitles(client: ContentKitClient | undefined, ids: readonly string[]): Record<string, string> {
  const key = [...new Set(ids)].sort().slice(0, 100).join(",");
  const [titles, setTitles] = useState<Record<string, string>>({});
  useEffect(() => {
    if (!client || !key) return;
    const controller = new AbortController();
    const wanted = key.split(",");
    client.contents.manage({ ids: wanted, limit: wanted.length }, controller.signal).then(
      (items) => setTitles((cur) => ({ ...cur, ...Object.fromEntries(items.map((c) => [c.id, c.title])) })),
      () => {},
    );
    return () => controller.abort();
  }, [client, key]);
  return titles;
}

/** Where a comment is: the content's title when known, else its kind and id. */
export function ContentTarget({ kind, id, title, href }: { kind: string; id: string; title?: string; href?: string }) {
  const label = title || `${kind}/${id}`;
  return href ? (
    <a className="hover:underline" href={href}>
      {label}
    </a>
  ) : (
    <>{label}</>
  );
}
