// @vitest-environment jsdom
import { act, renderHook, waitFor } from "@testing-library/react";
import { expect, it } from "vitest";
import { FakeServer, bytes } from "../test/fake.js";
import { UploadClient } from "./client.js";
import { useUpload, useUploadQueue } from "./react.js";

const ref = { kind: "gallery", id: "1", version: "en" };

function client(s: FakeServer) {
  return new UploadClient({ endpoint: "http://x/api", fetch: s.fetch, transport: s.transport, retryDelay: () => 0 });
}

it("useUploadQueue uploads, reorders and commits", async () => {
  const s = new FakeServer();
  const c = client(s);
  const { result } = renderHook(() => useUploadQueue(c, { ref }));
  act(() => {
    result.current.add([1, 2].map((n) => new File([bytes(100, n)], `${n}.png`, { type: "image/png" })));
  });
  await waitFor(() => expect(result.current.ready).toBe(true));
  act(() => result.current.move(result.current.items[1]!.id, 0));
  let files: { name: string }[] = [];
  await act(async () => {
    files = await result.current.commit();
  });
  expect(files.map((f) => f.name)).toEqual(["2.png", "1.png"]);
  expect(result.current.items.every((i) => i.status === "committed")).toBe(true);
});

it("useUpload reports progress and the result", async () => {
  const s = new FakeServer();
  const { result } = renderHook(() => useUpload(client(s)));
  await act(async () => {
    await result.current.upload(new File([bytes(100)], "c.png", { type: "image/png" }), { ref, slot: "cover" });
  });
  expect(result.current.status).toBe("done");
  expect(result.current.result?.name).toBe("cover");
  expect(result.current.progress?.loaded).toBe(100);
});
