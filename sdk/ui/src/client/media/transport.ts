import { ContentKitError, aborted } from "../errors.js";
import type { RequestReply } from "../generated/wire.js";

/**
 * Sends a presigned request with its body. Resolves on 2xx; rejects with an
 * ContentKitError (storage, network or aborted). onProgress reports bytes sent.
 */
export type Transport = (
  req: RequestReply,
  body: Blob,
  opts: { signal?: AbortSignal; onProgress?: (sent: number) => void },
) => Promise<void>;

/** XMLHttpRequest when available (upload progress), otherwise fetch. */
export const defaultTransport: Transport = (req, body, opts) =>
  typeof XMLHttpRequest === "undefined" ? fetchTransport(req, body, opts) : xhrTransport(req, body, opts);

export const xhrTransport: Transport = (req, body, { signal, onProgress }) =>
  new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(aborted(signal));
    const xhr = new XMLHttpRequest();
    const onAbort = () => xhr.abort();
    const done = (err?: ContentKitError) => {
      signal?.removeEventListener("abort", onAbort);
      if (err) reject(err);
      else resolve();
    };
    xhr.open(req.method, req.url);
    for (const [k, v] of Object.entries(req.headers)) xhr.setRequestHeader(k, v);
    xhr.upload.onprogress = (e) => onProgress?.(e.loaded);
    xhr.onload = () =>
      done(xhr.status >= 200 && xhr.status < 300 ? undefined : storageError(xhr.status, xhr.responseText));
    xhr.onerror = () => done(new ContentKitError("network", "connection to storage failed"));
    xhr.ontimeout = xhr.onerror;
    xhr.onabort = () => done(aborted(signal));
    signal?.addEventListener("abort", onAbort, { once: true });
    xhr.send(body);
  });

export const fetchTransport: Transport = async (req, body, { signal, onProgress }) => {
  let res: Response;
  try {
    res = await fetch(req.url, { method: req.method, headers: req.headers, body, signal: signal ?? null });
  } catch (err) {
    if (signal?.aborted) throw aborted(signal);
    throw new ContentKitError("network", "connection to storage failed", { cause: err });
  }
  if (!res.ok) throw storageError(res.status, await res.text().catch(() => ""));
  await res.body?.cancel();
  onProgress?.(body.size);
};

function storageError(status: number, body: string): ContentKitError {
  const code = /<Code>([^<]+)<\/Code>/.exec(body)?.[1];
  return new ContentKitError("storage", `storage refused the upload (${status}${code ? " " + code : ""})`, { status });
}
