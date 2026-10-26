import type { ContentKitClient } from "../../client/client.js";
import { ContentKitError } from "../../client/errors.js";
import { allowsEntityScope } from "../../client/entity-scopes.js";
import type { Content, ContentModel, RefBody, Rule } from "../../client/generated/wire.js";
import { isPattern, ruleDir, screenFiles } from "../../client/media/rules.js";

const words = (s: string) => s.replace(/[_-]+/g, " ").replace(/\s+/g, " ").trim();

export interface BulkDraftDefaults {
  format?: string;
  language?: string;
  access?: Rule;
  artists?: readonly string[];
}

/** A file name as a title: no extension or [bracketed] / 【bracketed】 circle, artist and tag groups, separators as spaces. */
export const fileTitle = (name: string) => {
  const stem = name.replace(/\.[^.]+$/, "");
  let bare = stem;
  // Innermost groups first, so nested brackets go too.
  for (let prev = ""; prev !== bare; ) [prev, bare] = [bare, bare.replace(/\[[^[\]]*\]|【[^【】]*】/g, " ")];
  return (words(bare) || words(stem)).slice(0, 300);
};

/** A draft of kind titled from file, and the ref of its first version (uploads land there). */
export async function createDraft(client: ContentKitClient, kind: string, language: string, file: File, untitled: string): Promise<{ content: Content; ref: RefBody }> {
  const content = await client.contents.create({ kind, versions: [{ languages: [language], text: { [language]: { title: fileTitle(file.name) || untitled } } }] });
  const version = [...(content.versions ?? [])].sort((a, b) => a.position - b.position)[0];
  if (!version) throw new ContentKitError("invalid_request", `kind ${kind} made a draft without a version`);
  return { content, ref: { kind, id: content.id, version: version.id } };
}

/** Applies only chosen defaults; safe to repeat when a file is retried. */
export async function applyDraftDefaults(client: ContentKitClient, draft: { content: Content; ref: RefBody }, model: ContentModel, defaults: BulkDraftDefaults): Promise<Content> {
  let { content } = draft;
  const spec = model.kinds[content.kind];
  const versionAccess = spec?.access_scope === "version";
  if (defaults.format || defaults.access && !versionAccess) {
    content = await client.contents.update(content.id, {
      ...(defaults.format ? { attributes: { ...content.attributes, format: defaults.format } } : {}),
      ...(defaults.access && !versionAccess ? { access: defaults.access } : {}),
    });
  }
  if (defaults.language || defaults.access && versionAccess) {
    await client.contents.updateVersion(content.id, draft.ref.version!, {
      ...(defaults.language ? { languages: [defaults.language] } : {}),
      ...(defaults.access && versionAccess ? { access: defaults.access } : {}),
    });
  }
  if (defaults.artists?.length) {
    const set = { kinds: ["artist"], memberships: defaults.artists.map((group_id) => ({ group_id })) };
    if (allowsEntityScope(model.entity_kinds.artist, "content")) await client.entities.setSharedOfContent(content.id, set, defaults.language);
    else await client.entities.setOfVersion(content.id, draft.ref.version!, set, defaults.language);
  }
  return content;
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
