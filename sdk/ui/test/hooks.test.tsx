// @vitest-environment jsdom
import "../src/test/dom.js";
import { File as NodeFile } from "node:buffer";
import type { ChildProcess } from "node:child_process";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { createContentKitClient, fetchTransport, type ContentKitClient, type Op } from "../src/client/index.js";
import { ContentKitProvider, useMediaFolder, useMediaRead } from "../src/react/index.js";
import { bytes } from "./fake.js";
import { KillProxy, startServer, stopServer } from "./server.js";

const endpoint = process.env.CONTENTKIT_TEST_S3_ENDPOINT;
const id = (n: number) => `0192f000-0000-7000-8000-${String(900 + n).padStart(12, "0")}`;
// Node's File: jsdom's Blob is not a body Node's fetch can send.
const png = (name: string, seed: number) => new NodeFile([bytes(3000, seed)], name, { type: "image/png" }) as unknown as File;
const wait = { timeout: 60_000, interval: 100 };

describe.skipIf(!endpoint)("hooks against MinIO and the media handlers", () => {
  let proxy: KillProxy;
  let proc: ChildProcess;
  let base: string;

  beforeAll(async () => {
    proxy = new KillProxy(new URL(endpoint!));
    const s = await startServer(await proxy.listen());
    proc = s.proc;
    base = s.url;
  });

  afterAll(async () => {
    if (proc) await stopServer(proc);
    await proxy?.close();
  });

  const client = (actor = "alice"): ContentKitClient =>
    createContentKitClient({
      baseUrl: base,
      mounts: { upload: `${base}/upload`, media: `${base}/read` },
      headers: () => ({ "X-Test-Actor": actor }),
      media: { transport: fetchTransport, retryDelay: () => 200 },
    });
  const provider = (c: ContentKitClient) =>
    function Wrapper({ children }: { children: ReactNode }) {
      return <ContentKitProvider client={c}>{children}</ContentKitProvider>;
    };

  it("useMediaRead polls an editor read until the worker is done, and merges a viewer's windows", async () => {
    const ref = { kind: "gallery", id: id(1) };
    const c = client();
    const ops: Op[] = [];
    for (const n of [1, 2, 3]) {
      const up = await c.media.upload(png(`${n}.png`, n), { ref, path: `originals/00${n}.png` });
      ops.push({ op: "put", path: up.path, blob: up.blob });
    }
    const { result } = renderHook(() => useMediaRead(ref, { editor: true, prefix: "originals/", poll: 100 }), { wrapper: provider(c) });
    await act(async () => void (await c.media.commit(ref, ops)));
    await waitFor(() => expect(result.current.read?.files.map((f) => f.path)).toEqual(["originals/001.png", "originals/002.png", "originals/003.png"]), wait);
    await waitFor(() => expect(result.current.processing).toBe(false), wait);
    expect(result.current.read!.files.every((f) => f.upload && !f.staged && !f.pending)).toBe(true);

    const viewer = renderHook(() => useMediaRead(ref, { prefix: "low-res/", window: { start: 0, end: 3, chunk: 2 } }), { wrapper: provider(client("reader")) });
    await waitFor(() => expect(viewer.result.current.read?.files.filter((f) => f.url)).toHaveLength(3), wait);
    expect(viewer.result.current.read).toMatchObject({ access: "full", total: 3, offset: 0, limit: 4 });
  });

  it("useMediaFolder uploads files, adds them in queue order, moves, renames and removes them", async () => {
    const ref = { kind: "gallery", id: id(2) };
    const c = client();
    const { result } = renderHook(() => useMediaFolder(ref, { paths: ["originals/{name}"] }), { wrapper: provider(c) });
    await waitFor(() => expect(result.current.read.read).not.toBeNull(), wait);
    act(() => void result.current.add([png("b.png", 11), png("a.png", 12), png("b.png", 13)]));
    await waitFor(() => expect(result.current.queue.ready).toBe(true), wait);
    expect(result.current.queue.items.map((i) => i.path)).toEqual(["originals/b.png", "originals/a.png", "originals/b-2.png"]);
    await act(async () => void (await result.current.commit()));
    expect(result.current.queue.items).toEqual([]);
    const paths = () => result.current.uploads.map((f) => f.path);
    await waitFor(() => expect(paths()).toEqual(["originals/b.png", "originals/a.png", "originals/b-2.png"]), wait);

    await act(async () => result.current.move("originals/a.png", 0));
    await waitFor(() => expect(paths()).toEqual(["originals/a.png", "originals/b.png", "originals/b-2.png"]), wait);
    await act(async () => result.current.rename("originals/b-2.png", "c"));
    await waitFor(() => expect(paths()).toEqual(["originals/a.png", "originals/b.png", "originals/c.png"]), wait);
    await act(async () => result.current.edit("originals/a.png", { rotate: 90 }));
    await waitFor(() => expect(result.current.uploads[0]?.edit).toEqual({ rotate: 90 }), wait);
    await act(async () => result.current.remove(["originals/b.png", "originals/c.png"]));
    await waitFor(() => expect(paths()).toEqual(["originals/a.png"]), wait);
    // The server stays the authority: a rename onto a taken name is refused.
    await act(async () => void (await c.media.commit(ref, [{ op: "put", path: "originals/z.png", blob: (await c.media.upload(png("z.png", 14), { ref, path: "originals/z.png" })).blob }])));
    await waitFor(() => expect(paths()).toContain("originals/z.png"), wait);
    await act(async () => {
      await expect(result.current.rename("originals/z.png", "a")).rejects.toMatchObject({ code: "conflict" });
    });
  });

  it("useMediaFolder shows the server's refusal when a commit passes the kind's file cap", async () => {
    const ref = { kind: "post", id: id(3) };
    const { result } = renderHook(() => useMediaFolder(ref, { paths: ["originals/{name}"] }), { wrapper: provider(client()) });
    await waitFor(() => expect(result.current.read.read).not.toBeNull(), wait);
    act(() => void result.current.add([png("1.png", 21), png("2.png", 22), png("3.png", 23)]));
    await waitFor(() => expect(result.current.queue.ready || result.current.refused.length > 0).toBe(true), wait);
    if (result.current.refused.length) {
      // The editor read states the cap: the third file never uploads.
      expect(result.current.refused.map((r) => [r.file.name, r.error.code])).toEqual([["3.png", "too_many_files"]]);
      return;
    }
    await act(async () => {
      await expect(result.current.commit()).rejects.toMatchObject({ code: "too_many_files", status: 409 });
    });
    expect(result.current.error?.code).toBe("too_many_files");
  });
});
