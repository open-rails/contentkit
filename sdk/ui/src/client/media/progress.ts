import { ContentKitError, aborted, readContentKitError } from "../errors.js";
import type { ReadResult } from "../generated/wire.js";

export interface MediaProgress {
  event: "snapshot" | "progress" | "resync" | "heartbeat";
  cursor: string;
  read?: ReadResult;
}

/** Bounded SSE parser. Reader and callbacks are sequential to apply backpressure. */
export async function readProgress(response: Response, receive: (update: MediaProgress) => void | Promise<void>, signal: AbortSignal): Promise<void> {
  if (!response.headers.get("content-type")?.startsWith("text/event-stream") || !response.body) {
    await response.body?.cancel();
    throw new ContentKitError("invalid_request", "Expected a media progress stream.");
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let pending = "", data = "", event = "", cursor = "";
  const cancel = () => { void reader.cancel(); };
  let heartbeat: ReturnType<typeof setTimeout> | undefined;
  signal.addEventListener("abort", cancel, { once: true });
  try {
    for (;;) {
      if (signal.aborted) throw aborted(signal);
      heartbeat = setTimeout(cancel, 30_000);
      const chunk = await reader.read();
      clearTimeout(heartbeat);
      if (signal.aborted) throw aborted(signal);
      if (chunk.done) throw new ContentKitError("network", "Media progress disconnected.");
      pending += decoder.decode(chunk.value, { stream: true });
      let index: number;
      while ((index = pending.indexOf("\n")) !== -1) {
        const line = pending.slice(0, index).replace(/\r$/, "");
        pending = pending.slice(index + 1);
        if (!line) {
          if (data) {
            if (event === "error") {
              const body = JSON.parse(data) as { code?: string };
              const status = body.code === "forbidden" ? 403 : body.code === "unauthorized" ? 401 : body.code === "not_found" ? 404 : 503;
              throw await readContentKitError(Response.json(body, { status }));
            }
            if (event === "snapshot" || event === "progress" || event === "resync" || event === "heartbeat") {
              await receive({ event, cursor, ...(event === "heartbeat" ? {} : { read: JSON.parse(data) as ReadResult }) });
              if (signal.aborted) throw aborted(signal);
            }
          }
          data = ""; event = "";
          continue;
        }
        const separator = line.indexOf(":");
        const key = separator === -1 ? line : line.slice(0, separator);
        const value = separator === -1 ? "" : line.slice(separator + 1).replace(/^ /, "");
        if (key === "event") event = value;
        if (key === "id" && !value.includes("\0")) cursor = value;
        if (key === "data") data += (data ? "\n" : "") + value;
        if (data.length + pending.length > 1024 * 1024) throw new ContentKitError("invalid_request", "Media progress event exceeds its limit.");
      }
      if (pending.length > 1024 * 1024) throw new ContentKitError("invalid_request", "Media progress line exceeds its limit.");
    }
  } finally {
    clearTimeout(heartbeat);
    signal.removeEventListener("abort", cancel);
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}
