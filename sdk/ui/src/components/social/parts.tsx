import { cn } from "cn";
import { useCallback, useState, type ReactNode } from "react";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
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

/** A content refusal or fault in words: the content wording for shared codes, else the code's. */
export function contentError(m: Translator, e: unknown): string {
  const err = toContentKitError(e);
  if (err.code === "rate_limited") return m.t("contentErrors.rate_limited", { seconds: err.retryAfter ?? 60 });
  if (err.code === "forbidden" || err.code === "unauthorized" || err.code === "not_found" || err.code === "conflict") return m.t(`contentErrors.${err.code}`);
  if (err.code === "moderation_rejected") return err.message ? m.t("comments.rejected", { reason: err.message }) : m.t("comments.rejectedNoReason");
  return m.error(err);
}

/** Runs one async action at a time: its pending state and last error, reported once. */
export function useAction(operation: SocialOperation, onError?: ContentKitErrorHandler) {
  const report = useErrorReporter(onError);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<ContentKitError | null>(null);
  const run = useCallback(
    async <T,>(fn: () => Promise<T>): Promise<T | undefined> => {
      setPending(true);
      setError(null);
      try {
        return await fn();
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
    [operation, report],
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
      {user?.avatar && <AvatarImage src={user.avatar} srcSet={user.avatar_srcset} sizes={size === "lg" ? "40px" : size === "sm" ? "24px" : "32px"} alt="" loading="lazy" />}
      <AvatarFallback aria-hidden>{(name.trim()[0] ?? "?").toUpperCase()}</AvatarFallback>
    </Avatar>
  );
}

/** A user's name, linked when the host gives a profile URL. */
export function UserName({ user, name, href, className }: { user?: PublicUser; name: string; href?: (u: PublicUser) => string | undefined; className?: string }) {
  const url = user && href?.(user);
  const cls = cn("font-medium text-foreground", className);
  return url ? (
    <a href={url} className={cn(cls, "hover:underline")}>
      {name}
    </a>
  ) : (
    <span className={cls}>{name}</span>
  );
}

/** A confirmation for a destructive action. */
export function ConfirmDialog(p: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  confirm: ReactNode;
  onConfirm: () => void;
}) {
  const { t } = useMessages();
  return (
    <AlertDialog open={p.open} onOpenChange={p.onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{p.title}</AlertDialogTitle>
          {p.description && <AlertDialogDescription>{p.description}</AlertDialogDescription>}
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            onClick={() => {
              p.onOpenChange(false);
              p.onConfirm();
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
