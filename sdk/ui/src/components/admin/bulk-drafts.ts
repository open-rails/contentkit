import type { ContentKitClient } from "../../client/client.js";
import { ContentKitError } from "../../client/errors.js";
import type { Content, RefBody } from "../../client/generated/wire.js";
import { isPattern, ruleDir, screenFiles } from "../../client/media/rules.js";

const words = (s: string) => s.replace(/[_-]+/g, " ").replace(/\s+/g, " ").trim();

/** A file name as a title: no extension or [bracketed] circle, artist and tag groups, separators as spaces. */
export const fileTitle = (name: string) => {
  const stem = name.replace(/\.[^.]+$/, "");
  return (words(stem.replace(/\[[^\]]*\]/g, " ")) || words(stem)).slice(0, 300);
};

/** A draft of kind titled from file, and the ref of its first version (uploads land there). */
export async function createDraft(client: ContentKitClient, kind: string, language: string, file: File, untitled: string): Promise<{ content: Content; ref: RefBody }> {
  const content = await client.contents.create({ kind, text: { [language]: { title: fileTitle(file.name) || untitled } } });
  const version = [...(content.versions ?? [])].sort((a, b) => a.position - b.position)[0];
  if (!version) throw new ContentKitError("invalid_request", `kind ${kind} made a draft without a version`);
  return { content, ref: { kind, id: content.id, version: version.id } };
}

/**
 * Where file uploads in ref, by the item's own upload rules: a page archive to
 * its import path (expanded into pages server-side), a video to its source.
 */
export async function uploadTarget(client: ContentKitClient, ref: RefBody, file: File, signal?: AbortSignal): Promise<{ path: string; type: string }> {
  const { uploads } = await client.media.read(ref, { editor: true, limit: 1, signal });
  if (!uploads) throw new ContentKitError("forbidden", `only editors of ${ref.kind} ${ref.id} upload to it`);
  // Pages first (not covers), then other {name} paths, then literal ones such as a video's source.
  const paths = uploads.filter((r) => isPattern(r.path) && !r.named);
  const literal = uploads.filter((r) => !isPattern(r.path));
  const { accepted, refused } = screenFiles([file], [...paths.filter((r) => r.page), ...paths.filter((r) => !r.page), ...literal]);
  const pick = accepted[0];
  if (!pick) throw refused[0]?.error ?? new ContentKitError("type_not_allowed", `${file.name}: type not allowed`);
  return { path: isPattern(pick.rule.path) ? ruleDir(pick.rule.path) + file.name : pick.rule.path, type: pick.file.type };
}
