import { describe, expect, it } from "vitest";
import type { ReadResult } from "../generated/wire.js";
import { acceptOf, defaultImage, filesFor, ratioLabel, ruleFor, screenFiles, uniqueName, withMediaType, type UploadRule } from "./rules.js";
import { chunkOffsets, mergeReads, processing } from "./windows.js";

const MiB = 1 << 20;
const images: UploadRule = { path: "images/{name}", types: ["image/png", "image/jpeg"], max_bytes: 25 * MiB, max: 2 };
const videos: UploadRule = { path: "videos/{name}", types: ["video/mp4", "video/x-matroska"], max_bytes: 1024 * MiB, min_aspect: 1 / 2.4, max_aspect: 2.4 };
const cover: UploadRule = { path: "cover", types: ["image/png"], max_bytes: MiB };
const file = (name: string, type: string, size = 10) => new File([new Uint8Array(size)], name, { type });

describe("upload rules", () => {
  it("finds a path's rule: a literal wins, a pattern takes one segment", () => {
    const rules = [images, videos, cover, { path: "{name}", types: [], max_bytes: 0 }];
    expect(ruleFor(rules, "images/a.png")).toBe(images);
    expect(ruleFor(rules, "images/a")).toBe(images);
    expect(ruleFor(rules, "cover.png")?.path).toBe("cover");
    expect(ruleFor(rules, "cover")?.path).toBe("cover");
    expect(ruleFor(rules, "notes.txt")?.path).toBe("{name}");
    expect(ruleFor(rules, "images/deep/a.png")).toBeUndefined();
    expect(ruleFor([images], "images/")).toBeUndefined();
  });

  it("screens files: type, size and each rule's cap, counting files already there", () => {
    const s = screenFiles(
      [file("a.png", "image/png"), file("b.gif", "image/gif"), file("big.jpg", "image/jpeg", 26 * MiB), file("c.jpg", "image/jpeg"), file("d.png", "image/png"), file("m.mkv", "")],
      [images, videos],
      (r) => (r === images ? 0 : 3),
    );
    expect(s.accepted.map((a) => [a.file.name, a.rule.path, a.file.type])).toEqual([
      ["a.png", "images/{name}", "image/png"],
      ["c.jpg", "images/{name}", "image/jpeg"],
      ["m.mkv", "videos/{name}", "video/x-matroska"],
    ]);
    expect(s.refused.map((r) => [r.file.name, r.error.code, r.error.details])).toEqual([
      ["b.gif", "type_not_allowed", { type: "image/gif", allowed: ["image/png", "image/jpeg", "video/mp4", "video/x-matroska"] }],
      ["big.jpg", "too_large", { type: "image/jpeg", size: 26 * MiB, max_bytes: 25 * MiB }],
      ["d.png", "too_many_files", { max: 2 }],
    ]);
  });

  it("lets a rule without types or limits take anything", () => {
    const s = screenFiles([file("x.bin", "application/octet-stream", 99 * MiB)], [{ path: "originals/{name}", types: [], max_bytes: 0 }]);
    expect(s.accepted).toHaveLength(1);
    expect(s.refused).toEqual([]);
  });

  it("types files by extension when the browser does not, and builds accept", () => {
    expect(withMediaType(file("clip.MOV", "")).type).toBe("video/quicktime");
    expect(withMediaType(file("subs.srt", "application/octet-stream"), ["application/x-subrip"]).type).toBe("application/x-subrip");
    const typed = file("a.png", "image/png");
    expect(withMediaType(typed)).toBe(typed);
    expect(acceptOf([videos])).toBe("video/mp4,video/x-matroska,.mp4,.m4v,.mkv");
    expect(acceptOf([])).toBeUndefined();
  });

  it("numbers a name around names taken, by stem", () => {
    expect(uniqueName("a.png", ["b.png"])).toBe("a.png");
    expect(uniqueName("a.png", ["a.jpg", "a-2.webp"])).toBe("a-3.png");
    expect(uniqueName("notes", ["notes"])).toBe("notes-2");
  });

  it("labels ratios and lists a rule's uploads", () => {
    expect([ratioLabel(2.4), ratioLabel(1 / 2.4), ratioLabel(16 / 9), ratioLabel(1)]).toEqual(["2.4:1", "1:2.4", "1.78:1", "1:1"]);
    const read = { files: [{ path: "images/a.png", type: "image/png", upload: true }, { path: "videos/v.mp4", type: "video/mp4", upload: true }, { path: "low-res/a.webp", type: "image/webp", from: "images/a.png" }, { path: "images/u.png", type: "image/png", upload: true, unattached: true }] } as ReadResult;
    expect(filesFor(read, images, [images, videos]).map((f) => f.path)).toEqual(["images/a.png"]);
  });

  it("builds a preset's default image at every width, never for a preview", () => {
    const preset = { kind: "user", name: "avatar", from: "avatar", base: "https://m.example/", namespace: "app", to: "avatar-{w}.webp", widths: [128, 256], aspect: "1:1" };
    expect(defaultImage(preset, "0192f000-0000-7000-8000-000000000001")).toEqual({
      preset: "avatar",
      aspect: "1:1",
      renditions: [
        { url: "https://m.example/v1/app/user/0192f000-0000-7000-8000-000000000001/public/avatar-128.webp", w: 128, h: 128 },
        { url: "https://m.example/v1/app/user/0192f000-0000-7000-8000-000000000001/public/avatar-256.webp", w: 256, h: 256 },
      ],
    });
    expect(defaultImage({ ...preset, first: 1, to: "preview-{n}.webp" }, "x").renditions).toEqual([]);
  });
});

describe("read windows", () => {
  it("covers a range with chunk offsets, clipped to the total", () => {
    expect(chunkOffsets(0, 10)).toEqual([0]);
    expect(chunkOffsets(95, 130, 120)).toEqual([50, 100]);
    expect(chunkOffsets(0, Number.MAX_SAFE_INTEGER, 401, 200)).toEqual([0, 200, 400]);
    expect(chunkOffsets(5, 5)).toEqual([]);
  });

  it("merges windows: the newest listing, every window's URLs, the earliest expiry", () => {
    const base = { access: "full" as const, total: 3, limit: 1 };
    const a: ReadResult = { ...base, offset: 0, expires: 100, files: [{ path: "p/1", type: "image/webp", url: "u1" }, { path: "p/2", type: "image/webp" }, { path: "p/3", type: "image/webp" }] };
    const b: ReadResult = { ...base, offset: 2, expires: 200, meta: { v: 2 }, files: [{ path: "p/1", type: "image/webp" }, { path: "p/3", type: "image/webp", url: "u3" }] };
    expect(mergeReads([a, b, null])).toEqual({ ...b, offset: 0, limit: 3, expires: 100, files: [{ path: "p/1", type: "image/webp", url: "u1" }, { path: "p/3", type: "image/webp", url: "u3" }] });
    expect(mergeReads([a])).toBe(a);
    expect(mergeReads([])).toBeNull();
  });

  it("knows an editor read still processing, unless the item is full", () => {
    const read = (files: ReadResult["files"], extra: Partial<ReadResult> = {}): ReadResult => ({ access: "full", expires: 0, total: 0, offset: 0, limit: 0, files, ...extra });
    expect(processing(read([{ path: "a", type: "image/png", upload: true, pending: ["low"] }]))).toBe(true);
    expect(processing(read([{ path: "a", type: "image/png", upload: true, staged: true }]))).toBe(true);
    expect(processing(read([{ path: "a", type: "image/png", upload: true, pending: ["low"], failed: { of: "low", message: "x" } }]))).toBe(false);
    expect(processing(read([{ path: "a", type: "image/png", upload: true, pending: ["low"] }], { full: true }))).toBe(false);
    expect(processing(read([], { state: "processing" }))).toBe(true);
  });
});
