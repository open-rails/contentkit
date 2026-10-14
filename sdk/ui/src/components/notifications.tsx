import { Popover } from "@base-ui/react/popover";
import { Notification01Icon, RefreshIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../appearance.js";
import type { ContentKitClient } from "../client/client.js";
import type { Item } from "../client/generated/wire.js";
import { useMessages } from "../i18n/context.js";
import { useErrorReporter, type ContentKitErrorHandler } from "../react/context.js";
import { useNotificationUnread, useNotifications, type UseNotificationsOptions } from "../react/notifications.js";
import { useContentScope } from "../react/use-resource.js";
import { ContentKitUiRoot, useScopeProps } from "../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Skeleton } from "#ckui/ui/skeleton";
import { ErrorLine, RelativeTime, useAction } from "./social/parts.js";

export interface NotificationInboxProps {
  /** Host-owned rendering for every kind, including unknown future kinds. */
  renderItem: (item: Item) => ReactNode;
  /** Domain actions/links; the inbox itself never executes a source's workflow. */
  renderActions?: (item: Item) => ReactNode;
  client?: ContentKitClient;
  pageSize?: number;
  initialFilter?: "all" | "unread" | "archived";
  refreshInterval?: UseNotificationsOptions["refreshInterval"];
  onSignIn?: () => void;
  onError?: ContentKitErrorHandler;
  appearance?: ContentKitUiAppearance;
  className?: string;
}

/** A user inbox with cursor pagination and independent local read state. */
export function NotificationInbox({ renderItem, renderActions, client, pageSize, initialFilter = "all", refreshInterval, onSignIn, onError, appearance, className }: NotificationInboxProps) {
  const { t } = useMessages();
  const heading = useId();
  const [filter, setFilter] = useState(initialFilter);
  const inbox = useNotifications({ client, pageSize, unread: filter === "unread", dismissed: filter === "archived", refreshInterval });
  const { onSignIn: providerSignIn } = useContentScope(client);
  const signIn = onSignIn ?? providerSignIn;
  const filterButton = useRef<HTMLButtonElement>(null);
  const report = useErrorReporter(onError);
  useEffect(() => {
    if (inbox.error) report(inbox.error, "notifications.load");
  }, [inbox.error, report]);
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-4 text-sm", className)} data-ckui="notification-inbox" role="region" aria-labelledby={heading}>
      <header className="flex items-center justify-between gap-3">
        <h2 id={heading} className="text-base font-semibold">{t("notifications.title")}</h2>
        {!inbox.signedOut && <Button size="icon-sm" variant="ghost" aria-label={t("notifications.refresh")} disabled={inbox.loading} onClick={inbox.reload}><HugeiconsIcon icon={RefreshIcon} strokeWidth={2} /></Button>}
      </header>
      {inbox.signedOut ? (
        <div className="grid justify-items-start gap-3 py-4 text-muted-foreground">
          <p>{t("notifications.signInHint")}</p>
          {signIn && <Button variant="outline" onClick={signIn}>{t("notifications.signIn")}</Button>}
        </div>
      ) : <>
        <div className="flex gap-1" role="group" aria-label={t("notifications.filter")}>
          {(["all", "unread", "archived"] as const).map((value) => <Button key={value} ref={filter === value ? filterButton : undefined} size="sm" variant={filter === value ? "secondary" : "ghost"} aria-pressed={filter === value} onClick={() => setFilter(value)}>{t(`notifications.${value}`)}</Button>)}
        </div>
        {inbox.error && <div className="grid justify-items-start gap-2"><ErrorLine>{t("notifications.loadFailed")}</ErrorLine><Button size="sm" variant="outline" onClick={inbox.reload}>{t("common.retry")}</Button></div>}
        {inbox.loading && !inbox.items.length ? <div role="status" aria-label={t("notifications.loading")} className="grid gap-3"><Skeleton className="h-20 w-full" /><Skeleton className="h-20 w-full" /></div> : !inbox.items.length && !inbox.error ? <p role="status" className="py-8 text-center text-muted-foreground">{t(filter === "unread" ? "notifications.emptyUnread" : "notifications.empty")}</p> : (
          <ul className="grid divide-y divide-border overflow-hidden rounded-lg border border-border" aria-busy={inbox.loading}>
            {inbox.items.map((item) => <NotificationRow key={item.id} item={item} renderItem={renderItem} renderActions={renderActions} onRead={async (id, read) => { const item = await inbox.setRead(id, read); if (filter === "unread" && read) filterButton.current?.focus(); return item; }} onDismiss={async (id, dismissed) => { const item = await inbox.setDismissed(id, dismissed); filterButton.current?.focus(); return item; }} onError={onError} />)}
          </ul>
        )}
        {inbox.hasMore && <Button variant="outline" disabled={inbox.loadingMore || inbox.loading} onClick={() => void inbox.loadMore()}>{t(inbox.loadingMore ? "notifications.loading" : "notifications.loadMore")}</Button>}
      </>}
    </ContentKitUiRoot>
  );
}

function NotificationRow({ item, renderItem, renderActions, onRead, onDismiss, onError }: Pick<NotificationInboxProps, "renderItem" | "renderActions" | "onError"> & { item: Item; onRead: (id: string, read: boolean) => Promise<Item>; onDismiss: (id: string, dismissed: boolean) => Promise<Item> }) {
  const { t } = useMessages();
  const read = useAction("notifications.read", onError);
  const dismiss = useAction("notifications.dismiss", onError);
  const label = useId();
  const status = item.status === "open" ? t("notifications.open") : item.status === "resolved" ? t("notifications.resolved") : item.status === "dismissed" ? t("notifications.dismissed") : undefined;
  return (
    <li className={cn("grid gap-3 p-4", !item.read_at && "bg-primary/5")} data-ckui="notification-item" data-unread={!item.read_at}>
      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        {!item.read_at && <Badge variant="secondary">{t("notifications.unread")}</Badge>}
        {status && <Badge variant={item.status === "open" ? "outline" : "secondary"}>{status}</Badge>}
        <RelativeTime at={item.created_at} />
      </div>
      <div id={label} className="min-w-0 break-words">{renderItem(item)}</div>
      <div className="flex flex-wrap items-center gap-2">
        {renderActions?.(item)}
        <Button size="sm" variant="ghost" aria-describedby={label} disabled={read.pending || dismiss.pending} onClick={() => void read.run(() => onRead(item.id, !item.read_at))}>{t(item.read_at ? "notifications.markUnread" : "notifications.markRead")}</Button>
        <Button size="sm" variant="ghost" aria-describedby={label} disabled={read.pending || dismiss.pending} onClick={() => void dismiss.run(() => onDismiss(item.id, !item.dismissed_at))}>{t(item.dismissed_at ? "notifications.restore" : "notifications.dismiss")}</Button>
      </div>
      <ErrorLine error={read.error ?? dismiss.error} />
    </li>
  );
}

export interface NotificationBellProps extends NotificationInboxProps {
  /** Unread indicator refresh. Default 30 seconds while the page is visible. */
  unreadRefreshInterval?: UseNotificationsOptions["refreshInterval"];
}

/** A keyboard-accessible inbox popover. Its dot means unread items exist, not a guessed count. */
export function NotificationBell({ appearance, className, unreadRefreshInterval, ...inbox }: NotificationBellProps) {
  const { t } = useMessages();
  const unread = useNotificationUnread({ client: inbox.client, refreshInterval: unreadRefreshInterval });
  const scope = useScopeProps(appearance);
  const report = useErrorReporter(inbox.onError);
  useEffect(() => {
    if (unread.error) report(unread.error, "notifications.load");
  }, [unread.error, report]);
  return (
    <Popover.Root onOpenChange={(open) => { if (open) unread.reload(); }}>
      <ContentKitUiRoot appearance={appearance} className={cn("inline-flex", className)} data-ckui="notification-bell">
        <Popover.Trigger render={<Button variant="ghost" size="icon" />} aria-label={t(unread.unread ? "notifications.unreadLabel" : "notifications.title")} className="relative">
          <HugeiconsIcon icon={Notification01Icon} strokeWidth={2} />
          {unread.unread && <span className="absolute right-1.5 top-1.5 size-2 rounded-full bg-primary ring-2 ring-background" aria-hidden="true" data-ckui="notification-unread" />}
        </Popover.Trigger>
      </ContentKitUiRoot>
      <Popover.Portal>
        <Popover.Positioner {...scope} className={cn(scope.className, "z-50")} sideOffset={8} align="end">
          <Popover.Popup className="max-h-[min(80vh,40rem)] w-[min(26rem,calc(100vw-2rem))] overflow-auto rounded-xl border border-border bg-background p-4 text-foreground shadow-lg outline-none" aria-label={t("notifications.title")}>
            <NotificationInbox {...inbox} appearance={appearance} />
          </Popover.Popup>
        </Popover.Positioner>
      </Popover.Portal>
    </Popover.Root>
  );
}
