import { cn } from "cn";
import { useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { ContentKitError } from "../../client/errors.js";
import { MessagesContext, useMessages } from "../../i18n/context.js";
import { createTranslator, resolveMessages, type ContentKitUiMessageBundle } from "../../i18n/messages.js";
import { useTranslator } from "../../provider.js";
import type { AdminNavigation, ContentKitAuth } from "../../react/admin.js";
import { GuardContext, useGuarded, type ContentKitErrorHandler, type ContentKitGuard } from "../../react/context.js";
import { AppearanceContext, ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { ConfirmDialog } from "../social/parts.js";

export { useGuarded, type ContentKitGuard };

/** What every drop-in admin flow takes. */
export interface AdminFlowProps {
  /** The ContentKit client; or give baseUrl and auth. Without either, the provider's. */
  client?: ContentKitClient;
  /** Where the host mounts ContentKit, with auth. */
  baseUrl?: string;
  /** The host's auth (auth-ui's AuthClient or IssuerClient): its authFetch carries the session. */
  auth?: ContentKitAuth;
  /** Runs every write (auth-ui's useStepUpGuard()). */
  guard?: ContentKitGuard;
  /** Default theme "inherit": the host's shadcn variables. */
  appearance?: ContentKitUiAppearance;
  className?: string;
  /** BCP 47; its built-in bundle loads. Default the provider's. */
  language?: string;
  messages?: ContentKitUiMessageBundle | readonly ContentKitUiMessageBundle[];
  onError?: ContentKitErrorHandler;
}

/** A multi-view flow's place: views and filters in the URL under basePath, through the host router when given. */
export interface AdminRouteProps {
  /** Where the host mounts the flow, e.g. "/admin/comments". Without it, views live in memory. */
  basePath?: string;
  /** The host router; without it the history API. */
  navigation?: AdminNavigation;
}

function Words({ language, messages, children }: { language?: string; messages?: AdminFlowProps["messages"]; children: ReactNode }) {
  const parent = useMessages();
  const own = useTranslator(language, messages);
  const layered = useMemo(
    () => (language ? own : createTranslator(resolveMessages([parent.messages as ContentKitUiMessageBundle, ...(messages ? (Array.isArray(messages) ? messages : [messages]) : [])]), undefined, parent.language)),
    [language, own, parent, messages],
  );
  return <MessagesContext.Provider value={layered}>{children}</MessagesContext.Provider>;
}

/** A flow's root: its words, its look (the host's theme unless set), the guard for writes, and the `.ckui` scope. */
export function AdminRoot({
  appearance,
  language,
  messages,
  guard,
  className,
  flow,
  children,
}: Pick<AdminFlowProps, "appearance" | "language" | "messages" | "guard" | "className"> & { flow: string; children: ReactNode }) {
  const parent = useContext(AppearanceContext);
  const look = useMemo<ContentKitUiAppearance>(
    () => ({ ...parent, ...appearance, theme: appearance?.theme ?? parent?.theme ?? "inherit", variables: { ...parent?.variables, ...appearance?.variables } }),
    [parent, appearance],
  );
  const body = (
    <AppearanceContext.Provider value={look}>
      <GuardContext.Provider value={guard}>
        <ContentKitUiRoot appearance={look} className={cn("grid gap-4 text-sm text-foreground", className)} data-ckui={flow}>
          {children}
        </ContentKitUiRoot>
      </GuardContext.Provider>
    </AppearanceContext.Provider>
  );
  return language || messages ? (
    <Words language={language} messages={messages}>
      {body}
    </Words>
  ) : (
    body
  );
}

/** Said when the caller may do nothing here. */
export function NoAccess({ children }: { children?: ReactNode }) {
  const { t } = useMessages();
  return (
    <div className="grid gap-1 rounded-lg border border-dashed border-border p-6 text-center" data-ckui="no-access" role="status">
      <p className="font-medium">{t("admin.noAccessTitle")}</p>
      <p className="text-muted-foreground">{children ?? t("admin.noAccess")}</p>
    </div>
  );
}

/** Rows loading. */
export function Rows({ count = 3 }: { count?: number }) {
  return (
    <div className="grid gap-3" aria-busy>
      {Array.from({ length: count }, (_, i) => (
        <Skeleton key={i} className="h-14 w-full" />
      ))}
    </div>
  );
}

/** A failed read with a retry; a refusal says so. */
export function LoadFailed({ error, message, retry }: { error?: ContentKitError; message: string; retry: () => void }) {
  const { t } = useMessages();
  const refused = error?.code === "forbidden" || error?.code === "unauthorized";
  return (
    <div className="grid justify-items-start gap-2" role="alert">
      <p className="text-destructive">{refused ? t(error.code === "forbidden" ? "contentErrors.forbidden" : "contentErrors.unauthorized") : message}</p>
      <Button size="sm" variant="outline" onClick={retry}>
        {t("common.retry")}
      </Button>
    </div>
  );
}

/** Previous and next around the current page number, announced as it changes. */
export interface PagerProps {
  page: number;
  hasNext: boolean;
  onPage: (page: number) => void;
  disabled?: boolean;
  /** Matching items; with truncated, a lower bound. Adds the range, page count and first/last. */
  total?: number;
  truncated?: boolean;
  pageSize?: number;
  /** Offers these page sizes when set. */
  onPageSize?: (size: number) => void;
  pageSizes?: readonly number[];
}

export function Pager({ page, hasNext, onPage, disabled, total, truncated, pageSize = 20, onPageSize, pageSizes = [20, 50, 100] }: PagerProps) {
  const { t } = useMessages();
  if (total === undefined) {
    if (page <= 1 && !hasNext) return null;
    return (
      <nav className="flex items-center justify-center gap-3" aria-label={t("admin.pages")} data-ckui="pager">
        <Button size="sm" variant="outline" disabled={disabled || page <= 1} onClick={() => onPage(page - 1)}>
          {t("admin.previous")}
        </Button>
        <span className="text-muted-foreground" aria-live="polite">
          {t("admin.page", { page })}
        </span>
        <Button size="sm" variant="outline" disabled={disabled || !hasNext} onClick={() => onPage(page + 1)}>
          {t("admin.next")}
        </Button>
      </nav>
    );
  }
  if (!total) return null;
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const from = (page - 1) * pageSize + 1;
  const to = Math.min(page * pageSize, total);
  return (
    <nav className="flex flex-wrap items-center justify-between gap-3 text-sm" aria-label={t("admin.pages")} data-ckui="pager">
      <span className="text-muted-foreground" aria-live="polite">
        {from <= to && t(truncated ? "admin.showingMore" : "admin.showing", { from, to, total })}
      </span>
      <div className="flex flex-wrap items-center gap-2">
        {onPageSize && (
          <Select value={String(pageSize)} disabled={disabled} onValueChange={(v) => v && onPageSize(Number(v))}>
            <SelectTrigger size="sm" aria-label={t("admin.perPage")}>
              <SelectValue>{(v: string) => `${t("admin.perPage")}: ${v}`}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {pageSizes.map((n) => (
                <SelectItem key={n} value={String(n)}>
                  {n}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
        {(pages > 1 || page > 1) && (
          <>
            <Button size="sm" variant="outline" disabled={disabled || page <= 1} onClick={() => onPage(1)}>
              {t("admin.first")}
            </Button>
            <Button size="sm" variant="outline" disabled={disabled || page <= 1} onClick={() => onPage(page - 1)}>
              {t("admin.previous")}
            </Button>
            <span className="text-muted-foreground">{t(truncated ? "admin.pageOfMore" : "admin.pageOf", { page, pages })}</span>
            <Button size="sm" variant="outline" disabled={disabled || !hasNext} onClick={() => onPage(page + 1)}>
              {t("admin.next")}
            </Button>
            {/* A truncated total has no known last page. */}
            <Button size="sm" variant="outline" disabled={disabled || truncated || page >= pages} onClick={() => onPage(pages)}>
              {t("admin.last")}
            </Button>
          </>
        )}
      </div>
    </nav>
  );
}

/** A positive page number from the query; 1 otherwise. */
export function pageOf(q: URLSearchParams): number {
  const n = Number(q.get("page"));
  return Number.isInteger(n) && n > 1 ? n : 1;
}

export interface ConfirmOptions {
  title: ReactNode;
  description?: ReactNode;
  confirm: ReactNode;
  /** A word to type first: for acts that cannot be undone. */
  typed?: string;
}

/** A promise-based confirmation: `if (await confirm({ … })) …`; render `dialog` once. */
export function useConfirm(): { confirm: (o: ConfirmOptions) => Promise<boolean>; dialog: ReactNode } {
  const [asking, setAsking] = useState<(ConfirmOptions & { resolve: (ok: boolean) => void }) | null>(null);
  const confirm = useCallback((o: ConfirmOptions) => new Promise<boolean>((resolve) => setAsking({ ...o, resolve })), []);
  const dialog = (
    <ConfirmDialog
      open={!!asking}
      onOpenChange={(open) => {
        if (!open && asking) {
          asking.resolve(false);
          setAsking(null);
        }
      }}
      title={asking?.title}
      description={asking?.description}
      confirm={asking?.confirm}
      typed={asking?.typed}
      onConfirm={() => {
        asking?.resolve(true);
        setAsking(null);
      }}
    />
  );
  return { confirm, dialog };
}
