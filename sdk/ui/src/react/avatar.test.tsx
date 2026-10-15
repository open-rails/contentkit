// @vitest-environment jsdom
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it } from "vitest";
import { createContentKitClient } from "../client/client.js";
import { ContentKitProvider } from "./provider.js";
import { useAvatar } from "./public.js";

afterEach(cleanup);
const id = "0192f000-0000-7000-8000-000000000001";
const rule = { kind: "user", name: "avatar", from: "avatar", base: "https://media.test", namespace: "accounts", to: "avatar-{w}.webp", widths: [64], default: true, stable: true };

it("refreshes a mounted self avatar after a save using the current published URL", async () => {
  let generation = 1;
  const url = () => `https://media.test/avatar-64-generation-${generation}.webp`;
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (input, options) => {
    if (String(input).endsWith("/kinds")) return Response.json({ avatars: rule });
    if (options?.method === "POST") return Response.json({ files: [] });
    return Response.json({ access: "none", expires: 0, total: 0, offset: 0, limit: 50, files: [], public: [{ preset: "avatar", renditions: [{ url: url(), w: 64, h: 64 }] }] });
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={id}>{children}</ContentKitProvider>;
  const view = renderHook(() => useAvatar(id), { wrapper });
  await waitFor(() => expect(view.result.current).not.toBeNull());
  const before = view.result.current!.renditions[0]!.url;
  generation = 2;
  await act(async () => { await client.media.commit({ kind: "user", id }, [{ op: "remove", path: "avatar.png" }]); });
  await waitFor(() => expect(view.result.current!.renditions[0]!.url).not.toBe(before));
  expect(view.result.current!.renditions[0]!.url).toBe(url());
});

it("loads current avatar publication in another app and refreshes when returning to it", async () => {
  let generation = 2;
  const url = () => `https://shared-media.test/avatar-generation-${generation}.webp`;
  const client = createContentKitClient({ baseUrl: "https://other-app.test/ck", fetch: async (input) => Response.json(String(input).endsWith("/kinds") ? { avatars: rule } : { access: "none", expires: 0, total: 0, offset: 0, limit: 50, files: [], public: [{ preset: "avatar", renditions: [{ url: url(), w: 64 }] }] }) });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={id}>{children}</ContentKitProvider>;
  const view = renderHook(() => useAvatar(id), { wrapper });
  await waitFor(() => expect(view.result.current?.renditions[0]?.url).toBe(url()));
  generation = 3;
  act(() => window.dispatchEvent(new Event("focus")));
  await waitFor(() => expect(view.result.current?.renditions[0]?.url).toBe(url()));
});

it("waits for the processed publication, then clears the avatar on removal", async () => {
  let generation = 1;
  let removed = false;
  const ref = { kind: "user", id };
  const file = { path: "avatar.png", upload: true, type: "image/png", size: 10, url: "https://media.test/private-original.png" };
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (input, options) => {
    if (String(input).endsWith("/kinds")) return Response.json({ avatars: rule });
    if (options?.method === "POST") return Response.json({ files: removed ? [] : [file] });
    return Response.json({ access: "full", expires: 0, total: removed ? 0 : 1, offset: 0, limit: 50, files: removed ? [] : [file], public: removed ? [] : [{ preset: "avatar", renditions: [{ url: `https://media.test/generation-${generation}.webp`, w: 64 }] }] });
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={id}>{children}</ContentKitProvider>;
  const views = renderHook(() => ({ first: useAvatar(id), second: useAvatar(id) }), { wrapper });
  await waitFor(() => expect(views.result.current.first?.renditions[0]?.url).toContain("generation-1"));
  await act(async () => { await client.media.commit(ref, [{ op: "edit", path: file.path, edit: { rotate: 90 } }]); });
  expect(views.result.current.second?.renditions[0]?.url).toContain("generation-1");
  generation = 2;
  await act(async () => { await client.media.waitFor(ref, file.path); });
  await waitFor(() => expect(views.result.current.first?.renditions[0]?.url).toContain("generation-2"));
  expect(views.result.current.first).toEqual(views.result.current.second);
  removed = true;
  await act(async () => { await client.media.commit(ref, [{ op: "remove", path: file.path }]); });
  await waitFor(() => expect(views.result.current.first).toBeNull());
  expect(views.result.current.second).toBeNull();
});

it("retains the last publication during an outage, without retaining it after a successful empty read", async () => {
  let unavailable = false;
  let empty = false;
  let reads = 0;
  const url = "https://media.test/current-generation.webp";
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", media: { retries: 0 }, fetch: async (input) => {
    if (String(input).endsWith("/kinds")) return Response.json({ avatars: rule });
    reads++;
    if (unavailable) return Response.json({ code: "unavailable", error: "temporary outage" }, { status: 503 });
    return Response.json({ access: "none", expires: 0, total: 0, offset: 0, limit: 50, files: [], public: empty ? [] : [{ preset: "avatar", renditions: [{ url, w: 64 }] }] });
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={id}>{children}</ContentKitProvider>;
  const view = renderHook(() => useAvatar(id), { wrapper });
  await waitFor(() => expect(view.result.current?.renditions[0]?.url).toBe(url));
  unavailable = true;
  await act(async () => { window.dispatchEvent(new Event("focus")); await new Promise((resolve) => setTimeout(resolve, 0)); });
  expect(reads).toBe(2);
  expect(view.result.current?.renditions[0]?.url).toBe(url);
  unavailable = false;
  empty = true;
  act(() => window.dispatchEvent(new Event("focus")));
  await waitFor(() => expect(view.result.current).toBeNull());
});

it("keeps other-account avatars on the stable path without per-author API reads", async () => {
  const requests: string[] = [];
  const client = createContentKitClient({ baseUrl: "https://content.test/ck", fetch: async (input) => {
    requests.push(String(input));
    return Response.json({ avatars: rule });
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer={id}>{children}</ContentKitProvider>;
  const view = renderHook(() => useAvatar("another-account"), { wrapper });
  await waitFor(() => expect(view.result.current?.renditions[0]?.url).toContain("/another-account/public/avatar-64.webp"));
  expect(requests).toHaveLength(1);
  expect(requests[0]).toMatch(/\/kinds$/);
});
