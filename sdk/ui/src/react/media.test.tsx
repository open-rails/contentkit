// @vitest-environment jsdom
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { FakeServer, fakeClient } from "../../test/fake.js";
import type { ContentKitClient } from "../client/client.js";
import { createContentURLs, type ContentLink } from "../urls/index.js";
import { ContentKitProvider, useCanonicalContent, useEditorCrop, useMediaFolder, usePublicImage, type Navigate } from "./index.js";

const user = { kind: "user", id: "0192f000-0000-7000-8000-000000000041" };
const preset = { kind: "user", name: "avatar", from: "avatar", base: "https://m.example", namespace: "app", to: "avatar-{w}.webp", widths: [128, 256], aspect: "1:1" };
const wrap = (c: ContentKitClient, extra: { urls?: ReturnType<typeof createContentURLs>; navigate?: Navigate } = {}) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ContentKitProvider client={c} {...extra}>
        {children}
      </ContentKitProvider>
    );
  };

afterEach(() => {
  document.head.innerHTML = "";
  history.replaceState(null, "", "/");
});

it("usePublicImage shows a read's exact renditions, else the kind's default image; a host listing skips the read", async () => {
  const s = new FakeServer();
  s.presets = [preset];
  const c = fakeClient(s);
  const { result } = renderHook(() => usePublicImage("user", user.id, "avatar"), { wrapper: wrap(c) });
  await waitFor(() => expect(result.current.loading).toBe(false));
  expect(result.current).toMatchObject({ isDefault: true, rule: preset });
  expect(result.current.image?.renditions[0]).toEqual({ url: `https://m.example/v1/app/user/${user.id}/public/avatar-128.webp`, w: 128, h: 128 });
  expect(s.reads.at(-1)!.get("prefix")).toBe("avatar");

  const own = [{ url: "https://m.example/v1/app/user/x/public/avatar-128-gen.webp", w: 128, h: 128 }];
  s.items.set(`user/${user.id}`, [{ path: "avatar.png", type: "image/png", size: 3, upload: true }]);
  s.publicImages.set(`user/${user.id}`, [{ from: "avatar.png", preset: "avatar", renditions: own }]);
  await act(async () => c.media.commit(user, [{ op: "edit", path: "avatar.png" }]));
  await waitFor(() => expect(result.current.isDefault).toBe(false));
  expect(result.current.image).toEqual({ preset: "avatar", aspect: "1:1", renditions: own });

  const reads = s.reads.length;
  const listed = renderHook(() => usePublicImage("user", user.id, "avatar", { image: { preset: "avatar", renditions: own } }), { wrapper: wrap(c) });
  expect(listed.result.current.image?.renditions).toEqual(own);
  expect(s.reads.length).toBe(reads);
});

it("usePublicImage still shows the item's image when the server has no presets", async () => {
  const s = new FakeServer();
  const c = fakeClient(s);
  vi.spyOn(c.media, "presets").mockRejectedValue(new Error("404"));
  s.items.set(`user/${user.id}`, [{ path: "avatar.png", type: "image/png", size: 3, upload: true }]);
  s.publicImages.set(`user/${user.id}`, [{ from: "avatar.png", preset: "avatar", renditions: [{ url: "u", w: 64, h: 64 }] }]);
  const { result } = renderHook(() => usePublicImage("user", user.id, "avatar"), { wrapper: wrap(c) });
  await waitFor(() => expect(result.current.image?.renditions).toEqual([{ url: "u", w: 64, h: 64 }]));
  expect(result.current.rule).toBeUndefined();
});

it("useEditorCrop loads an upload's editor view and its edit, and commits a new edit", async () => {
  const s = new FakeServer();
  const ref = { kind: "post", id: "0192f000-0000-7000-8000-000000000042" };
  s.seed(ref, [{ path: "images/a.png", type: "image/png", size: 3, w: 4000, h: 3000, edit: { rotate: 90 } }]);
  const c = fakeClient(s);
  const onSaved = vi.fn();
  let path: string | null = null;
  const { result, rerender } = renderHook(() => useEditorCrop(ref, path, { client: c, onSaved }));
  expect(result.current.status).toBe("idle");
  path = "images/a.png";
  rerender();
  await waitFor(() => expect(result.current.status).toBe("cropping"));
  expect(result.current).toMatchObject({ source: { url: "fake://cdn/private/e-images/a.png", width: 4000, height: 3000 }, edit: { rotate: 90 } });
  await act(async () => {
    await result.current.save({ crop: { x: 0, y: 0, w: 100, h: 100 } });
  });
  expect(s.commits.at(-1)).toEqual([{ op: "edit", path: "images/a.png", edit: { crop: { x: 0, y: 0, w: 100, h: 100 } } }]);
  expect(result.current.status).toBe("done");
  expect(onSaved).toHaveBeenCalledOnce();

  path = "images/missing.png";
  rerender();
  await waitFor(() => expect(result.current.status).toBe("error"));
  expect(result.current.status === "error" && result.current.error.code).toBe("not_found");
});

it("useMediaFolder moves within a path, refuses a taken name and reports update failures", async () => {
  const s = new FakeServer();
  const ref = { kind: "gallery", id: "0192f000-0000-7000-8000-000000000043" };
  s.rules.set("gallery", [{ path: "originals/{name}", types: ["image/png"], max_bytes: 1 << 20 }]);
  s.seed(ref, [
    { path: "originals/1.png", type: "image/png", size: 3 },
    { path: "originals/2.png", type: "image/png", size: 3 },
  ]);
  const c = fakeClient(s);
  const onError = vi.fn();
  const { result } = renderHook(() => useMediaFolder(ref, { onError }), { wrapper: wrap(c) });
  await waitFor(() => expect(result.current.groups[0]?.files).toHaveLength(2));
  await act(async () => result.current.move("originals/2.png", 0));
  expect(s.commits.at(-1)).toEqual([{ op: "move", path: "originals/2.png", index: 0 }]);
  await waitFor(() => expect(result.current.uploads.map((f) => f.path)).toEqual(["originals/2.png", "originals/1.png"]));

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

it("useCanonicalContent replaces a stale address and keeps canonical, og and hreflang tags while mounted", async () => {
  const s = new FakeServer();
  const c = fakeClient(s);
  const urls = createContentURLs({ routes: { video: "watch" }, languages: ["en", "es"], origin: "https://example.com" });
  const navigate = vi.fn<Navigate>();
  const link: ContentLink = { content_kind: "video", code: "G4VRQ3ZQ5", slug: "night-run", slugs: { es: "carrera-nocturna" } };
  document.head.innerHTML = '<link rel="canonical" href="https://example.com/old">';
  let content: ContentLink | null = null;
  const { result, rerender, unmount } = renderHook(() => useCanonicalContent(content, { title: "Night run", location: "/es/watch/g4vrq3zq5/old-slug?t=12#c", defaultLanguage: "en" }), {
    wrapper: wrap(c, { urls, navigate }),
  });
  expect(navigate).not.toHaveBeenCalled();
  content = link;
  rerender();
  expect(navigate).toHaveBeenCalledWith("/es/watch/G4VRQ3ZQ5/carrera-nocturna?t=12#c", { replace: true });
  expect(result.current.url).toBe("https://example.com/es/watch/G4VRQ3ZQ5/carrera-nocturna");
  const head = () => [...document.head.children].map((e) => e.outerHTML);
  expect(head()).toEqual([
    '<link rel="canonical" href="https://example.com/es/watch/G4VRQ3ZQ5/carrera-nocturna">',
    '<meta property="og:url" content="https://example.com/es/watch/G4VRQ3ZQ5/carrera-nocturna">',
    '<meta property="og:title" content="Night run">',
    '<link rel="alternate" hreflang="en" href="https://example.com/en/watch/G4VRQ3ZQ5/night-run">',
    '<link rel="alternate" hreflang="es" href="https://example.com/es/watch/G4VRQ3ZQ5/carrera-nocturna">',
    '<link rel="alternate" hreflang="x-default" href="https://example.com/en/watch/G4VRQ3ZQ5/night-run">',
  ]);
  unmount();
  expect(head()).toEqual(['<link rel="canonical" href="https://example.com/old">']);
});

it("useCanonicalContent without navigate replaces history in place", () => {
  const c = fakeClient(new FakeServer());
  const urls = createContentURLs({ routes: { video: "watch" } });
  history.replaceState(null, "", "/watch/G4VRQ3ZQ5");
  renderHook(() => useCanonicalContent({ content_kind: "video", code: "G4VRQ3ZQ5", slug: "night-run" }), { wrapper: wrap(c, { urls }) });
  expect(location.pathname).toBe("/watch/G4VRQ3ZQ5/night-run");
  expect(document.head.querySelector("link[rel=canonical]")?.getAttribute("href")).toBe(`${location.origin}/watch/G4VRQ3ZQ5/night-run`);
});
