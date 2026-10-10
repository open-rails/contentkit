import { UploadError } from "./errors.js";

const contentId = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

/** Content ids are canonical lowercase UUIDv7s (contentref.ValidateID). */
export const isContentId = (id: string) => contentId.test(id);

/** Refuses a ref whose id ContentKit would reject, before any request. */
export function checkRef(ref: { id: string } | undefined) {
  if (ref && !isContentId(ref.id)) throw new UploadError("invalid_request", `content id must be a canonical UUIDv7, got "${ref.id}"`);
}
