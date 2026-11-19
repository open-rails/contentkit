import { readProgress, type MediaProgress } from "./progress.js";
import type { Http } from "../http.js";
import type {
  CommitBody,
  CommitReply,
  CompleteReply,
  ContentModel,
  PartsBody,
  PartsReply,
  PresetRule,
  PresignBody,
  PresignReply,
  ReadResult,
  RefBody,
  TicketBody,
} from "../generated/wire.js";
import { aborted, throwIfAborted } from "../errors.js";
import { call } from "../route.js";
import { checkRef, isAccountId, isContentId } from "./ref.js";

// The kinds keyed by account per client transport: the content model's `avatars`.
const accountKinds = new WeakMap<Http, ReadonlySet<string>>();
const modelReads = new WeakMap<Http, Promise<void>>();

/** Records the account kinds of a content model the client read. */
export function learnAccountKinds(http: Http, model: ContentModel) {
  accountKinds.set(http, new Set(model.avatars ? [model.avatars.kind] : []));
}

/** What a read returns (Reader.Read's ReadOptions). */
export interface ReadOptions {
  /** Editor-only processing summary, without files or signed media URLs. */
  status?: boolean;
  /** Only files under this path prefix, e.g. "low-res/". */
  prefix?: string;
  /** Sign these file paths (at most 64), without granting access to other files. Do not combine with offset/limit. */
  paths?: readonly string[];
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

  async read(ref: RefBody, o: ReadOptions = {}, signal?: AbortSignal) {
    const q = new URLSearchParams();
    if (o.status) q.set("status", "1");
    if (o.prefix) q.set("prefix", o.prefix);
    for (const path of o.paths ?? []) q.append("path", path);
    if (o.offset) q.set("offset", String(o.offset));
    if (o.limit) q.set("limit", String(o.limit));
    if (o.download) q.set("download", "1");
    if (o.editor) q.set("editor", "1");
    const s = q.toString();
    await this.check(ref, signal);
    return this.http.request<ReadResult>(this.itemURL(ref) + (s ? "?" + s : ""), { signal });
  }

  /** Observe canonical status using authenticated fetch, including reconnect cursors. */
  async watch(ref: RefBody, receive: (update: MediaProgress) => void | Promise<void>, signal: AbortSignal, after = "") {
    const query = new URLSearchParams({ status: "1" });
    if (after) query.set("after", after);
    await this.check(ref, signal);
    const response = await this.http.request<Response>(`${this.itemURL(ref)}?${query}`, { signal, stream: true, guestPreferences: false });
    await readProgress(response, receive, signal);
  }

  /** Every kind's public presets (the same for every viewer). */
  presets(signal?: AbortSignal) {
    return this.http.request<PresetRule[]>(`${this.http.mount("media")}/presets`, { signal });
  }

  /**
   * The item under the read API: `{media}/{kind}/{id}`, a version's at `…/versions/{version}`.
   * An account ref whose id is not a UUIDv7 passes once the client has read the content model.
   */
  itemURL(ref: RefBody): string {
    checkRef(ref, accountKinds.get(this.http));
    const item = `${this.http.mount("media", ref)}/${encodeURIComponent(ref.kind)}/${encodeURIComponent(ref.id)}`;
    return ref.version ? `${item}/versions/${encodeURIComponent(ref.version)}` : item;
  }

  private async post<T>(ref: RefBody, path: string, body: object, signal?: AbortSignal) {
    await this.check(ref, signal);
    return this.http.request<T>(this.http.mount("upload", ref) + path, { body, signal });
  }

  // An id only an account kind takes needs the content model: a client that
  // has not read it reads /kinds once, before any media request.
  private async check(ref: RefBody, signal?: AbortSignal) {
    if (!isContentId(ref.id) && isAccountId(ref.id) && !accountKinds.has(this.http)) {
      let read = modelReads.get(this.http);
      if (!read) {
        read = call(this.http, "GET", "/kinds").then(
          (model: ContentModel) => learnAccountKinds(this.http, model),
          (e: unknown) => {
            modelReads.delete(this.http);
            throw e;
          },
        );
        modelReads.set(this.http, read);
      }
      await until(read, signal);
    }
    checkRef(ref, accountKinds.get(this.http));
  }
}

// p, unless the caller cancels first; other callers keep waiting for p.
function until<T>(p: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (!signal) return p;
  throwIfAborted(signal);
  return new Promise<T>((resolve, reject) => {
    const stop = () => reject(aborted(signal));
    signal.addEventListener("abort", stop, { once: true });
    p.then(resolve, reject).finally(() => signal.removeEventListener("abort", stop));
  });
}
