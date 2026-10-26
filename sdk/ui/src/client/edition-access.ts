import type { ContentsClient } from "./contents.js";
import type { Content, Rule, Version } from "./generated/wire.js";

/** An explicit edition selection never falls back to a sibling. */
export function selectedEdition(content: Pick<Content, "versions" | "selected_version_id">, versionID?: string): Version | undefined {
  const id = versionID ?? content.selected_version_id;
  if (id) return content.versions.find((version) => version.id === id);
  return content.versions.find((version) => version.state !== "deleted");
}

/** A channel's parent policy governs every edition; channel-free items retain edition policies. */
export function contentAccess(content: Content, versionID?: string): Rule | undefined {
  return content.channel_id ? content.access : selectedEdition(content, versionID)?.access;
}

/** Save at the authoritative layer and return the updated content view. */
export async function saveContentAccess(contents: Pick<ContentsClient, "update" | "updateVersion">, content: Content, access: Rule, versionID?: string): Promise<Content> {
  if (content.channel_id) return contents.update(content.id, { access, revision: content.revision });
  const version = selectedEdition(content, versionID);
  if (!version) throw new Error("contentkit-ui: select an existing edition before changing access");
  const updated = await contents.updateVersion(content.id, version.id, { access });
  return { ...content, versions: content.versions.map((entry) => entry.id === updated.id ? updated : entry) };
}
