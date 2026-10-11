// @vitest-environment jsdom
import "../../src/test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeAll, describe, expect, it, vi } from "vitest";
import type { ContentKitClient } from "../../src/client/index.js";
import { ContentKitProvider, useEditorCrop, useMediaFolder, usePublicImage } from "../../src/react/index.js";
import type { Config } from "../support/harness.js";
import { Accounts, client, harness, item, png, recorder, wait } from "./setup.js";

describe("usePublicImage, useEditorCrop and useMediaFolder against the real ContentKit", () => {
  const h = harness();
  // The client sends a ref as given: only kind and id.
  const bare = ({ kind, id }: { kind: string; id: string }) => ({ kind, id });
  const accounts = new Accounts(h);
  let cfg: Config;

  beforeAll(async () => {
    cfg = await h.config();
    await accounts.load("alice", "bob", "carol");
  });

  const wrap = (c: ContentKitClient) =>
    function Wrapper({ children }: { children: ReactNode }) {
      return <ContentKitProvider client={c}>{children}</ContentKitProvider>;
    };

  it("usePublicImage shows no image for a preset without a default, then a read's exact renditions; a host listing skips the read", async () => {
    // An account's own avatar (the shared user kind, through adapters/authkit).
    const alice = accounts.get("alice");
    const ref = { kind: "user", id: alice.id };
    const r = recorder();
    const c = client(h, cfg, alice, { record: r });
    const { result } = renderHook(() => usePublicImage("user", alice.id, "avatar"), { wrapper: wrap(c) });
    await waitFor(() => expect(result.current.loading).toBe(false), wait);
    // The harness's avatar preset declares no default: no URL that would 404.
    expect(result.current).toMatchObject({ image: null, isDefault: false, rule: { kind: "user", name: "avatar", from: "avatar", namespace: "accounts", widths: [64, 128, 256], aspect: "1:1", default: false } });

    await act(async () => {
      await c.media.put(png("avatar.png", 41, 300, 300), { ref, path: "avatar" });
      await c.media.waitFor(ref, "avatar", wait);
    });
    await waitFor(() => expect(result.current.image?.renditions.length).toBeGreaterThan(0), wait);
    expect(result.current.isDefault).toBe(false);
    const own = result.current.image!.renditions;
    expect(own.length).toBeGreaterThan(0);
    for (const x of own) expect(new URL(x.url).pathname).toMatch(new RegExp(`^/v1/accounts/user/${alice.id}/public/avatar-\\d+-.+\\.webp$`));
    expect(result.current.image).toMatchObject({ preset: "avatar", aspect: "1:1" });

    const reads = r.calls.filter((x) => x === "/read").length;
    const listed = renderHook(() => usePublicImage("user", alice.id, "avatar", { image: { preset: "avatar", renditions: own } }), { wrapper: wrap(c) });
    expect(listed.result.current.image?.renditions).toEqual(own);
    expect(r.calls.filter((x) => x === "/read").length).toBe(reads);
  });

  it("usePublicImage still shows the item's image when the server answers no presets", async () => {
    const bob = accounts.get("bob");
    const ref = { kind: "user", id: bob.id };
    await client(h, cfg, bob).media.put(png("avatar.png", 42, 300, 300), { ref, path: "avatar" });
    await client(h, cfg, bob).media.waitFor(ref, "avatar", wait);
    // A fresh client fetches the presets once: that request is refused.
    await h.faults([{ fault: "api", path: "/api/contentkit/media/presets", method: "GET", status: 404, code: "not_found", times: 1 }]);
    const { result } = renderHook(() => usePublicImage("user", bob.id, "avatar"), { wrapper: wrap(client(h, cfg, bob)) });
    await waitFor(() => expect(result.current.image?.renditions.length).toBeGreaterThan(0), wait);
    expect(result.current.rule).toBeUndefined();
    expect(result.current.isDefault).toBe(false);
  });

  it("useEditorCrop loads an upload's editor view and its edit, and commits a new edit", async () => {
    const ref = bare(await item(h, "album", accounts.get("alice")));
    const r = recorder();
    const c = client(h, cfg, accounts.get("alice"), { record: r });
    await c.media.put(png("a.png", 43, 400, 300), { ref, path: "images/a.png", edit: { rotate: 90 } });
    const onSaved = vi.fn();
    let path: string | null = null;
    const { result, rerender } = renderHook(() => useEditorCrop(ref, path, { client: c, onSaved }));
    expect(result.current.status).toBe("idle");
    path = "images/a.png";
    rerender();
    // The first editor read asks the worker for the view.
    await waitFor(() => expect(result.current.status).toBe("cropping"), wait);
    expect(result.current).toMatchObject({ source: { url: expect.stringMatching(new RegExp(`^${cfg.media}/v1/${cfg.namespace}/album/${ref.id}/private/sha256-`)), width: 400, height: 300 }, edit: { rotate: 90 } });
    await act(async () => {
      await result.current.save({ crop: { x: 0, y: 0, w: 100, h: 100 } });
    });
    expect(r.commits.at(-1)).toEqual([{ op: "edit", path: "images/a.png", edit: { crop: { x: 0, y: 0, w: 100, h: 100 } } }]);
    expect(result.current.status).toBe("done");
    expect(onSaved).toHaveBeenCalledOnce();

    path = "images/missing.png";
    rerender();
    await waitFor(() => expect(result.current.status).toBe("error"), wait);
    expect(result.current.status === "error" && result.current.error.code).toBe("not_found");
  });

  it("useMediaFolder moves within a path, refuses a taken name and reports update failures", async () => {
    const carol = accounts.get("carol");
    const ref = bare(await item(h, "gallery", carol));
    const r = recorder();
    const c = client(h, cfg, carol, { record: r });
    const ups = await Promise.all([1, 2].map((n) => c.media.upload(png(`${n}.png`, 43 + n), { ref, path: `originals/${n}.png` })));
    await c.media.commit(ref, ups.map((u) => ({ op: "put" as const, path: u.path, blob: u.blob })));
    const onError = vi.fn();
    const { result } = renderHook(() => useMediaFolder(ref, { onError }), { wrapper: wrap(c) });
    await waitFor(() => expect(result.current.groups[0]?.files).toHaveLength(2), wait);
    await act(async () => result.current.move("originals/2.png", 0));
    expect(r.commits.at(-1)).toEqual([{ op: "move", path: "originals/2.png", index: 0 }]);
    await waitFor(() => expect(result.current.uploads.map((f) => f.path)).toEqual(["originals/2.png", "originals/1.png"]), wait);

    await act(async () => {
      await expect(result.current.rename("originals/1.png", "2")).rejects.toMatchObject({ code: "conflict" });
    });
    expect(result.current.error?.code).toBe("conflict");
    await act(async () => {
      await expect(result.current.remove(["originals/9.png"])).rejects.toMatchObject({ code: "not_found" });
    });
    expect(onError).toHaveBeenLastCalledWith(expect.objectContaining({ code: "not_found" }), { operation: "folder.update" });
    await act(async () => result.current.rename("originals/1.png", "first"));
    expect(result.current.error).toBeUndefined();
  });
});
