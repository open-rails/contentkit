import { expect, it, vi } from "vitest";
import { createContentKitClient } from "./client.js";
import type { Item } from "./generated/wire.js";
import type { NotificationTransport } from "./notifications.js";

it("shares account inbox changes without issuing ContentKit notification requests", async () => {
  const item: Item = {
    id: "receipt", recipient: { audience: "user", id: "alice" },
    origin: { system: "contentkit", scope: "app", id: "comment.reply:B" },
    kind: "comment.reply", data: {}, created_at: "2026-10-24T00:00:00Z",
    updated_at: "2026-10-24T00:00:00Z", revision: 1,
    read_at: null, dismissed_at: null, resolved_at: null,
  };
  const listeners = new Set<() => void>();
  const transport: NotificationTransport = {
    list: vi.fn(async () => ({ items: [item], next_cursor: "" })),
    setRead: vi.fn(async () => ({ ...item, read_at: "2026-10-24T00:01:00Z" })),
    setDismissed: vi.fn(async () => ({ ...item, dismissed_at: "2026-10-24T00:02:00Z" })),
    subscribe: (listener) => { listeners.add(listener); return () => { listeners.delete(listener); }; },
    refresh: () => { for (const listener of listeners) listener(); },
  };
  const fetch = vi.fn();
  const client = createContentKitClient({ baseUrl: "/content", notifications: transport, fetch });
  const first = vi.fn(), second = vi.fn();
  const stopFirst = client.subscribe(first), stopSecond = client.subscribe(second);
  client.setReadScope("alice");
  expect(listeners.size).toBe(1);
  expect((await client.notifications.list({ unread: true })).items).toEqual([item]);
  await client.notifications.setRead("receipt", true);
  await client.notifications.setDismissed("receipt", true);
  expect(first.mock.calls.map(([change]) => change.type)).toEqual(["notifications.updated", "notifications.updated"]);
  client.notifications.refresh();
  expect(first).toHaveBeenLastCalledWith({ type: "notifications.changed" }, "alice");
  expect(second).toHaveBeenLastCalledWith({ type: "notifications.changed" }, "alice");
  expect(fetch).not.toHaveBeenCalled();
  stopFirst();
  expect(listeners.size).toBe(1);
  stopSecond(); stopSecond();
  expect(listeners.size).toBe(0);
});
