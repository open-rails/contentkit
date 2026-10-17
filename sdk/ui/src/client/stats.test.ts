import { expect, expectTypeOf, it } from "vitest";
import { createContentKitClient } from "./client.js";
import type { TopQuery } from "./stats.js";

it("uses the measured readership metric and preserves the native active-time response", async () => {
  expectTypeOf<TopQuery["metric"]>().toEqualTypeOf<"readers" | "comments" | undefined>();
  const requests: URL[] = [];
  const activity = { days: [{ date: "2026-10-16", readers: 2, users: 1, active_ms: 1234 }] };
  const client = createContentKitClient({
    baseUrl: "https://stats.test",
    fetch: async (input) => {
      const url = new URL(String(input));
      requests.push(url);
      return Response.json(url.pathname.endsWith("/activity") ? activity : { contents: [] });
    },
  });
  expect(await client.stats.activity({ kind: "gallery", days: 7 })).toEqual(activity);
  await client.stats.top({ kind: "gallery", metric: "readers", period: "90d" });
  await client.stats.top({ kind: "gallery", metric: "comments" });
  expect(requests[1]?.searchParams.get("metric")).toBe("readers");
  expect(requests[1]?.searchParams.get("period")).toBe("90d");
  expect(requests[2]?.searchParams.get("metric")).toBe("comments");
});
