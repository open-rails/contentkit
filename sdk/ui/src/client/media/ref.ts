import { ContentKitError } from "../errors.js";

const contentId = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

/** Content ids are canonical lowercase UUIDv7s (contentref.ValidateID). */
export const isContentId = (id: string) => contentId.test(id);

/** An item's identity: kind, id and, for a content's version, the version. */
export const itemKey = (ref: { kind: string; id: string; version?: string }) => (ref.version ? `${ref.kind}/${ref.id}/${ref.version}` : `${ref.kind}/${ref.id}`);

/** An item's ref and nothing else: kind, id and its version when it names one. */
export const itemRef = (ref: { kind: string; id: string; version?: string }) =>
  ref.version ? { kind: ref.kind, id: ref.id, version: ref.version } : { kind: ref.kind, id: ref.id };

/** Both refs name the same item (a version is its own item). */
export const sameItem = (a: { kind: string; id: string; version?: string }, b: { kind: string; id: string; version?: string }) =>
  a.kind === b.kind && a.id === b.id && (a.version ?? "") === (b.version ?? "");

/** Refuses a ref whose id ContentKit would reject, before any request. */
export function checkRef(ref: { id: string } | undefined) {
  if (ref && !isContentId(ref.id)) throw new ContentKitError("invalid_request", `content id must be a canonical UUIDv7, got "${ref.id}"`);
}
