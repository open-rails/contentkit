import type { ErrorCode as MediaErrorCode, ErrorDetails, Failure } from "./generated/wire.js";

// Every code the media handlers answer; the Record keeps the list exhaustive.
const MEDIA_CODES: Record<MediaErrorCode, true> = {
  invalid_request: true,
  unauthorized: true,
  forbidden: true,
  not_found: true,
  conflict: true,
  incomplete: true,
  not_uploaded: true,
  too_many_files: true,
  too_large: true,
  quota_exceeded: true,
  type_not_allowed: true,
  checksum_mismatch: true,
  rate_limited: true,
  unavailable: true,
  image_too_small: true,
  image_too_large: true,
  image_unreadable: true,
  animation_not_allowed: true,
  animation_too_long: true,
  animation_unsupported: true,
  video_too_long: true,
  video_too_large: true,
  video_over_budget: true,
  internal_error: true,
};

/** Codes the content, taxonomy and content-URL handlers add to the media ones. */
export const CONTENT_ERROR_CODES = ["moderation_rejected", "unprocessable", "comment_banned", "not_configured", "tenant_mismatch", "gone"] as const;

/**
 * Codes raised in the browser: network (no response), storage (the bucket
 * refused a PUT), aborted (the caller's signal), resume_mismatch (saved state
 * is not for this file), decode (not readable as an image) and render_timeout
 * (still processing after the wait).
 */
export const CLIENT_ERROR_CODES = ["network", "storage", "aborted", "resume_mismatch", "decode", "render_timeout"] as const;

export type ContentKitErrorCode = MediaErrorCode | (typeof CONTENT_ERROR_CODES)[number] | (typeof CLIENT_ERROR_CODES)[number];

/** Every code a ContentKitError can carry. */
export const ERROR_CODES: readonly ContentKitErrorCode[] = [...(Object.keys(MEDIA_CODES) as MediaErrorCode[]), ...CONTENT_ERROR_CODES, ...CLIENT_ERROR_CODES];

/** The ban that stops a comment (comment_banned). */
export interface BanNotice {
  scope: string;
  reason?: string;
  /** RFC 3339; absent for a permanent ban. */
  until?: string;
}

/** ContentKit's flat error body, as every module answers it. */
export interface ContentKitErrorBody {
  error: string;
  code: string;
  /** Seconds until a limit frees (rate_limited). */
  retry_after?: number;
  /** An image or video refusal's numbers. */
  details?: ErrorDetails;
  /** not_uploaded at commit: the blobs to upload again. */
  blobs?: string[];
  /** comment_banned: the ban that applies. */
  ban?: BanNotice;
  /** rate_limited: the limited interaction (comment, reaction, poll_vote, …). */
  action?: string;
}

export interface ContentKitErrorInit extends ErrorOptions {
  status?: number;
  retryAfter?: number;
  details?: ErrorDetails;
  blobs?: string[];
  ban?: BanNotice;
  action?: string;
}

export class ContentKitError extends Error {
  override readonly name = "ContentKitError";
  /** HTTP status; 0 for client-side codes. */
  readonly status: number;
  readonly retryAfter?: number;
  readonly details?: ErrorDetails;
  readonly blobs?: string[];
  readonly ban?: BanNotice;
  readonly action?: string;

  constructor(
    readonly code: ContentKitErrorCode,
    message: string,
    init: ContentKitErrorInit = {},
  ) {
    super(message, init.cause === undefined ? undefined : { cause: init.cause });
    this.status = init.status ?? 0;
    this.retryAfter = init.retryAfter;
    this.details = init.details;
    this.blobs = init.blobs;
    this.ban = init.ban;
    this.action = init.action;
  }

  /**
   * The request broke a rule the message states (a 4xx or a typed refusal),
   * as opposed to a fault on the server or the network: show it as is.
   */
  get refusal(): boolean {
    return (this.status >= 400 && this.status < 500 && this.code !== "storage") || this.code.startsWith("image_") || this.code === "decode";
  }

  /** Rate or quota refusals: stop starting new requests until the limit frees. */
  get isLimit(): boolean {
    return this.code === "rate_limited" || this.code === "quota_exceeded";
  }

  /** The commit would exceed the kind's file caps (too_many_files, 409): remove files first. */
  get isCeiling(): boolean {
    return this.code === "too_many_files";
  }

  /** Refusals that apply to every upload of this caller, not just this file. */
  get blocksQueue(): boolean {
    return this.isLimit || this.code === "unauthorized" || this.code === "forbidden";
  }

  /** Worth retrying the same request. */
  get transient(): boolean {
    return (
      this.code === "network" ||
      this.code === "internal_error" ||
      this.code === "unavailable" ||
      (this.code === "storage" && (this.status >= 500 || this.status === 403 || this.status === 408 || this.status === 429))
    );
  }
}

export const isContentKitError = (e: unknown): e is ContentKitError => e instanceof ContentKitError;

/** Anything a call can throw, as a ContentKitError (network when it is not one). */
export function toContentKitError(e: unknown): ContentKitError {
  if (e instanceof ContentKitError) return e;
  return new ContentKitError("network", e instanceof Error ? e.message : String(e), { cause: e });
}

export function aborted(signal?: AbortSignal): ContentKitError {
  return new ContentKitError("aborted", "request canceled", { cause: signal?.reason });
}

export function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted) throw aborted(signal);
}

const byStatus: Record<number, ContentKitErrorCode> = {
  401: "unauthorized",
  403: "forbidden",
  404: "not_found",
  409: "conflict",
  410: "gone",
  413: "too_large",
  415: "type_not_allowed",
  422: "unprocessable",
  429: "rate_limited",
  501: "not_configured",
  503: "unavailable",
};

const known = new Set<string>(ERROR_CODES);

/** Reads a response that is not 2xx. */
export async function readContentKitError(res: Response): Promise<ContentKitError> {
  let body: Partial<ContentKitErrorBody> = {};
  try {
    body = (await res.json()) as Partial<ContentKitErrorBody>;
  } catch {
    // not JSON (a proxy error page)
  }
  const header = Number(res.headers.get("Retry-After"));
  const retryAfter = body.retry_after ?? (Number.isFinite(header) && header > 0 ? header : undefined);
  const code = body.code && known.has(body.code) ? (body.code as ContentKitErrorCode) : (byStatus[res.status] ?? (res.status >= 500 ? "internal_error" : "invalid_request"));
  return new ContentKitError(code, body.error ?? `HTTP ${res.status}`, {
    status: res.status,
    retryAfter,
    blobs: body.blobs,
    details: body.details,
    ban: body.ban,
    action: body.action,
  });
}

/** An upload's recorded processing failure: its typed refusal, else a processing fault. */
export function failureError(f: Failure): ContentKitError {
  const code = f.code && known.has(f.code) ? (f.code as ContentKitErrorCode) : "internal_error";
  return new ContentKitError(code, f.message, { details: f.details });
}
