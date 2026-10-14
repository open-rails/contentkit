import { expect, it } from "vitest";
import { Http } from "../http.js";
import { MediaApi } from "./api.js";
import { isContentId } from "./ref.js";

it("refuses a ref whose content id is not a UUIDv7 before any request", async () => {
  expect(isContentId("0192f000-0000-7000-8000-000000000001")).toBe(true);
  for (const bad of ["1", "0192f000-0000-4000-8000-000000000001", "0192F000-0000-7000-8000-000000000001", "0192f000-0000-7000-c000-000000000001"]) expect(isContentId(bad)).toBe(false);
  let called = false;
  const api = new MediaApi(new Http({ baseUrl: "/u", fetch: (async () => ((called = true), new Response("{}"))) as typeof fetch }));
  await expect(api.presign({ ref: { kind: "post", id: "18" }, path: "cover", type: "image/png", size: 1, sha256: "0".repeat(64) })).rejects.toMatchObject({ code: "invalid_request" });
  expect(called).toBe(false);
});
