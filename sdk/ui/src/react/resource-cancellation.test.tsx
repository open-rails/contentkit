// @vitest-environment jsdom
import "../test/dom.js";
import { act, render, renderHook, screen, waitFor } from "@testing-library/react";
import { StrictMode } from "react";
import { expect, it } from "vitest";
import { createContentKitClient } from "../client/client.js";
import type { Card } from "../client/generated/wire.js";
import { useContents } from "./discovery.js";

const card = (id: string): Card => ({
  id, kind: "gallery", code: id, path: `/g/${id}`, title: id, language: "en", cover: [],
  version: { id: `${id}-version`, code: id, languages: ["en"], traits: [], has_audio: false },
  entities: {}, counts: {}, attributes: {}, access: { allowed: true, level: "public" },
});

function transport() {
  const requests: { url: URL; signal: AbortSignal; reply: (ids: string[], cursor?: string) => void }[] = [];
  const client = createContentKitClient({ baseUrl: "https://cancellation.test", fetch: (input, init) => new Promise<Response>((resolve) => {
    // Deliberately let aborted requests complete: late network results must not
    // enter the new query/visit even when a transport cannot stop the response.
    requests.push({ url: new URL(String(input)), signal: init!.signal!, reply: (ids, cursor) => resolve(Response.json({ contents: ids.map(card), next_cursor: cursor })) });
  }) });
  return { client, requests };
}

it.each(["first", "next"])("changing a query cancels its %s page and ignores its late response", async (phase) => {
  const s = transport();
  const view = renderHook(({ kind }) => useContents({ kind }, { client: s.client }), { initialProps: { kind: "X" } });
  await waitFor(() => expect(s.requests).toHaveLength(1));
  let pending: Promise<void> | undefined;
  if (phase === "next") {
    await act(async () => s.requests[0]!.reply(["X-first"], "X-next"));
    await waitFor(() => expect(view.result.current.items).toHaveLength(1));
    act(() => { pending = view.result.current.loadMore(); });
    await waitFor(() => expect(s.requests).toHaveLength(2));
    expect(s.requests[1]!.url.searchParams.get("cursor")).toBe("X-next");
  }
  const old = s.requests.at(-1)!;
  view.rerender({ kind: "Y" });
  await waitFor(() => expect(old.signal.aborted).toBe(true));
  await waitFor(() => expect(s.requests.at(-1)!.url.searchParams.get("kind")).toBe("Y"));
  await act(async () => s.requests.at(-1)!.reply(["Y-first"]));
  await waitFor(() => expect(view.result.current.items.map((c) => c.id)).toEqual(["Y-first"]));
  await act(async () => { old.reply(["X-late"]); await pending; });
  expect(view.result.current.items.map((c) => c.id)).toEqual(["Y-first"]);
  expect(view.result.current.error).toBeUndefined();
  expect(view.result.current.loadingMore).toBe(false);
});

it.each(["first", "next"])("unmount cancels the %s page and a revisit gets a fresh slot", async (phase) => {
  const s = transport();
  const useRead = () => useContents({ kind: "gallery" }, { client: s.client });
  const oldView = renderHook(useRead);
  await waitFor(() => expect(s.requests).toHaveLength(1));
  let pending: Promise<void> | undefined;
  if (phase === "next") {
    await act(async () => s.requests[0]!.reply(["old-first"], "next"));
    await waitFor(() => expect(oldView.result.current.items).toHaveLength(1));
    act(() => { pending = oldView.result.current.loadMore(); });
    await waitFor(() => expect(s.requests).toHaveLength(2));
  }
  const old = s.requests.at(-1)!;
  oldView.unmount();
  await waitFor(() => expect(old.signal.aborted).toBe(true));
  const count = s.requests.length;
  const current = renderHook(useRead);
  await waitFor(() => expect(s.requests).toHaveLength(count + 1));
  expect(current.result.current.items).toEqual([]);
  await act(async () => s.requests.at(-1)!.reply(["fresh-first"]));
  await waitFor(() => expect(current.result.current.items.map((c) => c.id)).toEqual(["fresh-first"]));
  await act(async () => { old.reply(["old-late"]); await pending; });
  expect(current.result.current.items.map((c) => c.id)).toEqual(["fresh-first"]);
  expect(current.result.current.error).toBeUndefined();
});

it("keeps a shared pending request alive for the remaining StrictMode subscriber", async () => {
  const s = transport();
  function Reader({ name }: { name: string }) {
    const list = useContents({ kind: "gallery" }, { client: s.client });
    return <span data-testid={name}>{list.items.map((c) => c.id).join(",")}</span>;
  }
  const tree = (second: boolean) => <StrictMode><Reader name="remaining" />{second && <Reader name="departing" />}</StrictMode>;
  const view = render(tree(true));
  await waitFor(() => expect(s.requests).toHaveLength(1));
  view.rerender(tree(false));
  await act(async () => {});
  expect(s.requests[0]!.signal.aborted).toBe(false);
  await act(async () => s.requests[0]!.reply(["shared"]));
  expect(await screen.findByText("shared")).toBeVisible();
  expect(s.requests).toHaveLength(1);
});
