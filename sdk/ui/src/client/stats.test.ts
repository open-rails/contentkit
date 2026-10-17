import { expect, it } from "vitest";
import { createContentKitClient } from "./client.js";

it("reads daily people by audience and preserves the native active-time response", async () => {
  const requests: URL[] = [];
  const activity = { days: [{ date: "2026-10-16", readers: 3, anon: 1, free: 1, premium: 1, active_ms: 1234 }] };
  const client = createContentKitClient({
    baseUrl: "https://stats.test",
    fetch: async (input) => {
      const url = new URL(String(input));
      requests.push(url);
      return Response.json(activity);
    },
  });
  expect(await client.stats.activity({ kind: "gallery", days: 10 })).toEqual(activity);
  expect(requests[0]?.pathname.endsWith("/stats/activity")).toBe(true);
  expect(requests[0]?.searchParams.get("days")).toBe("10");
  expect("top" in client.stats).toBe(false);
});
