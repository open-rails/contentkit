import { useCallback, useEffect, useMemo, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { FileDownload, Pending, PlanSegment, ReaderPlan } from "../client/generated/wire.js";
import { isPending } from "../client/versions.js";
import { useMessages } from "../i18n/context.js";
import type { Translator } from "../i18n/messages.js";
import { useMediaRead, type ReadWindow } from "./read.js";
import { keyOf, useContentScope, useResource } from "./use-resource.js";

export interface UseReaderPlan {
  /** null while loading, pending or failed. */
  plan: ReaderPlan | null;
  /** The version has no current publication yet; the hook asks again. */
  pending: boolean;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
}

// setTimeout's longest delay.
const MAX_DELAY = 2 ** 31 - 1;

/** When the plan changes by itself: its next scheduled segment's release. */
export function nextRelease(plan: ReaderPlan | null, now = Date.now()): number | null {
  const at = (plan?.segments ?? []).flatMap((s) => (!s.released && s.release_at ? [Date.parse(s.release_at)] : [])).filter((t) => t > now);
  return at.length ? Math.min(...at) : null;
}

/**
 * A version's reader plan for this viewer: its segments (released or not,
 * open or locked), previews, playback item and reader metadata. It asks again
 * while the version is being published and when its next segment is released.
 */
export function useReaderPlan(content: string | null | undefined, version: string | null | undefined, o: { poll?: number; client?: ContentKitClient } = {}): UseReaderPlan {
  const { client, store, scope } = useContentScope(o.client);
  const key = content && version ? keyOf("reader-plan", scope, content, version) : null;
  const r = useResource<ReaderPlan | Pending>(store, key, { type: "reader-plan", id: version ?? undefined }, (s) => client.versions.reader(content!, version!, s));
  const pending = isPending(r.data);
  const plan = r.data && !pending ? (r.data as ReaderPlan) : null;
  const { data, reload } = r;
  // Every answer schedules the next look: a pending one polls, a plan waits for its next release.
  useEffect(() => {
    const at = isPending(data) ? Date.now() + (o.poll ?? 5000) : nextRelease(data ? (data as ReaderPlan) : null);
    if (at === null) return;
    const t = setTimeout(reload, Math.min(MAX_DELAY, Math.max(0, at - Date.now()) + 500));
    return () => clearTimeout(t);
  }, [data, reload, o.poll]);
  return { plan, pending, loading: !!key && (r.loading || !r.loaded), error: r.error, reload };
}

/** A segment's page with the URL of one rendition, once its item's read signed it. */
export interface ReaderPage {
  name: string;
  w: number;
  h: number;
  /** undefined until read, or when the page lacks the rendition. */
  url?: string;
}

export interface UseSegmentPages {
  pages: ReaderPage[];
  loading: boolean;
  error?: ContentKitError;
}

/**
 * An open segment's pages with URLs of one rendition, read through its
 * published item (windowed for long segments, refreshed before the URLs expire).
 */
export function useSegmentPages(segment: PlanSegment | null | undefined, rendition: string, o: { window?: ReadWindow; client?: ContentKitClient } = {}): UseSegmentPages {
  const item = segment?.item ?? null;
  const read = useMediaRead(item ?? { kind: "", id: "" }, { window: o.window, read: item ? undefined : null, client: o.client });
  const pages = useMemo(() => {
    const urls = new Map((read.read?.files ?? []).flatMap((f) => (f.url ? [[f.path, f.url] as const] : [])));
    return (segment?.pages ?? []).map((p) => {
      const path = p.files[rendition];
      return { name: p.name, w: p.w, h: p.h, url: path ? urls.get(path) : undefined };
    });
  }, [segment, rendition, read.read]);
  return { pages, loading: !!item && read.loading, error: read.error };
}

export interface UseVersionDownload {
  /**
   * Grants the download (asking again while it is prepared), starts it in the
   * browser and resolves with the grant.
   */
  download: () => Promise<FileDownload>;
  /** Being prepared by the server, or granted and starting. */
  preparing: boolean;
  error?: ContentKitError;
  /** While a quota refused: when the viewer may download again (ms). */
  retryAt?: number;
  /**
   * What to tell the viewer about the last refusal: when a quota frees, when
   * a download the site holds back opens ("available to download in N
   * days"), a locked version, nothing to download, or a failure.
   */
  message?: string;
}

/** Starts a granted download: the browser fetches its URL, an attachment. */
export function startDownload(d: FileDownload): void {
  const a = document.createElement("a");
  a.href = d.url;
  a.download = d.name;
  a.rel = "noopener";
  a.style.display = "none";
  document.body.appendChild(a);
  a.click();
  a.remove();
}

/** The viewer's line for a download refusal at now. */
export function downloadMessage(m: Translator, err: ContentKitError | undefined, now = Date.now()): string | undefined {
  switch (err?.code) {
    case undefined:
      return undefined;
    case "rate_limited":
      return m.t("versionDownload.limited", { minutes: Math.max(1, Math.ceil((err.retryAfter ?? 60) / 60)) });
    case "download_unavailable": {
      const ms = err.availableAt ? Date.parse(err.availableAt) - now : NaN;
      return ms > 0 ? m.plural("versionDownload.availableIn", Math.max(1, Math.ceil(ms / 86_400_000))) : m.t("versionDownload.unavailable");
    }
    case "forbidden":
      return m.t("versionDownload.locked");
    case "no_download":
      return m.error(err);
    default:
      return m.t("versionDownload.failed");
  }
}

/**
 * A version's download, charged to the viewer's download quota: the
 * original or rendition its kind serves, or its pages as a zip.
 */
export function useVersionDownload(
  content: string,
  version: string,
  o: { poll?: number; client?: ContentKitClient; start?: (d: FileDownload) => void } = {},
): UseVersionDownload {
  const { client } = useContentScope(o.client);
  const m = useMessages();
  const [preparing, setPreparing] = useState(false);
  const [error, setError] = useState<ContentKitError>();
  const [retryAt, setRetryAt] = useState<number>();
  const start = o.start ?? startDownload;
  const download = useCallback(async () => {
    setError(undefined);
    setRetryAt(undefined);
    setPreparing(true);
    try {
      for (;;) {
        const out = await client.versions.download(content, version);
        if (!isPending(out)) {
          start(out);
          return out;
        }
        await new Promise((r) => setTimeout(r, o.poll ?? 3000));
      }
    } catch (e) {
      const err = toContentKitError(e);
      setError(err);
      if (err.code === "rate_limited") setRetryAt(Date.now() + (err.retryAfter ?? 60) * 1000);
      throw err;
    } finally {
      setPreparing(false);
    }
  }, [client, content, version, o.poll, start]);
  return { download, preparing, error, retryAt, message: downloadMessage(m, error) };
}
