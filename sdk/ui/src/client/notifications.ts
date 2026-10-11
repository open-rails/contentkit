import type { Page } from "./generated/wire.js";
import type { Http } from "./http.js";
import { call } from "./route.js";

/** The authenticated person's shared inbox; presentation belongs to the host. */
export class NotificationsClient {
  constructor(private readonly http: Http) {}

  /** Native and imported notices, newest first. Reading does not resolve work items. */
  list(o: { limit?: number; cursor?: string; unread?: boolean; signal?: AbortSignal } = {}): Promise<Page> {
    return call(this.http, "GET", "/inbox", { query: { limit: o.limit, cursor: o.cursor, unread: o.unread }, signal: o.signal });
  }

  /** Changes local read state; it does not mark an OpenRails source notice seen. */
  async setRead(id: string, read = true, signal?: AbortSignal): Promise<void> {
    const changed = this.http.captureChanges();
    await call(this.http, read ? "PUT" : "DELETE", "/{id}/read", { params: { id }, signal });
    changed({ type: "notifications.read", id, read });
  }
}
