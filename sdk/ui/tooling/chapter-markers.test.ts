import assert from "node:assert/strict";
import test from "node:test";
import { chapterRanges } from "../src/client/chapters.ts";
import { nextPublicationRefresh, publicationPhase } from "../src/client/publication.ts";
import type { Version } from "../src/client/generated/wire.ts";

test("inserted pages follow manifest order up to the next chapter marker", () => {
  const files = ["pages/001.jpg", "pages/017.jpg", "pages/017c.jpg", "pages/019b.jpg", "pages/020.jpg", "pages/040.jpg", "pages/041.jpg"];
  const chapters = chapterRanges(files, ["pages/001.jpg", "pages/020.jpg", "pages/041.jpg"]);
  assert.equal(chapters.problem, null);
  assert.deepEqual(chapters.ranges.map(({ start, end }) => files.slice(start, end)), [
    ["pages/001.jpg", "pages/017.jpg", "pages/017c.jpg", "pages/019b.jpg"],
    ["pages/020.jpg", "pages/040.jpg"],
    ["pages/041.jpg"],
  ]);
  const reordered = ["pages/001.jpg", "pages/020.jpg", "pages/017c.jpg", "pages/041.jpg"];
  const moved = chapterRanges(reordered, ["pages/001.jpg", "pages/020.jpg", "pages/041.jpg"]);
  assert.equal(moved.problem, null);
  assert.deepEqual(reordered.slice(moved.ranges[1]!.start, moved.ranges[1]!.end), ["pages/020.jpg", "pages/017c.jpg"]);
});

test("missing, duplicate and reordered start markers cannot silently retarget a chapter", () => {
  assert.equal(chapterRanges(["pages/001.jpg", "pages/020.jpg"], ["pages/missing.jpg"]).problem, "missing");
  assert.equal(chapterRanges(["pages/001.jpg", "pages/020.jpg"], ["pages/020.jpg", "pages/001.jpg"]).problem, "order");
  assert.equal(chapterRanges(["pages/001.jpg", "pages/020.jpg"], ["pages/001.jpg", "pages/001.jpg"]).problem, "order");
  assert.equal(chapterRanges(["pages/001.jpg", "pages/001.jpg"], ["pages/001.jpg"]).problem, "duplicate");
  assert.equal(chapterRanges(["pages/001.jpg"], [""]).problem, "unselected");
});

test("publication observation preserves live content and stops on a durable failure", () => {
  const now = Date.parse("2026-10-25T12:00:00Z");
  const version = {
    id: "edition-a", live: true, state: "published",
    publication: { state: "pending", scheduled_at: "2026-10-25T13:00:00Z" },
  } as Version & { publication: { state: string; scheduled_at: string } };
  const content = { versions: [version] };
  assert.equal(publicationPhase(version, now), "scheduled");
  assert.equal(nextPublicationRefresh(content, now), now + 3_600_000);
  const afterDue = now + 3_600_001;
  assert.equal(publicationPhase(version, afterDue), "pending");
  assert.equal(nextPublicationRefresh(content, afterDue), afterDue + 2000);
  version.publication.state = "failed";
  assert.equal(publicationPhase(version, afterDue), "failed");
  assert.equal(nextPublicationRefresh(content, afterDue), null);
  assert.equal(version.live, true);
  assert.equal(nextPublicationRefresh({ versions: [{ live: true, state: "published" } as Version] }, now), null);
});
