import { expect, it } from "vitest";
import type { ReaderPlan } from "./generated/wire.js";
import { matchAudio, readerMetaProblems, readerNames } from "./reader-meta.js";

const page = (name: string) => ({ name, w: 800, h: 1200, files: { low: `low/${name}.webp` } });
const plan: ReaderPlan = {
  content_id: "c1",
  version_id: "v1",
  publication_id: "p1",
  pages: 3,
  segments: [
    { position: 1, start: 0, end: 2, released: true, locked: false, pages: [page("001"), page("002")] },
    { position: 2, start: 2, end: 3, released: true, locked: false, pages: [page("003")] },
  ],
  previews: [],
  playback: { item: { kind: "gallery.published", id: "x" }, tracks: [{ name: "001", path: "listen/001/" }, { name: "Intro", path: "listen/Intro/" }, { name: "theme", path: "listen/theme/" }] },
};

it("readerNames: every segment's pages in reading order and the playback tracks", () => {
  expect(readerNames(plan)).toEqual({ pages: ["001", "002", "003"], files: ["001", "Intro", "theme"] });
  expect(readerNames({ ...plan, playback: undefined, segments: [{ start: 0, end: 3, released: false, locked: true }] })).toEqual({ pages: [], files: [] });
});

it("matchAudio: an anchor's name wins over a page's, case and extension ignored; the rest is reported", () => {
  const got = matchAudio({ pages: ["001", "002", "003"], files: ["001.mp3", "INTRO", "theme"] }, ["intro"]);
  expect(got.foreground).toEqual([
    { file: "INTRO", anchor: "intro" },
    { file: "001.mp3", page: "001" },
  ]);
  expect(got.unmatchedPages).toEqual(["002", "003"]);
  expect(got.unmatchedFiles).toEqual(["theme"]);
});

it("readerMetaProblems: what the server refuses, and a cue with no place", () => {
  const names = { pages: ["001", "002"], files: ["001", "theme"] };
  expect(
    readerMetaProblems(
      {
        schema_version: 1,
        defaults: { auto_advance_seconds: 4 },
        anchors: [{ id: "a1", page: "002", offset_percent: 50 }],
        background: [{ file: "theme", from_anchor: "a1", to_page: "002", loop: true }],
        foreground: [{ file: "001", page: "001" }],
      },
      names,
    ),
  ).toEqual([]);
  expect(
    readerMetaProblems(
      {
        schema_version: 1,
        defaults: { auto_advance_seconds: 0 },
        anchors: [
          { id: "a1", page: "009", offset_percent: 101 },
          { id: "a1", page: "001", offset_percent: 0, duration_seconds: -1 },
          { id: " ", page: "001", offset_percent: Number.NaN },
        ],
        background: [{ file: "nope", from_anchor: "zz" }],
        foreground: [{ file: "001" }, { file: "001", page: "001", anchor: "a1" }],
      },
      names,
    ),
  ).toEqual([
    { at: "defaults", index: 0, problem: "seconds" },
    { at: "anchors", index: 0, problem: "page" },
    { at: "anchors", index: 0, problem: "offset" },
    { at: "anchors", index: 1, problem: "id" },
    { at: "anchors", index: 1, problem: "duration" },
    { at: "anchors", index: 2, problem: "id" },
    { at: "anchors", index: 2, problem: "offset" },
    { at: "background", index: 0, problem: "file" },
    { at: "background", index: 0, problem: "anchor" },
    { at: "foreground", index: 0, problem: "target" },
    { at: "foreground", index: 1, problem: "target" },
  ]);
});
