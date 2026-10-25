import type { ContentsClient } from "./contents.js";
import type { Content, Rule, Version } from "./generated/wire.js";

/** The selected edition owns access even when a kind retains the historical
 * content access_scope default or hides its single edition in the UI.
 */
export function selectedEdition(content: Pick<Content, "versions" | "selected_version_id">, versionID?: string): Version | undefined {
  const id = versionID ?? content.selected_version_id;
  if (id) return content.versions.find((version) => version.id === id);
  return content.versions.find((version) => version.state !== "deleted");
}

/** Existing-item access edits always target exactly one edition. */
export function saveEditionAccess(contents: Pick<ContentsClient, "updateVersion">, content: Pick<Content, "id" | "versions" | "selected_version_id">, access: Rule, versionID?: string): Promise<Version> {
  const version = selectedEdition(content, versionID);
  if (!version) throw new Error("contentkit-ui: select an existing edition before changing access");
  return contents.updateVersion(content.id, version.id, { access });
}
