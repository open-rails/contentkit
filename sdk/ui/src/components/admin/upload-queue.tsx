import { cn } from "cn";
import { useCallback, useContext, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import type { CommitBody, FileInfo, RefBody } from "../../client/generated/wire.js";
import { samePath, type Progress, type UploadState } from "../../client/media/client.js";
import { useMessages } from "../../i18n/context.js";
import { megabytes } from "../../i18n/messages.js";
import { useFlowClient } from "../../react/admin.js";
import { ContentKitContext, useErrorReporter, useGuarded } from "../../react/context.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { ErrorLine } from "../social/parts.js";
import { MediaProcessingPanel } from "../media-processing-panel.js";
import { AdminRoot, type AdminFlowProps } from "./shell.js";

export interface UploadQueueProps extends AdminFlowProps {
  /** Makes the host's item for a file (e.g. a video record) and resolves with its ref; once per file, a reload included. */
  createItem: (file: File) => Promise<RefBody>;
  /** The upload path in each item. Default "source". */
  path?: string;
  /** Where a file uploads in its new item (path and media type), in place of path. */
  target?: (ref: RefBody, file: File, signal: AbortSignal) => Promise<{ path: string; type?: string }>;
  /** Opens a file's item: an Open button once it has one. */
  onOpen?: (ref: RefBody) => void;
  /** The file picker's accept, e.g. "video/*". */
  accept?: string;
  /** Files uploading at once. Default 2. */
  concurrency?: number;
  /** Each committed file: its item and the upload as an editor reads it. */
  onUploaded?: (ref: RefBody, file: FileInfo) => void;
  /** The localStorage key prefix for unfinished uploads. Scoped to the API mount and known provider viewer; disabled without a viewer ID. */
  storageKey?: string;
}

type Status = "queued" | "creating" | "uploading" | "committing" | "done" | "failed" | "paused";

interface Entry {
  id: string;
  file: File;
  key: string;
  status: Status;
  createItem: UploadQueueProps["createItem"];
  progress?: Progress;
  commit?: CommitBody;
  ref?: RefBody;
  error?: ContentKitError;
  /** Resumed from an upload saved before a reload. */
  resumed?: boolean;
}

interface Saved {
  ref: RefBody;
  name: string;
  size: number;
  state?: UploadState;
  /** Exact generic commit, retained until its result is confirmed. */
  commit?: CommitBody;
}

const ACTIVE: readonly Status[] = ["creating", "uploading", "committing"];
const fingerprint = (f: File) => `${f.name}\u0000${f.size}\u0000${f.lastModified}`;
let seq = 0;

function readSaved(key: string | undefined): Record<string, Saved> {
  if (!key) return {};
  try {
    return JSON.parse(localStorage.getItem(key) ?? "{}") as Record<string, Saved>;
  } catch {
    return {};
  }
}

function writeSaved(key: string | undefined, fn: (all: Record<string, Saved>) => void) {
  if (!key) return;
  try {
    const all = readSaved(key);
    fn(all);
    if (Object.keys(all).length) localStorage.setItem(key, JSON.stringify(all));
    else localStorage.removeItem(key);
  } catch {
    // Without storage, a reload starts over.
  }
}

/**
 * Many files, each into its own new item (createItem): uploads in the
 * background with progress, pause, retry and resume. An unfinished upload is
 * kept in localStorage; adding the same file after a reload continues it in
 * the same item. Leaving the page while files upload asks first.
 */
export function UploadQueue({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: UploadQueueProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="upload-queue" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <ScopedQueue {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<UploadQueueProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };

/** The queue inside an admin flow that already has its root (ContentManager's bulk upload). */
export function UploadQueueBody(p: Inner) {
  return <ScopedQueue {...p} />;
}

// Only a known account may restore uploads from persistent browser storage.
// Changing the account or API mount remounts the queue and aborts old work.
function ScopedQueue(p: Inner) {
  const viewer = useContext(ContentKitContext)?.viewer;
  const scope = JSON.stringify([p.client.url("upload"), viewer]);
  const storageKey = typeof viewer === "string" ? `${p.storageKey ?? "ckui.uploads"}:${encodeURIComponent(scope)}` : undefined;
  return <Queue {...p} key={scope} storageKey={storageKey} />;
}

function Queue({ client, createItem, path = "source", target: place, accept, concurrency = 2, onUploaded, onOpen, storageKey, onError }: Inner) {
  const m = useMessages();
  const { t } = m;
  const report = useErrorReporter(onError);
  const guarded = useGuarded();
  const [entries, setEntries] = useState<Entry[]>([]);
  const [waiting, setWaiting] = useState<Saved[]>(() => Object.values(readSaved(storageKey)));
  const [over, setOver] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const controllers = useRef(new Map<string, AbortController>());
  const latest = useRef({ onUploaded, place });
  useEffect(() => {
    latest.current = { onUploaded, place };
  });

  const patch = useCallback((id: string, next: Partial<Entry>) => setEntries((es) => es.map((e) => (e.id === id ? { ...e, ...next } : e))), []);

  const run = useCallback(
    async (entry: Entry) => {
      const ctl = new AbortController();
      controllers.current.set(entry.id, ctl);
      const saved = readSaved(storageKey)[entry.key];
      try {
        let ref = entry.ref ?? saved?.ref;
        if (!ref) {
          patch(entry.id, { status: "creating", error: undefined });
          ref = await entry.createItem(entry.file);
          const made = ref;
          writeSaved(storageKey, (all) => void (all[entry.key] = { ref: made, name: entry.file.name, size: entry.file.size }));
        }
        const target = ref;
        patch(entry.id, { ref, error: undefined });
        let batch = entry.commit ?? saved?.commit;
        if (!batch) {
          const where = latest.current.place;
          const dest = where ? await guarded(() => where(target, entry.file, ctl.signal)) : { path, type: undefined };
          patch(entry.id, { status: "uploading" });
          const remember = (state: UploadState | null) =>
            writeSaved(storageKey, (all) => void (all[entry.key] = { ref: target, name: entry.file.name, size: entry.file.size, state: state ?? undefined }));
          const options = { ref, path: dest.path, type: dest.type, signal: ctl.signal, onProgress: (progress: Progress) => patch(entry.id, { progress }), onState: remember };
          const up = await guarded(async () => {
            try {
              return await client.media.upload(entry.file, { ...options, resume: saved?.state });
            } catch (e) {
              if (saved?.state && toContentKitError(e).code === "resume_mismatch") return client.media.upload(entry.file, options);
              throw e;
            }
          });
          batch = { ref: target, operation_id: crypto.randomUUID(), ops: [{ op: "put", path: up.path, source_name: up.sourceName, blob: up.blob }] };
        }
        const commit = batch;
        patch(entry.id, { status: "committing" });
        const sources = Object.fromEntries(commit.ops.flatMap((op) => op.op === "put" && op.blob ? [[op.blob, entry.file]] : []));
        const files = await guarded(() => client.media.commit(target, commit.ops, {
          signal: ctl.signal,
          operationID: commit.operation_id,
          sources,
          onState: (commit) => {
            patch(entry.id, { commit });
            writeSaved(storageKey, (all) => void (all[entry.key] = { ref: target, name: entry.file.name, size: entry.file.size, commit }));
          },
        }));
        writeSaved(storageKey, (all) => void delete all[entry.key]);
        patch(entry.id, { status: "done", progress: undefined });
        const file = files.find((f) => commit.ops.some((op) => op.path && samePath(f.path, op.path)));
        if (file) latest.current.onUploaded?.(target, file);
      } catch (e) {
        const error = toContentKitError(e);
        if (error.code === "aborted") patch(entry.id, { status: "paused" });
        else {
          patch(entry.id, { status: "failed", error });
          report(error, "upload", entry.file.name);
        }
      } finally {
        controllers.current.delete(entry.id);
      }
    },
    [client, path, storageKey, guarded, patch, report],
  );

  // Starts queued files while fewer than concurrency run.
  useEffect(() => {
    const active = entries.filter((e) => ACTIVE.includes(e.status)).length;
    const next = entries.filter((e) => e.status === "queued").slice(0, Math.max(0, concurrency - active));
    if (!next.length) return;
    setEntries((es) => es.map((e) => (next.some((n) => n.id === e.id) ? { ...e, status: "creating" } : e)));
    for (const e of next) void run(e);
  }, [entries, concurrency, run]);

  const busy = entries.some((e) => ACTIVE.includes(e.status) || e.status === "queued");
  useEffect(() => {
    if (!busy) return;
    const ask = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    addEventListener("beforeunload", ask);
    return () => removeEventListener("beforeunload", ask);
  }, [busy]);
  useLayoutEffect(() => {
    const running = controllers.current;
    return () => {
      for (const c of running.values()) c.abort();
    };
  }, []);

  const add = (files: Iterable<File>) => {
    const saved = readSaved(storageKey);
    const added: Entry[] = [];
    for (const file of files) {
      const key = fingerprint(file);
      if (entries.some((e) => e.key === key && e.status !== "done") || added.some((e) => e.key === key)) continue;
      added.push({ id: `u${++seq}`, file, key, status: "queued", createItem, ref: saved[key]?.ref, resumed: !!saved[key] });
    }
    if (!added.length) return;
    setWaiting((w) => w.filter((s) => !added.some((a) => a.file.name === s.name && a.file.size === s.size)));
    setEntries((es) => [...es, ...added]);
  };
  const pause = (id?: string) => {
    for (const [k, c] of controllers.current) if (!id || k === id) c.abort();
    setEntries((es) => es.map((e) => (e.status === "queued" && (!id || e.id === id) ? { ...e, status: "paused" } : e)));
  };
  const resume = (id?: string) => setEntries((es) => es.map((e) => ((e.status === "paused" || e.status === "failed") && (!id || e.id === id) ? { ...e, status: "queued", error: undefined } : e)));
  const remove = (entry: Entry) => {
    controllers.current.get(entry.id)?.abort();
    if (entry.status !== "done") writeSaved(storageKey, (all) => void delete all[entry.key]);
    setEntries((es) => es.filter((e) => e.id !== entry.id));
  };
  const forget = (s: Saved) => {
    writeSaved(storageKey, (all) => {
      for (const [k, v] of Object.entries(all)) if (v.name === s.name && v.size === s.size) delete all[k];
    });
    setWaiting((w) => w.filter((x) => x !== s));
  };

  const done = entries.filter((e) => e.status === "done").length;
  const pausable = entries.some((e) => ACTIVE.includes(e.status) || e.status === "queued");
  const resumable = entries.some((e) => e.status === "paused" || e.status === "failed");
  const percent = (e: Entry) => (e.progress?.total ? Math.round((e.progress.loaded / e.progress.total) * 100) : 0);
  return (
    <>
      <div
        className={cn("grid justify-items-center gap-2 rounded-lg border-2 border-dashed border-border p-6 text-center transition-colors", over && "border-primary bg-accent")}
        data-ckui="upload-drop"
        onDragOver={(e) => {
          e.preventDefault();
          setOver(true);
        }}
        onDragLeave={() => setOver(false)}
        onDrop={(e) => {
          e.preventDefault();
          setOver(false);
          add(e.dataTransfer.files);
        }}
      >
        <p className="text-muted-foreground">{t("uploadQueue.drop")}</p>
        <Button size="sm" onClick={() => input.current?.click()}>
          {t("uploadQueue.add")}
        </Button>
        <input
          ref={input}
          type="file"
          multiple
          hidden
          accept={accept}
          aria-label={t("uploadQueue.add")}
          onChange={(e) => {
            add(e.target.files ?? []);
            e.target.value = "";
          }}
        />
      </div>
      {waiting.length > 0 && (
        <div className="grid gap-2 rounded-lg border border-border p-3" data-ckui="upload-waiting" role="status">
          <p className="font-medium">{t("uploadQueue.unfinished")}</p>
          <ul className="grid gap-1">
            {waiting.map((s) => (
              <li key={`${s.name}:${s.size}`} className="flex flex-wrap items-center gap-2">
                <span className="min-w-0 flex-1 break-all">{s.name}</span>
                <span className="text-xs text-muted-foreground">{megabytes(s.size)}</span>
                <Button size="xs" variant="ghost" onClick={() => forget(s)}>
                  {t("uploadQueue.forget")}
                </Button>
              </li>
            ))}
          </ul>
          <p className="text-xs text-muted-foreground">{t("uploadQueue.unfinishedHint")}</p>
        </div>
      )}
      {entries.length > 0 && (
        <>
          <div className="flex flex-wrap items-center gap-2">
            <p className="text-muted-foreground" aria-live="polite">
              {t("uploadQueue.summary", { done, total: entries.length })}
            </p>
            <div className="ms-auto flex gap-2">
              {pausable && (
                <Button size="sm" variant="outline" onClick={() => pause()}>
                  {t("uploadQueue.pauseAll")}
                </Button>
              )}
              {resumable && (
                <Button size="sm" variant="outline" onClick={() => resume()}>
                  {t("uploadQueue.resumeAll")}
                </Button>
              )}
            </div>
          </div>
          <ul className="grid divide-y divide-border rounded-lg border border-border">
            {entries.map((e) => (
              <li key={e.id} className="grid gap-1.5 p-3" data-ckui="upload-entry" data-status={e.status}>
                <div className="flex flex-wrap items-center gap-2">
                  <span className="min-w-0 flex-1 font-medium break-all">{e.file.name}</span>
                  <span className="text-xs text-muted-foreground">{megabytes(e.file.size)}</span>
                  {e.resumed && e.status !== "done" && <Badge variant="secondary">{t("uploadQueue.resumed")}</Badge>}
                  <Badge variant={e.status === "failed" ? "destructive" : e.status === "done" ? "outline" : "secondary"}>{t(`uploadQueue.${e.status}`)}</Badge>
                </div>
                {e.status === "uploading" && (
                  <div className="grid gap-1" data-ckui="upload-transfer" data-phase={e.progress?.phase} data-loaded={e.progress?.loaded ?? 0} data-total={e.progress?.total ?? e.file.size}>
                    <div className="h-1.5 overflow-hidden rounded-full bg-muted" role="progressbar" aria-label={e.file.name} aria-valuenow={percent(e)} aria-valuemin={0} aria-valuemax={100} aria-valuetext={e.progress?.phase === "uploading" ? `${megabytes(e.progress.loaded)} / ${megabytes(e.progress.total)}` : undefined}>
                      <div className="h-full bg-primary transition-[width]" style={{ width: `${percent(e)}%` }} />
                    </div>
                    <p className="text-xs text-muted-foreground tabular-nums" role="status">
                      {t(`mediaPanel.phase_${e.progress?.phase ?? "hashing"}`)}
                      {e.progress?.phase === "uploading" && <> · {megabytes(e.progress.loaded)} / {megabytes(e.progress.total)}</>}
                    </p>
                  </div>
                )}
                {e.error && <ErrorLine>{m.error(e.error)}</ErrorLine>}
                {e.status === "done" && e.ref && <MediaProcessingPanel item={e.ref} client={client} />}
                <div className="flex flex-wrap gap-2">
                  {(ACTIVE.includes(e.status) || e.status === "queued") && (
                    <Button size="xs" variant="ghost" onClick={() => pause(e.id)}>
                      {t("uploadQueue.pause")}
                    </Button>
                  )}
                  {(e.status === "paused" || e.status === "failed") && (
                    <Button size="xs" variant="outline" onClick={() => resume(e.id)}>
                      {t(e.status === "failed" ? "common.retry" : "uploadQueue.resume")}
                    </Button>
                  )}
                  {onOpen && e.ref && (e.status === "done" || e.status === "failed") && (
                    <Button size="xs" variant="outline" onClick={() => onOpen(e.ref!)}>
                      {t("uploadQueue.open")}
                    </Button>
                  )}
                  {e.status !== "done" && (
                    <Button size="xs" variant="ghost" onClick={() => remove(e)}>
                      {t("common.remove")}
                    </Button>
                  )}
                </div>
              </li>
            ))}
          </ul>
        </>
      )}
    </>
  );
}
