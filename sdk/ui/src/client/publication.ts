import type { Content, Version } from "./generated/wire.js";

/** Editor-only status of the accepted publication operation. Older responses
 * may omit it; reader availability is always the separate version.live value.
 */
export function publicationOf(version: Version): { state: string; scheduled_at: string; error?: string } | undefined {
  return (version as Version & { publication?: { state: string; scheduled_at: string; error?: string } }).publication;
}

export function publicationPhase(version: Version, now = Date.now()): "scheduled" | "pending" | "waiting" | "running" | "failed" | null {
  const publication = publicationOf(version);
  if (!publication || publication.state === "completed" || publication.state === "cancelled") return null;
  if (publication.state === "failed") return "failed";
  if (Date.parse(publication.scheduled_at) > now) return "scheduled";
  if (publication.state === "waiting" || publication.state === "running") return publication.state;
  return "pending";
}

/** A live edition can still have an unfinished replacement. Wait for due
 * accepted work, returning future schedules and durable failures to the caller.
 */
export function publicationPendingNow(content: Pick<Content, "live" | "state" | "publish_at" | "versions">, now = Date.now()): boolean {
  const versions = (content.versions ?? []).filter((version) => version.state === "published");
  if (versions.some((version) => publicationPhase(version, now) === "failed")) return false;
  return versions.some((version) => {
    const phase = publicationPhase(version, now);
    if (phase === "scheduled") return false;
    if (phase !== null) return true;
    if (publicationOf(version)) return false;
    const scheduled = Math.max(Date.parse(content.publish_at ?? "") || 0, Date.parse(version.publish_at ?? "") || 0);
    return content.state === "published" && !version.live && scheduled <= now;
  });
}

/** Observe only accepted editor work; a timestamp never changes local access. */
export function nextPublicationRefresh(content: (Pick<Content, "versions"> & Partial<Pick<Content, "state" | "publish_at">>) | null, now = Date.now()): number | null {
  const due = (content?.versions ?? []).flatMap((version) => {
    const phase = publicationPhase(version, now);
    const publication = publicationOf(version);
    if (!publication) {
      // Text-only catalogs use the existing publication worker without a
      // media-copy request. Their intended state still needs observation.
      if (content?.state !== "published" || version.state !== "published" || version.live) return [];
      const scheduled = Math.max(Date.parse(content.publish_at ?? "") || 0, Date.parse(version.publish_at ?? "") || 0);
      return [scheduled > now ? scheduled : now + 2000];
    }
    if (phase === null || phase === "failed") return [];
    return [phase === "scheduled" ? Date.parse(publication.scheduled_at) : now + 2000];
  });
  return due.length ? Math.min(...due) : null;
}
