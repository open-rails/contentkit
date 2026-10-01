import { UploadError, aborted, fromResponse } from "./errors.js";
import { checkRef } from "./ref.js";
import type {
  CommitBody,
  CommitReply,
  CompleteReply,
  PartsBody,
  PartsReply,
  PresignBody,
  PresignReply,
  ReadResult,
  RefBody,
  TicketBody,
} from "./wire.gen.js";

export interface ApiOptions {
  /** Base URL where the host mounts media.UploadHandler, e.g. "/api/media/upload". */
  endpoint: string;
  /** Base URL where the host mounts Reader.Handler, e.g. "/api/media"; reads and HLS playlists live under it. */
  readEndpoint?: string;
  /** Extra headers per call (auth, CSRF). */
  headers?: () => HeadersInit | Promise<HeadersInit>;
  credentials?: RequestCredentials;
  fetch?: typeof fetch;
}

/** What a read returns (Reader.Read's ReadOptions). */
export interface ReadOptions {
  /** Only files under this path prefix, e.g. "low-res/". */
  prefix?: string;
  /** The range of files that get URLs. */
  offset?: number;
  limit?: number;
  /** Sign each file's download name into its URL. */
  download?: boolean;
  /** Editors also get the uploads: edit, frame, pending, failure and editor view. */
  editor?: boolean;
}

const trim = (u: string) => u.replace(/\/$/, "");

/** The upload API of media.UploadHandler and the read API of Reader.Handler; one method per route. */
export class UploadApi {
  constructor(private readonly o: ApiOptions) {}

  presign(b: PresignBody, signal?: AbortSignal) {
    return this.post<PresignReply>("/presign", b, signal);
  }
  parts(b: PartsBody, signal?: AbortSignal) {
    return this.post<PartsReply>("/parts", b, signal);
  }
  listParts(b: TicketBody, signal?: AbortSignal) {
    return this.post<PartsReply>("/parts/list", b, signal);
  }
  complete(b: TicketBody, signal?: AbortSignal) {
    return this.post<CompleteReply>("/complete", b, signal);
  }
  abort(b: TicketBody, signal?: AbortSignal) {
    return this.post<void>("/abort", b, signal);
  }
  commit(b: CommitBody, signal?: AbortSignal) {
    return this.post<CommitReply>("/commit", b, signal);
  }
  /** A JPEG still of the video upload at path, t seconds in, w pixels wide (0: the frame's). */
  frame(ref: RefBody, path: string, t: number, w?: number, signal?: AbortSignal) {
    checkRef(ref);
    const q = new URLSearchParams({ kind: ref.kind, id: ref.id, path, t: String(t) });
    if (w) q.set("w", String(w));
    return this.call<Blob>(trim(this.o.endpoint) + "/frame?" + q, undefined, signal, true);
  }

  read(ref: RefBody, o: ReadOptions = {}, signal?: AbortSignal) {
    const q = new URLSearchParams();
    if (o.prefix) q.set("prefix", o.prefix);
    if (o.offset) q.set("offset", String(o.offset));
    if (o.limit) q.set("limit", String(o.limit));
    if (o.download) q.set("download", "1");
    if (o.editor) q.set("editor", "1");
    const s = q.toString();
    return this.call<ReadResult>(this.itemURL(ref) + (s ? "?" + s : ""), undefined, signal);
  }

  /** The URL of an item under the read API: `{readEndpoint}/{kind}/{id}`. */
  itemURL(ref: RefBody): string {
    checkRef(ref);
    if (this.o.readEndpoint === undefined) throw new Error("contentkit-upload: reads need ClientOptions.readEndpoint");
    return `${trim(this.o.readEndpoint)}/${encodeURIComponent(ref.kind)}/${encodeURIComponent(ref.id)}`;
  }

  private post<T>(path: string, body: object, signal?: AbortSignal) {
    checkRef((body as { ref?: RefBody }).ref);
    return this.call<T>(trim(this.o.endpoint) + path, body, signal);
  }

  private async call<T>(url: string, body: unknown, signal?: AbortSignal, blob = false): Promise<T> {
    const f = this.o.fetch ?? fetch;
    const headers = new Headers(await this.o.headers?.());
    if (body !== undefined) headers.set("Content-Type", "application/json");
    let res: Response;
    try {
      res = await f(url, {
        method: body === undefined ? "GET" : "POST",
        headers,
        body: body === undefined ? null : JSON.stringify(body),
        credentials: this.o.credentials ?? "same-origin",
        signal: signal ?? null,
      });
    } catch (err) {
      if (signal?.aborted) throw aborted(signal);
      throw new UploadError("network", `media API ${new URL(url, "http://x").pathname} unreachable`, 0, undefined, { cause: err });
    }
    if (!res.ok) throw await fromResponse(res);
    if (blob) return (await res.blob()) as T;
    return (res.status === 204 ? undefined : await res.json()) as T;
  }
}
