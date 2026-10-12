// @vitest-environment jsdom
import "../test/dom.js";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../client/client.js";
import type { Item } from "../client/generated/wire.js";
import { ContentKitProvider } from "../react/provider.js";
import { NotificationBell, NotificationInbox } from "./notifications.js";

function item(id: string, fields: Partial<Item> = {}): Item {
  return { id, recipient: { audience: "user", id: "alice" }, origin: { system: "host", scope: "site", id }, kind: "future.kind", data: { title: `Notice ${id}` }, created_at: "2026-10-11T12:00:00Z", updated_at: "2026-10-11T12:00:00Z", read_at: null, dismissed_at: null, resolved_at: null, revision: 1, ...fields };
}
const renderItem = (notice: Item) => <p>{(notice.data as { title: string }).title}</p>;

function fakeInbox(initial: Item[], failRead = false) {
  const state = { items: initial, calls: [] as string[] };
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (url, init) => {
    const u = new URL(String(url));
    state.calls.push(`${init?.method} ${u.pathname}`);
    if (init?.method !== "GET") {
      if (failRead) return Response.json({ code: "unavailable", error: "unavailable" }, { status: 503 });
      const id = u.pathname.split("/").at(-2)!;
      state.items = state.items.map((n) => n.id === id ? { ...n, revision: n.revision + 1, ...(u.pathname.endsWith("/dismiss") ? { dismissed_at: init?.method === "PUT" ? "2026-10-11T13:00:00Z" : null } : { read_at: init?.method === "PUT" ? "2026-10-11T13:00:00Z" : null }) } : n);
      return Response.json(state.items.find((n) => n.id === id));
    }
    const rows = state.items.filter((n) => (!!n.dismissed_at === (u.searchParams.get("dismissed") === "true")) && (u.searchParams.get("unread") !== "true" || !n.read_at));
    const start = Number(u.searchParams.get("cursor") ?? "0"), size = Number(u.searchParams.get("limit") ?? "20");
    return Response.json({ items: rows.slice(start, start + size), next_cursor: rows.length > start + size ? String(start + size) : undefined });
  } });
  return { client, state };
}

it("renders host content, pages the inbox, and synchronizes the unread bell without changing workflow", async () => {
  const { client, state } = fakeInbox([item("one", { action_requested: true }), item("two", { status: "open", action_requested: true }), item("three")]);
  const user = userEvent.setup();
  render(<ContentKitProvider client={client} viewer="alice"><NotificationBell renderItem={renderItem} unreadRefreshInterval={false} /><NotificationInbox renderItem={renderItem} pageSize={2} /></ContentKitProvider>);
  await screen.findByText("Notice one");
  expect(screen.getAllByText("Needs action")).toHaveLength(1);
  expect(screen.queryByText("future.kind")).toBeNull();
  expect(screen.getByRole("button", { name: "Notifications, unread items" })).toBeVisible();
  expect(state.calls.every((c) => c.startsWith("GET"))).toBe(true);
  await user.click(screen.getByRole("button", { name: "Show more" }));
  await screen.findByText("Notice three");
  for (const id of ["one", "two", "three"]) {
    const row = screen.getByText(`Notice ${id}`).closest("li")!;
    await user.click(within(row).getByRole("button", { name: "Mark read" }));
    await waitFor(() => expect(row).toHaveAttribute("data-unread", "false"));
  }
  await waitFor(() => expect(screen.getByRole("button", { name: "Notifications" })).toBeVisible());
  expect(state.items.find((n) => n.id === "two")?.status).toBe("open");
  expect(state.items.find((n) => n.id === "one")?.action_requested).toBe(true);
  await user.click(screen.getByRole("button", { name: "Unread" }));
  await screen.findByText("You're all caught up.");
});

it("keeps failed writes unread and displays a retryable failure", async () => {
  const { client, state } = fakeInbox([item("one")], true);
  const error = vi.fn();
  const user = userEvent.setup();
  render(<ContentKitProvider client={client} viewer="alice"><NotificationInbox renderItem={renderItem} onError={error} /></ContentKitProvider>);
  const row = (await screen.findByText("Notice one")).closest("li")!;
  await user.click(within(row).getByRole("button", { name: "Mark read" }));
  await within(row).findByRole("alert");
  expect(row).toHaveAttribute("data-unread", "true");
  expect(state.items[0]?.read_at).toBeNull();
  expect(error).toHaveBeenCalledWith(expect.objectContaining({ code: "unavailable" }), { operation: "notifications.read" });
});

it("does not fetch signed-out inboxes and clears a previous viewer immediately", async () => {
  let viewer: string | null = "alice";
  let completeBob: ((r: Response) => void) | undefined;
  const fetcher = vi.fn(async () => viewer === "bob" ? new Promise<Response>((resolve) => { completeBob = resolve; }) : Response.json({ items: [item("alice")] }));
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: fetcher });
  const signIn = vi.fn();
  const tree = () => <ContentKitProvider client={client} viewer={viewer} onSignIn={signIn}><NotificationInbox renderItem={renderItem} /></ContentKitProvider>;
  const { rerender } = render(tree());
  await screen.findByText("Notice alice");
  viewer = "bob"; rerender(tree());
  expect(screen.queryByText("Notice alice")).toBeNull();
  await waitFor(() => expect(completeBob).toBeDefined());
  await act(async () => { completeBob!(Response.json({ items: [item("bob")] })); });
  await screen.findByText("Notice bob");
  viewer = null; const calls = fetcher.mock.calls.length; rerender(tree());
  expect(screen.queryByText("Notice bob")).toBeNull();
  await screen.findByText("Sign in to see your notifications.");
  expect(fetcher).toHaveBeenCalledTimes(calls);
  await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
  expect(signIn).toHaveBeenCalledOnce();
});

it("does not overwrite a successful read with an older refresh response", async () => {
  let refresh = false;
  let complete: ((r: Response) => void) | undefined;
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (_url, init) => {
    if (init?.method !== "GET") return Response.json(item("one", { read_at: "2026-10-11T13:00:00Z", revision: 2 }));
    if (refresh) return new Promise<Response>((resolve) => { complete = resolve; });
    return Response.json({ items: [item("one")] });
  } });
  render(<ContentKitProvider client={client} viewer="alice"><NotificationInbox renderItem={renderItem} /></ContentKitProvider>);
  const row = (await screen.findByText("Notice one")).closest("li")!;
  refresh = true;
  act(() => client.notifications.refresh());
  await waitFor(() => expect(complete).toBeDefined());
  await userEvent.click(within(row).getByRole("button", { name: "Mark read" }));
  await waitFor(() => expect(row).toHaveAttribute("data-unread", "false"));
  await act(async () => { complete!(Response.json({ items: [item("one")] })); });
  expect(row).toHaveAttribute("data-unread", "false");
});


it("archives receipts, restores them, and preserves tracked work", async () => {
  const { client, state } = fakeInbox([item("notice"), item("task", { status: "open", action_requested: true })]);
  const user = userEvent.setup();
  render(<ContentKitProvider client={client} viewer="alice"><NotificationInbox renderItem={renderItem} /></ContentKitProvider>);
  const row = (await screen.findByText("Notice notice")).closest("li")!;
  const task = screen.getByText("Notice task").closest("li")!;
  expect(within(task).getByRole("button", { name: "Dismiss" })).toBeVisible();
  await user.click(within(row).getByRole("button", { name: "Dismiss" }));
  await waitFor(() => expect(screen.queryByText("Notice notice")).toBeNull());
  await user.click(screen.getByRole("button", { name: "Archived" }));
  const archived = (await screen.findByText("Notice notice")).closest("li")!;
  expect(state.items[0]?.read_at).toBeNull();
  expect(state.items[0]?.status).toBeUndefined();
  await user.click(within(archived).getByRole("button", { name: "Restore to inbox" }));
  await waitFor(() => expect(screen.queryByText("Notice notice")).toBeNull());
  await user.click(screen.getByRole("button", { name: "All" }));
  await screen.findByText("Notice notice");
  expect(screen.getByText("Needs action")).toBeVisible();
  const activeTask = screen.getByText("Notice task").closest("li")!;
  await user.click(within(activeTask).getByRole("button", { name: "Dismiss" }));
  await waitFor(() => expect(screen.queryByText("Notice task")).toBeNull());
  expect(state.items.find((n) => n.id === "task")?.status).toBe("open");
  await user.click(screen.getByRole("button", { name: "Archived" }));
  const archivedTask = (await screen.findByText("Notice task")).closest("li")!;
  expect(within(archivedTask).getByText("Needs action")).toBeVisible();
});

it("ignores an older successful mutation response across mounted inbox views", async () => {
  let current = item("one", { status: "open", action_requested: true });
  let finishRead: (() => void) | undefined;
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (url, init) => {
    const u = new URL(String(url));
    if (init?.method === "GET") return Response.json({ items: !!current.dismissed_at === (u.searchParams.get("dismissed") === "true") ? [current] : [] });
    if (u.pathname.endsWith("/read")) {
      current = { ...current, read_at: "2026-10-11T13:00:00Z", revision: 2 };
      const earlier = current;
      return new Promise<Response>((resolve) => { finishRead = () => resolve(Response.json(earlier)); });
    }
    current = { ...current, dismissed_at: "2026-10-11T13:01:00Z", revision: 3 };
    return Response.json(current);
  } });
  const renderActions = (notice: Item) => <span>Revision {notice.revision}</span>;
  render(<ContentKitProvider client={client} viewer="alice">
    <section data-testid="first"><NotificationInbox renderItem={renderItem} /></section>
    <section data-testid="second"><NotificationInbox renderItem={renderItem} /></section>
    <section data-testid="archived"><NotificationInbox initialFilter="archived" renderItem={renderItem} renderActions={renderActions} /></section>
  </ContentKitProvider>);
  const first = within(screen.getByTestId("first")), second = within(screen.getByTestId("second")), archive = within(screen.getByTestId("archived"));
  await first.findByText("Notice one");
  await userEvent.click(first.getByRole("button", { name: "Mark read" }));
  await waitFor(() => expect(finishRead).toBeDefined());
  await userEvent.click(second.getByRole("button", { name: "Dismiss" }));
  await archive.findByText("Revision 3");
  await act(async () => { finishRead!(); });
  expect(archive.getByText("Notice one")).toBeVisible();
  expect(archive.getByText("Revision 3")).toBeVisible();
  expect(archive.getByText("Needs action")).toBeVisible();
  expect(first.queryByText("Notice one")).toBeNull();
});
