// @vitest-environment jsdom
import "../test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../client/client.js";
import { ContentKitError } from "../client/errors.js";
import { usePresets } from "./public.js";

const rules = [{ kind: "gallery", name: "cover", from: "*.jpg", to: "cover.webp", widths: [570], base: "https://media.test", namespace: "test", default: false, aspect: "7071:10000" }];

it("retries a failed public preset read after remount instead of caching the outage", async () => {
  const client = createContentKitClient({ baseUrl: "https://api.test" });
  const error = new ContentKitError("network", "offline");
  const request = vi.spyOn(client.media, "presets").mockRejectedValueOnce(error).mockResolvedValue(rules);
  const first = renderHook(() => usePresets({ client }));
  await waitFor(() => expect(first.result.current.error).toBe(error));
  first.unmount();
  const second = renderHook(() => usePresets({ client }));
  await waitFor(() => expect(second.result.current.presets).toEqual(rules));
  expect(request).toHaveBeenCalledTimes(2);
  expect(second.result.current.error).toBeUndefined();
});

it("offers an explicit retry while still mounted", async () => {
  const client = createContentKitClient({ baseUrl: "https://api.test" });
  vi.spyOn(client.media, "presets").mockRejectedValueOnce(new ContentKitError("network", "offline")).mockResolvedValue(rules);
  const view = renderHook(() => usePresets({ client }));
  await waitFor(() => expect(view.result.current.error?.code).toBe("network"));
  act(() => view.result.current.reload());
  await waitFor(() => expect(view.result.current.presets).toEqual(rules));
  expect(view.result.current.error).toBeUndefined();
});

it("continues to share successful public presets without another network read", async () => {
  const fetch = vi.fn(async () => Response.json(rules));
  const client = createContentKitClient({ baseUrl: "https://api.test", fetch });
  const first = renderHook(() => usePresets({ client }));
  await waitFor(() => expect(first.result.current.presets).toEqual(rules));
  first.unmount();
  const second = renderHook(() => usePresets({ client }));
  expect(second.result.current.presets).toEqual(rules);
  expect(fetch).toHaveBeenCalledOnce();
});

it("shares a retry's recovery with every mounted consumer", async () => {
  let recovered = false;
  const client = createContentKitClient({ baseUrl: "https://api.test", media: { retries: 0 }, fetch: async () => recovered ? Response.json(rules) : Response.json({ code: "unavailable" }, { status: 503 }) });
  const first = renderHook(() => usePresets({ client }));
  const second = renderHook(() => usePresets({ client }));
  await waitFor(() => expect(first.result.current.error).toBeDefined());
  await waitFor(() => expect(second.result.current.error).toBeDefined());
  recovered = true;
  act(() => first.result.current.reload());
  await waitFor(() => expect(first.result.current.presets).toEqual(rules));
  await waitFor(() => expect(second.result.current.presets).toEqual(rules));
  expect(second.result.current.error).toBeUndefined();
});


it("does not request presets when no provider or client is present", () => {
  const view = renderHook(() => usePresets());
  expect(view.result.current.presets).toEqual([]);
  expect(view.result.current.loaded).toBe(true);
  expect(view.result.current.error).toBeUndefined();
  act(() => view.result.current.reload());
});
