// @vitest-environment jsdom
import "../../src/test/dom.js";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeAll, describe, expect, it } from "vitest";
import type { ContentKitChange, ContentKitClient } from "../../src/client/index.js";
import { ContentKitProvider, useUpload, useUploadQueue } from "../../src/react/index.js";
import type { Config, TestUser } from "../support/harness.js";
import { Accounts, client, harness, item, png, recorder, wait } from "./setup.js";

describe("upload hooks against the real ContentKit", () => {
  const h = harness();
  const accounts = new Accounts(h);
  let cfg: Config;
  let alice: TestUser;

  beforeAll(async () => {
    cfg = await h.config();
    await accounts.load("alice");
    alice = accounts.get("alice");
  });

  const provider = (c: ContentKitClient, onChange?: (change: ContentKitChange) => void) =>
    function Wrapper({ children }: { children: ReactNode }) {
      return (
        <ContentKitProvider client={c} onChange={onChange}>
          {children}
        </ContentKitProvider>
      );
    };
  const gallery = async () => {
    const { kind, id } = await item(h, "gallery", alice);
    return { kind, id };
  };

  it("useUploadQueue uploads, reorders and commits with the provider's client", async () => {
    const ref = await gallery();
    const changes: ContentKitChange[] = [];
    const { result } = renderHook(() => useUploadQueue({ ref, path: "originals/{name}" }), { wrapper: provider(client(h, cfg, alice), (c) => changes.push(c)) });
    act(() => {
      result.current.add([1, 2].map((n) => png(`${n}.png`, 3000 + n)));
    });
    await waitFor(() => expect(result.current.ready).toBe(true), wait);
    act(() => result.current.move(result.current.items[1]!.id, 0));
    let files: { path: string }[] = [];
    await act(async () => {
      files = await result.current.commit();
    });
    expect(files.map((f) => f.path)).toEqual(["originals/2.png", "originals/1.png"]);
    expect(result.current.items.every((i) => i.status === "committed")).toBe(true);
    expect(changes).toMatchObject([{ type: "media.committed", ref, files: [{ path: "originals/2.png" }, { path: "originals/1.png" }] }]);
  });

  it("useUpload reports progress and the result; with put it commits and waits", async () => {
    const ref = await gallery();
    const r = recorder();
    const { result } = renderHook(() => useUpload({ client: client(h, cfg, alice, { record: r }) }));
    const page = png("c.png", 3011);
    await act(async () => {
      await result.current.upload(page, { ref, path: "originals/c.png" });
    });
    expect(result.current.status).toBe("done");
    expect(result.current.result).toMatchObject({ path: "originals/c.png", blob: expect.stringMatching(/^u-/), exists: false });
    expect(result.current.progress?.loaded).toBe(page.size);
    expect(r.commits).toEqual([]);

    await act(async () => {
      await result.current.upload(png("c.png", 3012, 60, 40), { ref, path: "cover", put: { edit: { rotate: 180 } } });
    });
    expect(result.current.result).toMatchObject({ path: "cover.png", file: { path: "cover.png", edit: { rotate: 180 } } });
    expect(result.current.progress?.phase).toBe("processing");
  });
});
