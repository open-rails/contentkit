// @vitest-environment jsdom
import "../test/dom.js";
import { act, render, renderHook, screen, waitFor } from "@testing-library/react";
import { StrictMode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../client/client.js";
import { ResourceStore } from "./resources.js";
import { useList, useResource } from "./use-resource.js";

const storeForTest = () => new ResourceStore(createContentKitClient({ baseUrl: "https://resources.test", fetch: async () => Response.json({}) }));

it("keeps the remaining subscriber's read on its own key when the last binder changes keys", async () => {
  const store = storeForTest();
  const get = vi.fn(async (query: string) => query);
  const { result, rerender } = renderHook(({ query }) => ({
    a: useResource(store, "X", { type: "test", id: "X" }, () => get("X")),
    b: useResource(store, query, { type: "test", id: query }, () => get(query)),
  }), { initialProps: { query: "X" } });
  await waitFor(() => expect(result.current.a.data).toBe("X"));
  rerender({ query: "Y" });
  await waitFor(() => expect(result.current.b.data).toBe("Y"));
  act(() => store.invalidate((tag) => tag.id === "X"));
  await waitFor(() => expect(get).toHaveBeenCalledTimes(3));
  expect(get.mock.calls.map(([query]) => query)).toEqual(["X", "Y", "X"]);
  expect(result.current.a.data).toBe("X");
});

it("hands reads back to a remaining subscriber's latest closure without fetching on each render", async () => {
  const store = storeForTest();
  const get = vi.fn(async (label: string) => label);
  function Reader({ label, name }: { label: string; name: string }) {
    const read = useResource(store, "shared", { type: "test" }, () => get(label));
    return <span data-testid={name}>{read.data}</span>;
  }
  function View({ label, second }: { label: string; second: boolean }) {
    return <StrictMode><Reader label={label} name="remaining" />{second && <Reader label="departing" name="second" />}</StrictMode>;
  }
  const view = render(<View label="old" second />);
  await waitFor(() => expect(screen.getByTestId("remaining")).toHaveTextContent("old"));
  const initialCalls = get.mock.calls.length;
  view.rerender(<View label="latest" second />);
  expect(get).toHaveBeenCalledTimes(initialCalls);
  view.rerender(<View label="latest" second={false} />);
  act(() => store.invalidate(() => true));
  await waitFor(() => expect(screen.getByTestId("remaining")).toHaveTextContent("latest"));
  expect(get).toHaveBeenLastCalledWith("latest");
  expect(get).toHaveBeenCalledTimes(initialCalls + 1);
});

it("keeps pagination fetchers on their key after another shared subscriber moves", async () => {
  const store = storeForTest();
  const page = vi.fn(async (query: string, cursor: string | number | undefined) => ({ items: [{ id: `${query}-${cursor ?? "first"}` }], next: cursor === undefined ? "next" : null }));
  const { result, rerender } = renderHook(({ query }) => ({
    a: useList(store, "X", { type: "test", id: "X" }, (cursor) => page("X", cursor)),
    b: useList(store, query, { type: "test", id: query }, (cursor) => page(query, cursor)),
  }), { initialProps: { query: "X" } });
  await waitFor(() => expect(result.current.a.items).toEqual([{ id: "X-first" }]));
  rerender({ query: "Y" });
  await waitFor(() => expect(result.current.b.items).toEqual([{ id: "Y-first" }]));
  await act(() => result.current.a.loadMore());
  expect(page).toHaveBeenLastCalledWith("X", "next");
  expect(result.current.a.items).toEqual([{ id: "X-first" }, { id: "X-next" }]);
  expect(result.current.b.items).toEqual([{ id: "Y-first" }]);
});
