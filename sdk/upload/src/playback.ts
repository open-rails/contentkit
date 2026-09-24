/**
 * Why a video cannot play. `network` covers CORS too: browsers report a blocked
 * cross-origin response exactly like an unreachable server (status 0).
 */
export type PlaybackErrorKind = "network" | "access" | "not_found" | "rate_limited" | "unsupported" | "stalled" | "unknown";

export interface PlaybackError {
  kind: PlaybackErrorKind;
  /** Short support code, e.g. "fragLoadError/0" or "media/4". */
  code: string;
  status?: number;
}

/** The fields of hls.js ErrorData this reads. */
export interface HlsErrorLike {
  type?: string;
  details: string;
  fatal?: boolean;
  response?: { code?: number } | null;
  networkDetails?: unknown;
}

const UNSUPPORTED = new Set([
  "manifestParsingError",
  "manifestIncompatibleCodecsError",
  "levelParsingError",
  "levelEmptyError",
  "fragParsingError",
  "bufferAddCodecError",
  "bufferIncompatibleCodecsError",
  "bufferAppendError",
  "attachMediaError",
]);

/** HTTP status → kind; 0 (or none) is a blocked or unreachable request. */
export function statusKind(status: number | undefined): PlaybackErrorKind {
  if (status === 401 || status === 403) return "access";
  if (status === 404 || status === 410) return "not_found";
  if (status === 429) return "rate_limited";
  return "network";
}

function hlsStatus(d: HlsErrorLike): number | undefined {
  const code = d.response?.code;
  if (typeof code === "number") return code;
  const s = (d.networkDetails as { status?: unknown } | null | undefined)?.status;
  return typeof s === "number" ? s : undefined;
}

export function classifyHlsError(d: HlsErrorLike): PlaybackError {
  if (UNSUPPORTED.has(d.details)) return { kind: "unsupported", code: d.details };
  if (d.type === "networkError" || /Load(Error|TimeOut)$/i.test(d.details)) {
    const status = /TimeOut$/i.test(d.details) ? 0 : (hlsStatus(d) ?? 0);
    return { kind: statusKind(status), code: `${d.details}/${status}`, status };
  }
  return { kind: d.type === "mediaError" && d.fatal ? "unsupported" : "unknown", code: d.details };
}

/** A native `<video>` error (Safari's HLS): 2 network, 3 decode, 4 unsupported source. */
export function classifyMediaError(e: { code: number } | null | undefined): PlaybackError {
  const code = e?.code ?? 0;
  const kind: PlaybackErrorKind = code === 2 ? "network" : code === 3 || code === 4 ? "unsupported" : "unknown";
  return { kind, code: `media/${code}` };
}

// Fail fast: a dead endpoint surfaces in seconds, a blip gets 1–2 quick retries.
const noRetryStatus = new Set([401, 403, 404, 410, 429]);
const retry = (maxNumRetry: number) => ({
  maxNumRetry,
  retryDelayMs: 500,
  maxRetryDelayMs: 2000,
  shouldRetry: (_c: unknown, _n: number, _t: boolean, res: { code?: number } | undefined, fallback: boolean) =>
    !noRetryStatus.has(res?.code ?? -1) && fallback,
});
const policy = (ttfb: number, load: number) => ({
  default: { maxTimeToFirstByteMs: ttfb, maxLoadTimeMs: load, timeoutRetry: retry(1), errorRetry: retry(2) },
});

/** hls.js settings the player uses; spread under your own. */
export const hlsConfig = {
  manifestLoadPolicy: policy(5_000, 10_000),
  playlistLoadPolicy: policy(5_000, 10_000),
  fragLoadPolicy: policy(6_000, 30_000),
  keyLoadPolicy: policy(5_000, 10_000),
};

/** Non-fatal network failures (hls.js also tries other renditions) before the player gives up. */
export const NETWORK_FAILURES_BEFORE_ERROR = 3;
