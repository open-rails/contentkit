import { useCallback, useEffect, useMemo, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import { ContentKitError, throwIfAborted, toContentKitError } from "../client/errors.js";
import type { FileDownload, Pending, PlanSegment, ReaderPlan } from "../client/generated/wire.js";
import { isPending } from "../client/versions.js";
import { sleep } from "../client/media/client.js";
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
  /** SHA-256 of the original source image; stable across renaming and reordering. */
  id: string;
  /** Display name only; never a page reference. */
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
      return { id: p.id, name: p.name, w: p.w, h: p.h, url: path ? urls.get(path) : undefined };
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
 * original or rendition its kind serves, or its pages as a zip. Polls while
 * it is prepared (a zip is built after its pages publish, behind the worker
 * queue); render_timeout after ten minutes.
 */
export function useVersionDownload(
  content: string,
  version: string,
  o: { poll?: number; client?: ContentKitClient; start?: (d: FileDownload) => void } = {},
): UseVersionDownload {
  const { client, scope } = useContentScope(o.client);
  const m = useMessages();
  const owner = useMemo(() => ({ client, scope, content, version, active: undefined as AbortController | undefined }), [client, scope, content, version]);
  const [state, setState] = useState<{ owner: typeof owner; preparing: boolean; error?: ContentKitError; retryAt?: number }>();
  useEffect(() => () => owner.active?.abort(), [owner]);
  const current = state?.owner === owner ? state : undefined;
  const start = o.start ?? startDownload;
  const download = useCallback(async () => {
    owner.active?.abort();
    const controller = owner.active = new AbortController();
    const { signal } = controller;
    setState({ owner, preparing: true });
    const until = Date.now() + 600_000;
    try {
      for (;;) {
        const out = await client.versions.download(content, version, signal);
        // Also reject a late result from a transport that ignored cancellation.
        throwIfAborted(signal);
        if (!isPending(out)) {
          start(out);
          return out;
        }
        if (Date.now() >= until) throw new ContentKitError("render_timeout", "the download is still being prepared");
        await sleep(o.poll ?? 3000, signal);
      }
    } catch (e) {
      const error = toContentKitError(e);
      if (!signal.aborted && owner.active === controller) {
        setState({ owner, preparing: false, error, retryAt: error.code === "rate_limited" ? Date.now() + (error.retryAfter ?? 60) * 1000 : undefined });
      }
      throw error;
    } finally {
      if (!signal.aborted && owner.active === controller) setState((state) => state?.owner === owner ? { ...state, preparing: false } : state);
    }
  }, [client, content, version, o.poll, start, owner]);
  return { download, preparing: current?.preparing ?? false, error: current?.error, retryAt: current?.retryAt, message: downloadMessage(m, current?.error) };
}
