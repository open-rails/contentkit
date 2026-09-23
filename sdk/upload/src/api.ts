import { UploadError, aborted, fromResponse } from "./errors.js";
import type {
  CommitBody,
  CommitReply,
  CompleteReply,
  PartsBody,
  PartsReply,
  PresignBody,
  PresignReply,
  SlotBody,
  TicketBody,
} from "./wire.gen.js";

export interface ApiOptions {
  /** Base URL where the host mounts media.UploadHandler, e.g. "/api/media/upload". */
  endpoint: string;
  /** Extra headers per call (auth, CSRF). */
  headers?: () => HeadersInit | Promise<HeadersInit>;
  credentials?: RequestCredentials;
  fetch?: typeof fetch;
}

/** The upload API of media.UploadHandler; one method per route. */
export class UploadApi {
  constructor(private readonly o: ApiOptions) {}

  presign(b: PresignBody, signal?: AbortSignal) {
    return this.call<PresignReply>("/presign", b, signal);
  }
  parts(b: PartsBody, signal?: AbortSignal) {
    return this.call<PartsReply>("/parts", b, signal);
  }
  listParts(b: TicketBody, signal?: AbortSignal) {
    return this.call<PartsReply>("/parts/list", b, signal);
  }
  complete(b: TicketBody, signal?: AbortSignal) {
    return this.call<CompleteReply>("/complete", b, signal);
  }
  abort(b: TicketBody, signal?: AbortSignal) {
    return this.call<void>("/abort", b, signal);
  }
  commit(b: CommitBody, signal?: AbortSignal) {
    return this.call<CommitReply>("/commit", b, signal);
  }
  commitSlot(b: SlotBody, signal?: AbortSignal) {
    return this.call<void>("/commit-slot", b, signal);
  }

  private async call<T>(path: string, body: unknown, signal?: AbortSignal): Promise<T> {
    const f = this.o.fetch ?? fetch;
    const headers = new Headers(await this.o.headers?.());
    headers.set("Content-Type", "application/json");
    let res: Response;
    try {
      res = await f(this.o.endpoint.replace(/\/$/, "") + path, {
        method: "POST",
        headers,
        body: JSON.stringify(body),
        credentials: this.o.credentials ?? "same-origin",
        signal: signal ?? null,
      });
    } catch (err) {
      if (signal?.aborted) throw aborted(signal);
      throw new UploadError("network", `upload API ${path} unreachable`, 0, undefined, { cause: err });
    }
    if (!res.ok) throw await fromResponse(res);
    return (res.status === 204 ? undefined : await res.json()) as T;
  }
}
