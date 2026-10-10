// @vitest-environment jsdom
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { expect, it } from "vitest";
import { FakeServer, bytes, fakeClient as client } from "../../test/fake.js";
import type { ContentKitClient } from "../client/client.js";
import type { ContentKitChange } from "../client/http.js";
import { ContentKitProvider, useCrop, useRead, useUpload, useUploadQueue } from "./index.js";

const provider = (c: ContentKitClient, onChange?: (change: ContentKitChange) => void) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ContentKitProvider client={c} onChange={onChange}>
        {children}
      </ContentKitProvider>
    );
  };

const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000001" };

it("useUploadQueue uploads, reorders and commits with the provider's client", async () => {
  const s = new FakeServer();
  const changes: ContentKitChange[] = [];
  const { result } = renderHook(() => useUploadQueue({ ref, path: "originals/{name}" }), { wrapper: provider(client(s), (c) => changes.push(c)) });
  act(() => {
    result.current.add([1, 2].map((n) => new File([bytes(100, n)], `${n}.png`, { type: "image/png" })));
  });
  await waitFor(() => expect(result.current.ready).toBe(true));
  act(() => result.current.move(result.current.items[1]!.id, 0));
  let files: { path: string }[] = [];
  await act(async () => {
    files = await result.current.commit();
  });
  expect(files.map((f) => f.path)).toEqual(["originals/2.png", "originals/1.png"]);
  expect(result.current.items.every((i) => i.status === "committed")).toBe(true);
  expect(changes).toMatchObject([{ type: "media.committed", ref, files: [{ path: "originals/2.png" }, { path: "originals/1.png" }] }]);
});

it("hooks without a client or provider say how to get one", () => {
  expect(() => renderHook(() => useUpload())).toThrow(/ContentKitProvider/);
});

it("useUpload reports progress and the result; with put it commits and waits", async () => {
  const s = new FakeServer();
  const { result } = renderHook(() => useUpload({ client: client(s) }));
  await act(async () => {
    await result.current.upload(new File([bytes(100)], "c.png", { type: "image/png" }), { ref, path: "originals/c.png" });
  });
  expect(result.current.status).toBe("done");
  expect(result.current.result).toMatchObject({ path: "originals/c.png", blob: expect.stringMatching(/^u-/), exists: false });
  expect(result.current.progress?.loaded).toBe(100);
  expect(s.commits).toEqual([]);

  await act(async () => {
    await result.current.upload(new File([bytes(100, 2)], "c.png", { type: "image/png" }), { ref, path: "cover", put: { edit: { rotate: 180 } } });
  });
  expect(result.current.result).toMatchObject({ path: "cover.png", file: { path: "cover.png", edit: { rotate: 180 } } });
  expect(result.current.progress?.phase).toBe("processing");
});

it("useRead reads the item and refetches on reload", async () => {
  const s = new FakeServer();
  s.seed(ref, [{ path: "originals/1.png", type: "image/png", size: 3 }]);
  const c = client(s);
  const { result } = renderHook(() => useRead(ref, { editor: true, client: c }));
  await waitFor(() => expect(result.current.read?.files.map((f) => f.path)).toEqual(["originals/1.png"]));
  act(() => result.current.reload());
  await waitFor(() => expect(s.calls.filter((x) => x === "/read")).toHaveLength(2));
  const given = renderHook(() => useRead(ref, { read: null, client: c }));
  expect([given.result.current.read, given.result.current.loading]).toEqual([null, false]);
  expect(s.calls.filter((x) => x === "/read")).toHaveLength(2);
});

it("useRead shares one request per item and options, and set() and processed uploads update every reader", async () => {
  const s = new FakeServer();
  s.seed(ref, [{ path: "cover.png", type: "image/png", size: 3 }]);
  const c = client(s);
  const { result } = renderHook(
    () => ({ a: useRead(ref, { editor: true, prefix: "cover" }), b: useRead(ref, { editor: true, prefix: "cover" }), other: useRead(ref, { editor: true }) }),
    { wrapper: provider(c) },
  );
  await waitFor(() => expect(result.current.a.loading || result.current.b.loading || result.current.other.loading).toBe(false));
  expect(s.calls.filter((x) => x === "/read")).toHaveLength(2);
  expect(result.current.b.read).toBe(result.current.a.read);

  const read = result.current.a.read!;
  act(() => result.current.a.set({ ...read, files: [{ ...read.files[0]!, meta: { by: "set" } }] }));
  expect(result.current.b.read?.files[0]?.meta).toEqual({ by: "set" });
  expect(result.current.other.read?.files[0]?.meta).toBeUndefined();

  await act(async () => {
    await c.media.put(new File([bytes(100, 9)], "c.png", { type: "image/png" }), { ref, path: "cover", edit: { rotate: 90 } });
  });
  expect(result.current.a.read?.files.find((f) => f.path === "cover.png")?.edit).toEqual({ rotate: 90 });
  expect(result.current.other.read?.files.find((f) => f.path === "cover.png")?.edit).toEqual({ rotate: 90 });
  expect(s.calls.filter((x) => x === "/read")).toHaveLength(3); // put's own wait, no refetch
});

it("useCrop keeps a crop in original pixels at the aspect and yields the edit", () => {
  const source = { width: 400, height: 200 };
  const { result } = renderHook(() => useCrop({ source, aspect: "1:2" }));
  expect(result.current.crop).toEqual({ x: 150, y: 0, w: 100, h: 200 });
  act(() => result.current.setFromDisplay({ x: 100, y: 0, w: 60, h: 999 }, { width: 200, height: 100 }));
  expect(result.current.edit).toEqual({ crop: { x: 200, y: 0, w: 100, h: 200 } });
  act(() => result.current.rotateBy(90));
  expect(result.current.rotate).toBe(90);
  expect(result.current.edit).toEqual({ crop: { x: 200, y: 0, w: 100, h: 50 }, rotate: 90 });
  act(() => result.current.rotateBy(-90));
  act(() => result.current.setCrop({ x: 0, y: 0, w: 1000, h: 0 }));
  expect(result.current.crop).toEqual({ x: 0, y: 0, w: 100, h: 200 });

  const free = renderHook(() => useCrop({ source, initial: { crop: { x: 10, y: 10, w: 50, h: 40 }, rotate: 180 } }));
  expect(free.result.current.edit).toEqual({ crop: { x: 10, y: 10, w: 50, h: 40 }, rotate: 180 });
  act(() => free.result.current.reset());
  expect(free.result.current.edit).toBeNull();
});
