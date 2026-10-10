import type { Http } from "../http.js";
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
} from "../generated/wire.js";
import { checkRef } from "./ref.js";

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

/**
 * The upload API (media.UploadHandler) and read API (Reader.Handler), one
 * method per route. Each takes the item, whose mount the call goes to.
 */
export class MediaApi {
  constructor(private readonly http: Http) {}

  presign(b: PresignBody, signal?: AbortSignal) {
    return this.post<PresignReply>(b.ref, "/presign", b, signal);
  }
  parts(ref: RefBody, b: PartsBody, signal?: AbortSignal) {
    return this.post<PartsReply>(ref, "/parts", b, signal);
  }
  listParts(ref: RefBody, b: TicketBody, signal?: AbortSignal) {
    return this.post<PartsReply>(ref, "/parts/list", b, signal);
  }
  complete(ref: RefBody, b: TicketBody, signal?: AbortSignal) {
    return this.post<CompleteReply>(ref, "/complete", b, signal);
  }
  abort(ref: RefBody, b: TicketBody, signal?: AbortSignal) {
    return this.post<void>(ref, "/abort", b, signal);
  }
  commit(b: CommitBody, signal?: AbortSignal) {
    return this.post<CommitReply>(b.ref, "/commit", b, signal);
  }
  /** A JPEG still of the video upload at path, t seconds in, w pixels wide (0: the frame's). */
  async frame(ref: RefBody, path: string, t: number, w?: number, signal?: AbortSignal) {
    checkRef(ref);
    const q = new URLSearchParams({ kind: ref.kind, id: ref.id, path, t: String(t) });
    if (w) q.set("w", String(w));
    return this.http.request<Blob>(`${this.http.mount("upload", ref)}/frame?${q}`, { signal, blob: true });
  }

  async read(ref: RefBody, o: ReadOptions = {}, signal?: AbortSignal) {
    const q = new URLSearchParams();
    if (o.prefix) q.set("prefix", o.prefix);
    if (o.offset) q.set("offset", String(o.offset));
    if (o.limit) q.set("limit", String(o.limit));
    if (o.download) q.set("download", "1");
    if (o.editor) q.set("editor", "1");
    const s = q.toString();
    return this.http.request<ReadResult>(this.itemURL(ref) + (s ? "?" + s : ""), { signal });
  }

  /** The item under the read API: `{media}/{kind}/{id}`. */
  itemURL(ref: RefBody): string {
    checkRef(ref);
    return `${this.http.mount("media", ref)}/${encodeURIComponent(ref.kind)}/${encodeURIComponent(ref.id)}`;
  }

  private async post<T>(ref: RefBody, path: string, body: object, signal?: AbortSignal) {
    checkRef(ref);
    return this.http.request<T>(this.http.mount("upload", ref) + path, { body, signal });
  }
}
