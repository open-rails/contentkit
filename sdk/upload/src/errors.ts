import type { ErrorCode, ErrorReply } from "./wire.gen.js";

/**
 * Server codes (ErrorReply.code) plus client-side ones:
 * network (no response), storage (the bucket refused a PUT), aborted
 * (the caller's signal) and resume_mismatch (saved state is not for this file).
 */
export type UploadErrorCode = ErrorCode | "network" | "storage" | "aborted" | "resume_mismatch";

export class UploadError extends Error {
  override readonly name = "UploadError";
  constructor(
    readonly code: UploadErrorCode,
    message: string,
    readonly status = 0,
    /** Seconds until a rate limit frees (rate_limited). */
    readonly retryAfter?: number,
    options?: ErrorOptions & { originals?: string[] },
  ) {
    super(message, options);
    this.originals = options?.originals;
  }

  /** not_uploaded at commit: the originals to upload again. */
  readonly originals?: string[];

  /** Rate or quota refusals: stop starting new uploads until the limit frees. */
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
      (this.code === "storage" && (this.status >= 500 || this.status === 403 || this.status === 408 || this.status === 429))
    );
  }
}

export function aborted(signal?: AbortSignal): UploadError {
  return new UploadError("aborted", "upload canceled", 0, undefined, { cause: signal?.reason });
}

export function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted) throw aborted(signal);
}

const byStatus: Record<number, ErrorCode> = {
  401: "unauthorized",
  403: "forbidden",
  404: "not_found",
  413: "too_large",
  415: "type_not_allowed",
  422: "checksum_mismatch",
  429: "rate_limited",
};

/** Maps an upload API response that is not 2xx. */
export async function fromResponse(res: Response): Promise<UploadError> {
  let body: Partial<ErrorReply> = {};
  try {
    body = (await res.json()) as Partial<ErrorReply>;
  } catch {
    // not JSON (a proxy error page)
  }
  const header = Number(res.headers.get("Retry-After"));
  const retryAfter = body.retry_after ?? (Number.isFinite(header) && header > 0 ? header : undefined);
  const code: UploadErrorCode =
    body.code ?? byStatus[res.status] ?? (res.status >= 500 ? "internal_error" : "invalid_request");
  return new UploadError(code, body.error ?? `HTTP ${res.status}`, res.status, retryAfter, { originals: body.originals });
}
