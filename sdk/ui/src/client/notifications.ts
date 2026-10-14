import type { Item, Page } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call } from "./route.js";

/** One generic notice or tracked work item. */
export type NotificationItem = Item;

/** The authenticated person's shared inbox; presentation belongs to the host. */
export class NotificationsClient {
  constructor(private readonly http: Http) {}

  /** Active or archived receipts, newest first. Reading never resolves work. */
  list(o: { limit?: number; cursor?: string; unread?: boolean; dismissed?: boolean; signal?: AbortSignal } = {}): Promise<Page> {
    return call(this.http, "GET", "/inbox", { query: { limit: o.limit, cursor: o.cursor, unread: o.unread, dismissed: o.dismissed }, signal: o.signal });
  }

  /** Refresh mounted inboxes after the host receives a new-event signal. */
  refresh(): void {
    this.http.captureChanges()({ type: "notifications.changed" });
  }

  /** Local receipt state with canonical server timestamps and revision. */
  async setRead(id: string, read = true, signal?: AbortSignal): Promise<Item> {
    const changed = this.http.captureChanges();
    const item: Item = await call(this.http, read ? "PUT" : "DELETE", "/{id}/read", { params: { id }, signal });
    changed({ type: "notifications.updated", item });
    return item;
  }

  /** Archives or restores a receipt; never resolves source work. */
  async setDismissed(id: string, dismissed = true, signal?: AbortSignal): Promise<Item> {
    const changed = this.http.captureChanges();
    const item: Item = await call(this.http, dismissed ? "PUT" : "DELETE", "/{id}/dismiss", { params: { id }, signal });
    changed({ type: "notifications.updated", item });
    return item;
  }
}
