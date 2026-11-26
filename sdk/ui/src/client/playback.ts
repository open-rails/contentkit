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

/** Read again this long before the item token expires. */
export const REFRESH_BEFORE_EXPIRY = 60_000;
const MIN_REFRESH_DELAY = 30_000;

/**
 * Milliseconds until the read should be fetched again: shortly before
 * `expires` (the read's, unix seconds), never sooner than 30 s; null when
 * the read has no expiry. A read before the expiry answers the same token,
 * so nothing is gained by reading earlier.
 */
export function expiryDelay(expires: number | undefined, now = Date.now()): number | null {
  if (!expires) return null;
  return Math.max(expires * 1000 - REFRESH_BEFORE_EXPIRY - now, MIN_REFRESH_DELAY);
}

/**
 * Worth one grant refresh and retry: 401/403, or a 404 — media-gateway answers a
 * missing, expired or wrong token exactly like a missing object. A 429 (the
 * ingress's rate limit) or a blocked request is not: it fails without a loop.
 */
export function refreshable(e: PlaybackError): boolean {
  return e.kind === "access" || e.status === 404;
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

/** Adaptive-bitrate policy for the player; every field is optional. */
export interface AbrPolicy {
  /** Start at, and never cap the display below, the rung of this short side (px) when bandwidth allows. Default 1080. */
  minStartHeight?: number;
  /** Never pick a rung with a larger short side (px). Default unlimited. */
  maxHeight?: number;
  /** false: stock hls.js start (500 kbps guess) and a strict display cap. Default true. */
  preferHighRes?: boolean;
  /** Bits/s assumed when neither a measurement nor `navigator.connection` is available. Default 8 Mbps. */
  defaultEstimate?: number;
}

/** A rendition as hls.js levels describe it. */
export interface Rung {
  width: number;
  height: number;
  /** Peak bits/s (BANDWIDTH). */
  bitrate: number;
  /** AVERAGE-BANDWIDTH when known. */
  averageBitrate?: number;
}

/** What the browser says about the connection (Network Information API). */
export interface ConnectionHint {
  downlink?: number;
  saveData?: boolean;
}

export const ABR_DEFAULTS = { minStartHeight: 1080, maxHeight: Infinity, preferHighRes: true, defaultEstimate: 8_000_000 } as const;
const STOCK_ESTIMATE = 500_000;
// Start below the estimate: the first segment also pays for TTFB and decoder startup.
const START_FACTOR = 0.75;

// A rung within 10% of the display is sharp enough (a DPR-3 phone's 1170 px vs 1080p).
const UPSCALE = 1.1;

const short = (r: Rung) => Math.min(r.width, r.height) || r.height || r.width;

function resolve(p: AbrPolicy | undefined) {
  return { ...ABR_DEFAULTS, ...Object.fromEntries(Object.entries(p ?? {}).filter(([, v]) => v !== undefined)) } as Required<AbrPolicy>;
}

/**
 * The highest rung index (rungs ascend by bitrate) worth loading for a display
 * of width×height device pixels: the smallest rung covering the video's fitted
 * box (to within 10%), never below the minStartHeight rung, never above maxHeight.
 */
export function capRung(rungs: readonly Rung[], width: number, height: number, policy?: AbrPolicy): number {
  const p = resolve(policy);
  const n = rungs.length;
  if (!n) return -1;
  let cap = n - 1;
  if (width > 0 && height > 0) {
    for (let i = 0; i < n; i++) {
      const r = rungs[i]!;
      const scale = r.width && r.height ? Math.min(width / r.width, height / r.height) : 1;
      if (scale <= UPSCALE) {
        cap = i;
        break;
      }
    }
  }
  if (p.preferHighRes) {
    const floor = rungs.findIndex((r) => short(r) >= p.minStartHeight);
    cap = Math.max(cap, floor === -1 ? n - 1 : floor);
  }
  while (cap > 0 && short(rungs[cap]!) > p.maxHeight) cap--;
  return cap;
}

/** A level as hls.js lists it, for choosing what plays. */
export interface PlayableLevel {
  codecSet?: string;
  /** VIDEO-RANGE: "SDR" (or absent), "PQ" or "HLG". */
  videoRange?: string;
  videoCodec?: string;
  width: number;
  height: number;
  bitrate: number;
  frameRate?: number;
}

/** What the HDR choice reads of the browser; `globalThis` by default. */
export interface DisplayEnv {
  matchMedia?: (query: string) => { matches: boolean };
  navigator?: { mediaCapabilities?: { decodingInfo(c: MediaDecodingConfiguration): Promise<{ supported: boolean }> } };
}

export const isHdrLevel = (l: { videoRange?: string }) => !!l.videoRange && l.videoRange !== "SDR";

/** The screen shows high dynamic range (CSS `dynamic-range: high`). */
export function hdrDisplay(env: DisplayEnv = globalThis): boolean {
  try {
    return env.matchMedia?.("(dynamic-range: high)").matches === true;
  } catch {
    return false;
  }
}

/** MediaCapabilities confirms the browser decodes and renders level's HDR video. */
export async function decodesHdr(level: PlayableLevel, env: DisplayEnv = globalThis): Promise<boolean> {
  const caps = env.navigator?.mediaCapabilities;
  if (!caps?.decodingInfo || !level.videoCodec || !isHdrLevel(level)) return false;
  try {
    const info = await caps.decodingInfo({
      type: "media-source",
      video: {
        contentType: `video/mp4; codecs="${level.videoCodec}"`,
        width: level.width || 1920,
        height: level.height || 1080,
        bitrate: level.bitrate || 1,
        framerate: level.frameRate || 30,
        transferFunction: level.videoRange === "HLG" ? "hlg" : "pq",
        colorGamut: "rec2020",
      },
    });
    return info.supported === true;
  } catch {
    return false;
  }
}

/**
 * Levels to remove (indices, descending) so one dynamic range and one codec
 * set remain. HDR levels play only on an HDR display whose decoder confirms
 * them (each level asked), else SDR; a ladder without SDR keeps its HDR
 * levels. The codec set is that of the level hls.js lists first, the master
 * playlist's first playable codec (AV1 before H.264 when the browser decodes
 * it), when it has levels in that range, else the first such level's. hls.js
 * never switches codec sets for bandwidth, and the quality menu lists each
 * rung once. Video is never drawn through a canvas: the browser renders HDR.
 */
export async function otherLevels(levels: readonly PlayableLevel[], first: number, env: DisplayEnv = globalThis): Promise<number[]> {
  let hdr = levels.map(isHdrLevel);
  if (levels.some((l) => !isHdrLevel(l))) {
    const display = hdrDisplay(env);
    hdr = await Promise.all(levels.map((l) => (display && isHdrLevel(l) ? decodesHdr(l, env) : false)));
  }
  const kept = hdr.some(Boolean) ? hdr : levels.map((l) => !isHdrLevel(l));
  const lead = (levels[first] ?? levels[0])?.codecSet;
  const set = levels.some((l, i) => kept[i] && l.codecSet === lead) ? lead : levels.find((_, i) => kept[i])?.codecSet;
  return levels.flatMap((l, i) => (kept[i] && l.codecSet === set ? [] : [i])).reverse();
}

/** Bits/s to seed the estimator with: a measurement from this page, the browser's downlink, else the policy default. */
export function initialEstimate(policy?: AbrPolicy, conn?: ConnectionHint | null, measured?: number): number {
  const p = resolve(policy);
  if (conn?.saveData) return STOCK_ESTIMATE;
  if (measured && measured > 0) return measured;
  if (conn?.downlink && conn.downlink > 0) return conn.downlink * 1_000_000;
  return p.preferHighRes ? p.defaultEstimate : STOCK_ESTIMATE;
}

/** The first rung to load: the lowest under Save-Data, else the highest under cap the estimate sustains. */
export function startRung(rungs: readonly Rung[], estimate: number, cap: number, conn?: ConnectionHint | null): number {
  if (!rungs.length) return -1;
  if (conn?.saveData) return 0;
  let start = 0;
  for (let i = 0; i <= Math.min(cap, rungs.length - 1); i++) {
    const r = rungs[i]!;
    if ((r.averageBitrate || r.bitrate) <= estimate * START_FACTOR) start = i;
  }
  return start;
}

/** hls.js ABR settings: climb after a couple of good segments, drop before the buffer runs dry. */
export function abrHlsConfig(policy: AbrPolicy | undefined, estimate: number) {
  const p = resolve(policy);
  return {
    abrEwmaDefaultEstimate: estimate,
    abrEwmaDefaultEstimateMax: Math.max(estimate, 5_000_000),
    abrEwmaFastVoD: p.preferHighRes ? 2 : 3,
    abrEwmaSlowVoD: p.preferHighRes ? 6 : 9,
    abrBandWidthUpFactor: p.preferHighRes ? 0.8 : 0.7,
    abrBandWidthFactor: 0.9,
    capLevelToPlayerSize: true,
    ignoreDevicePixelRatio: false,
    maxBufferLength: 30,
    maxMaxBufferLength: 120,
    backBufferLength: 30,
  };
}
