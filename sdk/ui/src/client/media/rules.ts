import { ContentKitError } from "../errors.js";
import type { ErrorDetails, ReadResult } from "../generated/wire.js";
import { fill, publicURL, type PublicPreset } from "../public.js";
import { ratio } from "../aspect.js";
import { stem } from "./client.js";

// The shapes of media.UploadRule and media.PresetRule as the contract states them.

/** A video or audio upload's limits in effect. */
export interface VideoLimits {
  max_seconds?: number;
  max_fps?: number;
  max_pixels?: number;
  max_work?: number;
}

/** One upload path's rules, as an editor read carries them. The server stays the authority. */
export interface UploadRule {
  /** A literal ("cover") or a pattern ("originals/{name}"). */
  path: string;
  types: string[];
  max_bytes: number;
  /** Files allowed at the path; absent is unlimited. */
  max?: number;
  /** The server names each upload (inline images). */
  named?: boolean;
  /** The video upload path whose frames this upload can take. */
  frames?: string;
  /** An image's edit bounds: its first public preset's "W:H" and narrowest width. */
  aspect?: string;
  min_width?: number;
  video?: VideoLimits;
  /** Display aspects (width/height) a video's HLS presets accept. */
  min_aspect?: number;
  max_aspect?: number;
}

/**
 * A kind's public preset (`GET /media/presets`). Its template names only the
 * kind's default image: current files carry a generation, so an item's image
 * comes from a read's `public`, never from this.
 */
export interface PresetRule {
  kind: string;
  name: string;
  from: string;
  base: string;
  namespace: string;
  to: string;
  widths: number[];
  aspect?: string;
  min_width?: number;
  first?: number;
}

/** A read with the kind's upload rules (editor reads carry them). */
export type RuledRead = ReadResult & { uploads?: UploadRule[] };

/** The read's upload rules; empty when the server sends none. */
export const uploadRules = (read: ReadResult | null | undefined): UploadRule[] => (read as RuledRead | null | undefined)?.uploads ?? [];

const NAME = "{name}";

/** A pattern path ("originals/{name}") holds many files; a literal ("cover") one. */
export const isPattern = (path: string) => path.endsWith(NAME);

/** The directory of a pattern ("originals/"), or the literal itself. */
export const ruleDir = (path: string) => (isPattern(path) ? path.slice(0, -NAME.length) : path);

/** The rule an upload path (with or without its extension) falls under: a literal wins over a pattern. */
export function ruleFor<R extends { path: string }>(rules: readonly R[], path: string): R | undefined {
  const s = stem(path);
  return (
    rules.find((r) => !isPattern(r.path) && r.path === s) ??
    rules.find((r) => {
      if (!isPattern(r.path)) return false;
      const dir = ruleDir(r.path);
      return s.startsWith(dir) && s.length > dir.length && !s.slice(dir.length).includes("/");
    })
  );
}

/** Uploads of the read under rule (attached, in manifest order). */
export function filesFor(read: ReadResult | null | undefined, rule: { path: string }, rules: readonly { path: string }[] = [rule]) {
  return (read?.files ?? []).filter((f) => f.upload && !f.unattached && ruleFor(rules, f.path)?.path === rule.path);
}

const BY_EXTENSION: Record<string, string> = {
  jpg: "image/jpeg",
  jpeg: "image/jpeg",
  png: "image/png",
  webp: "image/webp",
  gif: "image/gif",
  avif: "image/avif",
  mp4: "video/mp4",
  m4v: "video/mp4",
  mov: "video/quicktime",
  mkv: "video/x-matroska",
  webm: "video/webm",
  m4a: "audio/mp4",
  mp3: "audio/mpeg",
  vtt: "text/vtt",
  srt: "application/x-subrip",
  ass: "text/x-ass",
  ssa: "text/x-ssa",
  zip: "application/zip",
};

/** The file with a content type from its extension when the browser left it empty or nonstandard (.mkv, .mov, .srt). */
export function withMediaType(file: File, allowed?: readonly string[]): File {
  if (file.type && (!allowed || allowed.includes(file.type))) return file;
  const type = BY_EXTENSION[file.name.split(".").pop()?.toLowerCase() ?? ""];
  return type && type !== file.type ? new File([file], file.name, { type, lastModified: file.lastModified }) : file;
}

/** `accept` for a file input from the rules' types (plus the extensions browsers leave untyped). */
export function acceptOf(rules: readonly UploadRule[]): string | undefined {
  const types = [...new Set(rules.flatMap((r) => r.types))];
  if (!types.length) return undefined;
  const exts = Object.entries(BY_EXTENSION).filter(([, t]) => types.includes(t)).map(([e]) => `.${e}`);
  return [...types, ...exts].join(",");
}

export interface Screened {
  /** Files to upload, each with its rule (typed by extension when needed). */
  accepted: { file: File; rule: UploadRule }[];
  /** Files the server would refuse, with the refusal it would answer. */
  refused: { file: File; error: ContentKitError }[];
}

/**
 * Checks files against the rules before any upload: a type some rule takes
 * (the first that does), its size, and the rule's file cap counting
 * have(rule) files already there. A rule without types, size or cap lets
 * that check through; the server decides.
 */
export function screenFiles(files: Iterable<File>, rules: readonly UploadRule[], have: (rule: UploadRule) => number = () => 0): Screened {
  const out: Screened = { accepted: [], refused: [] };
  const counts = new Map<UploadRule, number>();
  const allowed = [...new Set(rules.flatMap((r) => r.types))];
  for (const picked of files) {
    const file = withMediaType(picked, allowed);
    const rule = rules.find((r) => !r.types.length || r.types.includes(file.type));
    if (!rule) {
      const details: ErrorDetails = { type: file.type || undefined, allowed };
      out.refused.push({ file, error: new ContentKitError("type_not_allowed", `${file.name}: type not allowed`, { status: 415, details }) });
      continue;
    }
    if (rule.max_bytes > 0 && file.size > rule.max_bytes) {
      const details: ErrorDetails = { type: file.type, size: file.size, max_bytes: rule.max_bytes };
      out.refused.push({ file, error: new ContentKitError("too_large", `${file.name}: too large`, { status: 413, details }) });
      continue;
    }
    const n = counts.get(rule) ?? have(rule);
    if (rule.max && n >= rule.max) {
      const details = { max: rule.max } as ErrorDetails;
      out.refused.push({ file, error: new ContentKitError("too_many_files", `${file.name}: at most ${rule.max} files`, { status: 409, details }) });
      continue;
    }
    counts.set(rule, n + 1);
    out.accepted.push({ file, rule });
  }
  return out;
}

/** name, or name-2, name-3… so its stem is not among taken (names or paths without the directory). */
export function uniqueName(name: string, taken: Iterable<string>): string {
  const stems = new Set([...taken].map(stem));
  if (!stems.has(stem(name))) return name;
  const dot = name.lastIndexOf(".");
  const base = dot > 0 ? name.slice(0, dot) : name;
  const ext = dot > 0 ? name.slice(dot) : "";
  for (let n = 2; ; n++) {
    const candidate = `${base}-${n}${ext}`;
    if (!stems.has(stem(candidate))) return candidate;
  }
}

/** "16:9" style label of a width/height ratio: 2.4 → "2.4:1", 0.4167 → "1:2.4". */
export function ratioLabel(r: number): string {
  const round = (v: number) => String(Math.round(v * 100) / 100);
  return r >= 1 ? `${round(r)}:1` : `1:${round(1 / r)}`;
}

/**
 * The kind's default image for a preset, at every width: what the gateway
 * serves for an item without its own (a URL from the template never names a
 * current file). Empty for a preview preset.
 */
export function defaultImage(rule: PresetRule, id: string): PublicPreset {
  const r = ratio(rule.aspect);
  if (rule.first || rule.to.includes(NAME)) return { preset: rule.name, aspect: rule.aspect, renditions: [] };
  return {
    preset: rule.name,
    aspect: rule.aspect,
    renditions: rule.widths.map((w) => ({ url: publicURL(rule.base, rule.namespace, rule.kind, id, fill(rule.to, { w })), w, ...(r ? { h: Math.round(w / r) } : {}) })),
  };
}
