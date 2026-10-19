import { afterEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "./client.js";

afterEach(() => vi.useRealTimers());

it("publishes once its files finish, and refuses other errors at once", async () => {
  vi.useFakeTimers();
  const answers = [
    Response.json({ code: "not_ready", error: "media not ready: pages are still being published" }, { status: 409 }),
    Response.json({ id: "c1", kind: "gallery" }),
  ];
  const fetcher = vi.fn(async () => answers.shift()!);
  const client = createContentKitClient({ baseUrl: "https://api.test/ck", fetch: fetcher as unknown as typeof fetch });
  const published = client.contents.publishWhenReady("c1");
  await vi.advanceTimersByTimeAsync(1000);
  await expect(published).resolves.toMatchObject({ id: "c1" });
  expect(fetcher).toHaveBeenCalledTimes(2);

  const refused = vi.fn(async () => Response.json({ code: "forbidden", error: "forbidden" }, { status: 403 }));
  const other = createContentKitClient({ baseUrl: "https://api.test/ck", fetch: refused as unknown as typeof fetch });
  await expect(other.contents.publishWhenReady("c1")).rejects.toMatchObject({ code: "forbidden" });
  expect(refused).toHaveBeenCalledTimes(1);
});

it("gives up with the server's not_ready once the wait runs out", async () => {
  vi.useFakeTimers();
  const fetcher = vi.fn(async () => Response.json({ code: "not_ready", error: "media not ready: pages are still processing" }, { status: 409 }));
  const client = createContentKitClient({ baseUrl: "https://api.test/ck", fetch: fetcher as unknown as typeof fetch });
  const published = client.contents.publishWhenReady("c1", undefined, 2500).catch((e: unknown) => e);
  await vi.advanceTimersByTimeAsync(10_000);
  expect(await published).toMatchObject({ code: "not_ready" });
});
