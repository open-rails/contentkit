import { expect, it } from "vitest";
import { createContentKitClient } from "./client.js";
import { Http } from "./http.js";
import { call } from "./route.js";

it("exposes the publishing mount through the shared client", () => {
  const client = createContentKitClient({ baseUrl: "/api/contentkit/" });
  expect(client.url("publishing")).toBe("/api/contentkit/v1");
  const split = createContentKitClient({ baseUrl: "/api/contentkit/", mounts: { publishing: "https://publishing.example/ck/" } });
  expect(split.url("publishing")).toBe("https://publishing.example/ck");
  expect(split.url("contents")).toBe("/api/contentkit/v1");
});

it("sends prepare, completion and status through the configured publishing mount", async () => {
  const requests: { url: string; method: string | undefined; token: string | null }[] = [];
  const http = new Http({
    baseUrl: "/api/contentkit/",
    mounts: { publishing: "https://publishing.example/ck/" },
    token: () => "publisher-key",
    fetch: (async (url: string, init: RequestInit) => {
      requests.push({ url, method: init.method, token: new Headers(init.headers).get("Authorization") });
      return Response.json({ id: "operation", state: "processing" });
    }) as typeof fetch,
  });
  const params = { id: "operation" };
  await call(http, "PUT", "/publishing-operations/{id}", { params, body: {} });
  await call(http, "POST", "/publishing-operations/{id}/complete", { params, body: {} });
  await call(http, "GET", "/publishing-operations/{id}", { params });
  expect(requests).toEqual([
    { url: "https://publishing.example/ck/publishing-operations/operation", method: "PUT", token: "Bearer publisher-key" },
    { url: "https://publishing.example/ck/publishing-operations/operation/complete", method: "POST", token: "Bearer publisher-key" },
    { url: "https://publishing.example/ck/publishing-operations/operation", method: "GET", token: "Bearer publisher-key" },
  ]);
});
