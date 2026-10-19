import type { ForegroundCue, ReaderMeta, ReaderPlan } from "./generated/wire.js";

/** Page hashes and display labels in reading order, plus audio track names. */
export interface ReaderNames {
  pages: { id: string; label: string }[];
  files: string[];
}

/** The names an editor's reader plan lists: every page of every segment, in order, and the playback tracks. */
export function readerNames(plan: ReaderPlan): ReaderNames {
  return { pages: plan.segments.flatMap((s) => (s.pages ?? []).map((p) => ({ id: p.id, label: p.name }))), files: (plan.playback?.tracks ?? []).map((t) => t.name) };
}

/** An empty schema-1 reader metadata. */
export const emptyReaderMeta = (): ReaderMeta => ({ schema_version: 1, anchors: [], background: [], foreground: [] });

const stem = (name: string) => {
  const i = name.lastIndexOf(".");
  return (i > 0 ? name.slice(0, i) : name).toLowerCase();
};

export interface AudioMatch {
  /** One cue per matched file: on the anchor its stem names, else the page. */
  foreground: ForegroundCue[];
  unmatchedPages: string[];
  unmatchedFiles: string[];
}

/** Match audio against display labels, but store only the source hash in page cues. */
export function matchAudio(names: ReaderNames, anchors: readonly string[] = []): AudioMatch {
  const byStem = new Map<string, string>();
  for (const f of names.files) if (!byStem.has(stem(f))) byStem.set(stem(f), f);
  const used = new Set<string>();
  const foreground: ForegroundCue[] = [];
  for (const anchor of anchors) {
    const file = byStem.get(stem(anchor));
    if (file && !used.has(file)) {
      foreground.push({ file, anchor });
      used.add(file);
    }
  }
  const unmatchedPages: string[] = [];
  const pageCounts = new Map<string, number>();
  const labelCounts = new Map<string, number>();
  for (const page of names.pages) {
    pageCounts.set(page.id, (pageCounts.get(page.id) ?? 0) + 1);
    const label = stem(page.label);
    labelCounts.set(label, (labelCounts.get(label) ?? 0) + 1);
  }
  for (const page of names.pages) {
    const file = byStem.get(stem(page.label));
    const unique = pageCounts.get(page.id) === 1 && labelCounts.get(stem(page.label)) === 1;
    if (unique && file && !used.has(file)) {
      foreground.push({ file, page: page.id });
      used.add(file);
    } else unmatchedPages.push(page.label);
  }
  return { foreground, unmatchedPages, unmatchedFiles: names.files.filter((f) => !used.has(f)) };
}

/** Where a problem is: an anchor, a cue or the defaults, by index. */
export interface ReaderMetaProblem {
  at: "anchors" | "background" | "foreground" | "defaults";
  index: number;
  /** id: empty or repeated; offset: outside 0-100; duration, seconds: not positive; page, anchor, file: names nothing; target: a foreground cue without a page or an anchor. */
  problem: "id" | "offset" | "duration" | "seconds" | "page" | "anchor" | "file" | "target";
}

/** What the server refuses, and a foreground cue without a place: so an editor sees it before saving. */
export function readerMetaProblems(meta: ReaderMeta, names: ReaderNames): ReaderMetaProblem[] {
  const out: ReaderMetaProblem[] = [];
  const pages = new Set<string>();
  const ambiguous = new Set<string>();
  for (const page of names.pages) {
    if (pages.has(page.id)) ambiguous.add(page.id);
    pages.add(page.id);
  }
  for (const id of ambiguous) pages.delete(id);
  const files = new Set(names.files);
  const ids = new Set<string>();
  const auto = meta.defaults?.auto_advance_seconds;
  if (auto !== undefined && !(auto > 0)) out.push({ at: "defaults", index: 0, problem: "seconds" });
  (meta.anchors ?? []).forEach((a, index) => {
    if (!a.id.trim() || ids.has(a.id)) out.push({ at: "anchors", index, problem: "id" });
    ids.add(a.id);
    if (!pages.has(a.page)) out.push({ at: "anchors", index, problem: "page" });
    if (!(a.offset_percent >= 0 && a.offset_percent <= 100)) out.push({ at: "anchors", index, problem: "offset" });
    if (a.duration_seconds !== undefined && !(a.duration_seconds > 0)) out.push({ at: "anchors", index, problem: "duration" });
  });
  const place = (at: ReaderMetaProblem["at"], index: number, page?: string, anchor?: string) => {
    if (page !== undefined && !pages.has(page)) out.push({ at, index, problem: "page" });
    if (anchor !== undefined && !ids.has(anchor)) out.push({ at, index, problem: "anchor" });
  };
  (meta.background ?? []).forEach((c, index) => {
    if (!files.has(c.file)) out.push({ at: "background", index, problem: "file" });
    place("background", index, c.from_page, c.from_anchor);
    place("background", index, c.to_page, c.to_anchor);
  });
  (meta.foreground ?? []).forEach((c, index) => {
    if (!files.has(c.file)) out.push({ at: "foreground", index, problem: "file" });
    if ((c.page === undefined) === (c.anchor === undefined)) out.push({ at: "foreground", index, problem: "target" });
    place("foreground", index, c.page, c.anchor);
  });
  return out;
}
