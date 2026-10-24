import { expect, it } from "vitest";
import { createContentKitClient } from "./client.js";
import type { CommitBody } from "./generated/wire.js";

it("uploads to an existing version and returns pending media without publication or status calls", async () => {
  const requests: string[] = [];
  let committed: CommitBody | undefined;
  const client = createContentKitClient({
    baseUrl: "https://catalog.test/api",
    token: () => "api-only-key",
    fetch: async (input, init) => {
      const path = new URL(String(input)).pathname;
      requests.push(`${init?.method} ${path}`);
      expect(new Headers(init?.headers).get("Authorization")).toBe("Bearer api-only-key");
      if (path.endsWith("/presign")) return Response.json({ path: "import/pages.zip", blob: "staged", put: { method: "PUT", url: "https://storage.test/upload", headers: {} } });
      if (path.endsWith("/commit")) {
        committed = JSON.parse(String(init?.body));
        return Response.json({ files: [
          { path: "import/pages.zip", type: "application/zip", upload: true, pending: ["import"] },
        ] });
      }
      throw new Error(`Unexpected publication or polling request: ${path}`);
    },
    media: { transport: async (request, body) => {
      requests.push(`${request.method} ${request.url}`);
      expect(new Headers(request.headers).has("Authorization")).toBe(false);
      expect(await body.text()).toBe("archive bytes");
    } },
  });
  const ref = { kind: "gallery", id: "01900000-0000-7000-8000-000000000001", version: "01900000-0000-7000-8000-000000000002" };
  const file = new File(["archive bytes"], "pages.zip", { type: "application/zip" });
  const uploaded = await client.media.upload(file, { ref, path: "import/pages.zip" });
  const result = await client.media.commit(ref, [{ op: "put", path: uploaded.path, source_name: uploaded.sourceName, blob: uploaded.blob }]);
  expect(result.map((file) => file.path)).toEqual(["import/pages.zip"]);
  expect(result[0]?.pending).toEqual(["import"]);
  expect(committed?.ref).toEqual(ref);
  expect(committed?.ops).toEqual([{ op: "put", path: "import/pages.zip", source_name: "pages.zip", blob: "staged" }]);
  expect(requests).toEqual([
    "POST /api/v1/media/upload/presign",
    "PUT https://storage.test/upload",
    "POST /api/v1/media/upload/commit",
  ]);
});
