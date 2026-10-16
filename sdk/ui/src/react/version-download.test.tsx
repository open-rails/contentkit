// @vitest-environment jsdom
import "../test/dom.js";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../client/index.js";
import { ContentKitProvider, useVersionDownload } from "./index.js";

function hook(answer: Response) {
  const client = createContentKitClient({ baseUrl: "https://api.test", fetch: vi.fn(async () => answer) as typeof globalThis.fetch });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client}>{children}</ContentKitProvider>;
  return renderHook(() => useVersionDownload("c1", "v1", { start: () => {} }), { wrapper });
}

it("tells a quota refusal's wait from Retry-After, not a generic failure", async () => {
  const r = hook(new Response(JSON.stringify({ error: "download quota reached", code: "rate_limited" }), { status: 429, headers: { "Content-Type": "application/json", "Retry-After": "90" } }));
  await act(() => r.result.current.download().catch(() => {}));
  expect(r.result.current.error?.code).toBe("rate_limited");
  expect(r.result.current.message).toMatch(/try again in 2 min/i);
  expect(r.result.current.retryAt).toBeGreaterThan(Date.now() + 80_000);
});

it("tells when a download the site holds back opens", async () => {
  const at = new Date(Date.now() + 2.2 * 86_400_000).toISOString();
  const r = hook(new Response(JSON.stringify({ error: "x", code: "download_unavailable", reason: "release_window", available_at: at }), { status: 403, headers: { "Content-Type": "application/json" } }));
  await act(() => r.result.current.download().catch(() => {}));
  expect(r.result.current.error?.reason).toBe("release_window");
  expect(r.result.current.message).toMatch(/available to download in 3 days/i);
});


afterEach(() => vi.useRealTimers());

it("cancels pending polling when its reader leaves", async () => {
  vi.useFakeTimers();
  const client = createContentKitClient({ baseUrl: "https://api.test" });
  const request = vi.spyOn(client.versions, "download").mockResolvedValue({ status: "pending" });
  const start = vi.fn();
  const view = renderHook(() => useVersionDownload("c1", "v1", { client, poll: 20, start }));
  let pending: Promise<unknown>;
  await act(async () => { pending = view.result.current.download().catch((e: unknown) => e); await Promise.resolve(); });
  expect(request).toHaveBeenCalledOnce();
  view.unmount();
  await act(async () => { await vi.advanceTimersByTimeAsync(100); });
  expect(request).toHaveBeenCalledOnce();
  expect(start).not.toHaveBeenCalled();
  expect(await pending!).toMatchObject({ code: "aborted" });
});

it("ignores a late download grant after the version changes", async () => {
  const client = createContentKitClient({ baseUrl: "https://api.test" });
  let finish!: (value: never) => void;
  const request = vi.spyOn(client.versions, "download").mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
  const start = vi.fn();
  const view = renderHook(({ version }) => useVersionDownload("c1", version, { client, start }), { initialProps: { version: "v1" } });
  let pending: Promise<unknown>;
  await act(async () => { pending = view.result.current.download().catch((e: unknown) => e); });
  view.rerender({ version: "v2" });
  expect(view.result.current.preparing).toBe(false);
  expect(request.mock.calls[0]?.[2]?.aborted).toBe(true);
  await act(async () => { finish({ url: "https://media.test/old.zip", name: "old.zip" } as never); await pending; });
  expect(start).not.toHaveBeenCalled();
  expect(await pending!).toMatchObject({ code: "aborted" });
});
