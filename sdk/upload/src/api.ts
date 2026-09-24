import { UploadError, aborted, fromResponse } from "./errors.js";
import { checkRef } from "./ref.js";
import type {
  CommitBody,
  CommitReply,
  CompleteReply,
  FilesBody,
  FilesReply,
  PartsBody,
  PartsReply,
  PresignBody,
  PresignReply,
  Edit,
  SlotBody,
  SlotEditBody,
  SlotManifest,
  SlotRefBody,
  SlotFromFileBody,
  TicketBody,
  VideoImages,
  VideoImagesBody,
  VideoPosterBody,
  VideoPreviewBody,
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
  files(b: FilesBody, signal?: AbortSignal) {
    return this.call<FilesReply>("/files", b, signal);
  }
  commitSlot(b: SlotBody, signal?: AbortSignal) {
    return this.call<SlotManifest>("/commit-slot", b, signal);
  }
  editSlot(b: SlotEditBody, signal?: AbortSignal) {
    return this.call<SlotManifest>("/edit-slot", b, signal);
  }
  slot(b: SlotRefBody, signal?: AbortSignal) {
    return this.call<SlotManifest>("/slot", b, signal);
  }
  /** The committed original's bytes (for the crop editor). */
  slotOriginal(b: SlotRefBody, signal?: AbortSignal) {
    return this.call<Blob>("/slot-original", b, signal, true);
  }
  commitSlotFromFile(b: SlotFromFileBody, signal?: AbortSignal) {
    return this.call<SlotManifest>("/commit-slot-from-file", b, signal);
  }

  videoImages(b: VideoImagesBody, signal?: AbortSignal) {
    return this.call<VideoImages>("/video-images", b, signal);
  }
  videoPoster(b: VideoPosterBody, signal?: AbortSignal) {
    return this.call<VideoImages>("/video-poster", b, signal);
  }
  videoPreview(b: VideoPreviewBody, signal?: AbortSignal) {
    return this.call<VideoImages>("/video-preview", b, signal);
  }
  /** A JPEG of one video frame (the poster picker's exact frame). */
  frame(q: { kind: string; id: string; version?: string; file?: string; t: number; w?: number }, signal?: AbortSignal) {
    checkRef(q);
    const params = new URLSearchParams({ kind: q.kind, id: q.id, t: String(q.t) });
    if (q.version) params.set("version", q.version);
    if (q.file) params.set("file", q.file);
    if (q.w) params.set("w", String(q.w));
    return this.call<Blob>("/frame?" + params.toString(), undefined, signal, true);
  }

  private async call<T>(path: string, body: unknown, signal?: AbortSignal, blob = false): Promise<T> {
    const refs = body as { ref?: { id: string }; from?: { id: string } } | undefined;
    checkRef(refs?.ref);
    checkRef(refs?.from);
    const f = this.o.fetch ?? fetch;
    const headers = new Headers(await this.o.headers?.());
    if (body !== undefined) headers.set("Content-Type", "application/json");
    let res: Response;
    try {
      res = await f(this.o.endpoint.replace(/\/$/, "") + path, {
        method: body === undefined ? "GET" : "POST",
        headers,
        body: body === undefined ? null : JSON.stringify(body),
        credentials: this.o.credentials ?? "same-origin",
        signal: signal ?? null,
      });
    } catch (err) {
      if (signal?.aborted) throw aborted(signal);
      throw new UploadError("network", `upload API ${path} unreachable`, 0, undefined, { cause: err });
    }
    if (!res.ok) throw await fromResponse(res);
    if (blob) return (await res.blob()) as T;
    return (res.status === 204 ? undefined : await res.json()) as T;
  }
}
