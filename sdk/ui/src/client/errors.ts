import { CONTENTKIT_ERROR_CODES, type ErrorCode } from "./generated/error-codes.js";
import type { BanNotice, ErrorDetails, ErrorReply, Failure } from "./generated/wire.js";

/**
 * Codes raised in the browser: network (no server reached), server_unavailable
 * (a gateway or timeout answered instead of ContentKit), storage (the bucket
 * refused a PUT), aborted (the caller's signal), resume_mismatch (saved state
 * is not for this file), decode (not readable as an image) and render_timeout
 * (still processing after the wait).
 */
export const CLIENT_ERROR_CODES = ["network", "server_unavailable", "storage", "aborted", "resume_mismatch", "decode", "render_timeout"] as const;

export type ContentKitErrorCode = ErrorCode | (typeof CLIENT_ERROR_CODES)[number];

/** Every code a ContentKitError can carry: the server's registry, then the client's. */
export const ERROR_CODES: readonly ContentKitErrorCode[] = [...(Object.keys(CONTENTKIT_ERROR_CODES) as ErrorCode[]), ...CLIENT_ERROR_CODES];

export interface ContentKitErrorInit extends ErrorOptions {
  status?: number;
  retryAfter?: number;
  details?: ErrorDetails;
  blobs?: string[];
  ban?: BanNotice;
  action?: string;
  reason?: string;
  availableAt?: string;
  unknownCode?: string;
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
  /** A download_unavailable's code of the host's policy ("release_window"). */
  readonly reason?: string;
  /** A download_unavailable's opening time (RFC 3339), when a matter of time. */
  readonly availableAt?: string;
  /** A server code this client doesn't know (a newer server): `message` is the server's own and `code` its status's. */
  readonly unknownCode?: string;

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
    this.reason = init.reason;
    this.availableAt = init.availableAt;
    this.unknownCode = init.unknownCode;
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
      this.code === "server_unavailable" ||
      this.code === "internal_error" ||
      this.code === "unavailable" ||
      (this.code === "storage" && (this.status >= 500 || this.status === 403 || this.status === 408 || this.status === 429))
    );
  }
}

export const isContentKitError = (e: unknown): e is ContentKitError => e instanceof ContentKitError;

/** Anything a call can throw, as a ContentKitError: a transport failure's code, else internal_error. */
export function toContentKitError(e: unknown): ContentKitError {
  if (e instanceof ContentKitError) return e;
  return new ContentKitError(transportFailure(e) ?? "internal_error", e instanceof Error ? e.message : String(e), { cause: e });
}

/** A fetch that got no response: the caller's abort, a timeout, else no server reached. */
export function requestFailure(err: unknown, signal: AbortSignal | undefined, target: string): ContentKitError {
  const code = transportFailure(err);
  if (code !== "server_unavailable" && signal?.aborted) return aborted(signal);
  return code === "server_unavailable"
    ? new ContentKitError(code, `${target} did not respond`, { cause: err })
    : new ContentKitError("network", `${target} unreachable`, { cause: err });
}

// A gateway answered for ContentKit: down, restarting or timed out (52x: Cloudflare).
const GATEWAY_STATUS = new Set([502, 503, 504, 520, 521, 522, 523, 524]);

// fetch rejects with a TypeError when no server answers (offline, DNS,
// connection refused, CORS); engines word it differently.
const FETCH_FAILURE = /fetch|network|load failed|connection/i;

/**
 * The client code of a request no server answered (mirrors auth-ui's and
 * billing-ui's): network when none was reached, server_unavailable when a
 * gateway or timeout answered. Recognizes the sibling SDKs' codes too.
 */
export function transportFailure(error: unknown): "network" | "server_unavailable" | undefined {
  if (error instanceof TypeError) {
    const offline = globalThis.navigator?.onLine === false;
    return offline || FETCH_FAILURE.test(error.message) ? "network" : undefined;
  }
  if (!error || typeof error !== "object") return undefined;
  const { name, code, status } = error as { name?: unknown; code?: unknown; status?: unknown };
  if (name === "TimeoutError") return "server_unavailable";
  if (code === "network" || code === "network_error") return "network";
  if (code === "server_unavailable") return code;
  if (code === undefined || code === "unknown_error" || code === "server_error")
    return typeof status === "number" && GATEWAY_STATUS.has(status) ? "server_unavailable" : undefined;
  return undefined;
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
  429: "rate_limited",
  501: "not_configured",
  503: "unavailable",
};

const known = new Set<string>(ERROR_CODES);

/** Reads a response that is not 2xx. */
export async function readContentKitError(res: Response): Promise<ContentKitError> {
  let body: Partial<ErrorReply> = {};
  try {
    body = (await res.json()) as Partial<ErrorReply>;
  } catch {
    // not JSON (a proxy error page)
  }
  const header = Number(res.headers.get("Retry-After"));
  const retryAfter = body.retry_after ?? (Number.isFinite(header) && header > 0 ? header : undefined);
  const code =
    body.code && known.has(body.code)
      ? (body.code as ContentKitErrorCode)
      : !body.code && GATEWAY_STATUS.has(res.status)
        ? "server_unavailable"
        : (byStatus[res.status] ?? (res.status >= 500 ? "internal_error" : "invalid_request"));
  return new ContentKitError(code, body.error ?? `HTTP ${res.status}`, {
    status: res.status,
    retryAfter,
    blobs: body.blobs,
    details: body.details,
    ban: body.ban,
    action: body.action,
    reason: body.reason,
    availableAt: body.available_at,
    unknownCode: body.code && !known.has(body.code) ? body.code : undefined,
  });
}

/** An upload's recorded processing failure: its typed refusal, else a processing fault. */
export function failureError(f: Failure): ContentKitError {
  const code = f.code && known.has(f.code) ? (f.code as ContentKitErrorCode) : "internal_error";
  return new ContentKitError(code, f.message, { details: f.details });
}
