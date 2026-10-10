import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import { ContentKitError, failureError, toContentKitError } from "../client/errors.js";
import type { Edit, FileInfo, Op, ReadResult, RefBody } from "../client/generated/wire.js";
import { stem } from "../client/media/client.js";
import { filesFor, isPattern, ruleDir, ruleFor, screenFiles, uniqueName, uploadRules, type Screened, type UploadRule } from "../client/media/rules.js";
import { useContentKitClient, useErrorReporter, type ContentKitErrorHandler } from "./context.js";
import { useMediaRead, type UseMediaRead } from "./read.js";
import { useUploadQueue, type UseUploadQueue } from "./upload.js";

export interface MediaFolderOptions {
  /**
   * The upload paths shown and added to, e.g. ["images/{name}",
   * "videos/{name}"]: one group each. Default every pattern path of the kind
   * the server does not name. A file goes to the first path whose rule takes
   * its type; before the server states rules, to the first path.
   */
  paths?: readonly string[];
  /**
   * "manual" (default): commit() adds the uploaded files. "auto": each
   * upload commits once those before it have, in the order added (a draft).
   */
  commit?: "manual" | "auto";
  /** A file's name under its path; default its own, numbered around names taken. */
  name?: (file: File) => string;
  /** Files uploading at once. Default 2. */
  concurrency?: number;
  /** Poll interval (ms) while uploads process. Default 2500. */
  poll?: number | false;
  /** Every failure (uploads, commits, updates, processing); default the provider's. */
  onError?: ContentKitErrorHandler;
  /** Overrides the provider's client. */
  client?: ContentKitClient;
}

export interface FolderGroup {
  /** The upload path, e.g. "images/{name}". */
  path: string;
  /** Its rules, once an editor read states them. */
  rule?: UploadRule;
  /** Its uploads in manifest order. */
  files: FileInfo[];
}

export interface UseMediaFolder {
  /** The editor read of the item (every window, polled while processing). */
  read: UseMediaRead;
  groups: FolderGroup[];
  /** Every group's uploads. */
  uploads: FileInfo[];
  queue: UseUploadQueue;
  /** Screens files against the rules and queues the rest; returns the refusals. */
  add: (files: Iterable<File>) => Screened["refused"];
  /** The last add()'s refusals. */
  refused: Screened["refused"];
  dismiss: () => void;
  /** Moves an upload to index among its group's uploads. */
  move: (path: string, index: number) => Promise<void>;
  remove: (paths: readonly string[]) => Promise<void>;
  /** Renames an upload within its path; the extension stays unless name has one. */
  rename: (path: string, name: string) => Promise<void>;
  edit: (path: string, edit: Edit | null) => Promise<void>;
  /** Uploads file in place of the upload at path (same position and meta, no edit). */
  replace: (path: string, file: File) => Promise<void>;
  /** Commits the uploaded files in queue order and clears them from the queue. */
  commit: () => Promise<FileInfo[]>;
  /** Stops every upload, aborts its multipart upload and empties the queue (before deleting a draft). */
  discard: () => void;
  /** An update (move, remove, rename, edit, replace) is running. */
  updating: boolean;
  committing: boolean;
  /** The last update's or commit's failure; cleared by the next success. */
  error?: ContentKitError;
}

const ALL = { start: 0, end: Number.MAX_SAFE_INTEGER, chunk: 200 };

/** The upload's name under its group ("images/a.png" in "images/{name}" is "a.png"). */
export const nameIn = (group: { path: string }, path: string) => path.slice(ruleDir(group.path).length);

/** Its smallest derived image with a URL: a thumbnail for an editor. */
export function thumbnailOf(read: ReadResult | null | undefined, upload: FileInfo): FileInfo | undefined {
  let best: FileInfo | undefined;
  for (const f of read?.files ?? []) {
    if (f.upload || f.from !== upload.path || !f.url || !f.type.startsWith("image/")) continue;
    if (!best || (f.w ?? Infinity) < (best.w ?? Infinity)) best = f;
  }
  return best;
}

/**
 * An item's folder for its editors: the editor read, an upload queue, the
 * kind's upload rules screening files before they upload, and the updates
 * (move, remove, rename, edit, replace). Uploads the worker fails after this
 * mounted are reported once ("folder.process"). The queue is the first
 * item's: key the component by ref to switch items.
 */
export function useMediaFolder(ref: RefBody, o: MediaFolderOptions = {}): UseMediaFolder {
  const client = useContentKitClient(o.client);
  const report = useErrorReporter(o.onError);
  const read = useMediaRead(ref, { editor: true, window: ALL, poll: o.poll, client });
  const rules = uploadRules(read.read);
  const pathsKey = o.paths?.join("\n");
  const groupPaths = useMemo(
    () => (pathsKey !== undefined ? pathsKey.split("\n").filter(Boolean) : rules.filter((r) => isPattern(r.path) && !r.named).map((r) => r.path)),
    [pathsKey, rules],
  );
  const groups = useMemo((): FolderGroup[] => {
    const known: { path: string }[] = rules.length ? rules : groupPaths.map((path) => ({ path }));
    return groupPaths.map((path) => ({ path, rule: rules.find((r) => r.path === path), files: filesFor(read.read, { path }, known) }));
  }, [groupPaths, rules, read.read]);
  const uploads = useMemo(() => groups.flatMap((g) => g.files), [groups]);

  const queue = useUploadQueue({ client, ref, path: `${ruleDir(groupPaths[0] ?? "{name}")}{name}`, concurrency: o.concurrency });
  const [refused, setRefused] = useState<Screened["refused"]>([]);
  const [updating, setUpdating] = useState(0);
  const [committing, setCommitting] = useState(false);
  const [error, setError] = useState<ContentKitError>();
  // An automatic commit that failed: no more until commit() or new files.
  const [stalled, setStalled] = useState<ContentKitError | undefined>();
  const opts = useRef(o);
  opts.current = o;
  const state = useRef({ groups, ref });
  state.current = { groups, ref };

  const add = useCallback(
    (files: Iterable<File>) => {
      const { groups } = state.current;
      const gs = groups.map((g) => g.rule ?? { path: g.path, types: [], max_bytes: 0 });
      const queued = queue.queue.getSnapshot().items.filter((i) => i.status !== "failed");
      const inGroup = (path: string, rule: UploadRule) => ruleFor(gs, path)?.path === rule.path;
      const s = screenFiles(files, gs, (rule) => groups.find((g) => g.path === rule.path)!.files.length + queued.filter((i) => inGroup(i.path, rule)).length);
      const taken = new Map<string, string[]>();
      const takenIn = (rule: UploadRule) => {
        let t = taken.get(rule.path);
        if (!t) {
          const g = groups.find((x) => x.path === rule.path)!;
          t = [...g.files.map((f) => nameIn(g, f.path)), ...queued.filter((i) => inGroup(i.path, rule)).map((i) => nameIn(g, i.path))];
          taken.set(rule.path, t);
        }
        return t;
      };
      const paths = new Map<File, string>();
      for (const { file, rule } of s.accepted) {
        const t = takenIn(rule);
        const name = opts.current.name?.(file) ?? uniqueName(file.name, t);
        t.push(name);
        paths.set(file, ruleDir(rule.path) + name);
      }
      if (paths.size) {
        queue.add(paths.keys(), { path: (f) => paths.get(f)! });
        if (queue.queue.getSnapshot().blocked) queue.start();
        setStalled(undefined);
      }
      setRefused(s.refused);
      for (const r of s.refused) report(r.error, "upload", r.file.name);
      return s.refused;
    },
    [queue, report],
  );

  const update = useCallback(
    async (ops: Op[], run?: () => Promise<unknown>) => {
      setUpdating((n) => n + 1);
      try {
        if (run) await run();
        else await client.media.commit(state.current.ref, ops);
        setError(undefined);
      } catch (e) {
        const err = toContentKitError(e);
        if (err.code !== "aborted") setError(err);
        report(err, "folder.update");
        throw err;
      } finally {
        setUpdating((n) => n - 1);
      }
    },
    [client, report],
  );

  const commit = useCallback(async () => {
    setCommitting(true);
    try {
      const files = await queue.commit(undefined, { head: opts.current.commit === "auto" });
      for (const i of queue.queue.getSnapshot().items) if (i.status === "committed") queue.remove(i.id);
      setError(undefined);
      return files;
    } catch (e) {
      const err = toContentKitError(e);
      setError(err);
      report(err, "folder.commit");
      throw err;
    } finally {
      setCommitting(false);
    }
  }, [queue, report]);

  // A draft commits the uploaded head of the queue as it finishes; a failed commit waits for commit().
  const auto = o.commit === "auto";
  const head = queue.items[0];
  const headReady = head?.status === "uploaded";
  useEffect(() => {
    if (!auto || !headReady || committing || stalled) return;
    commit().catch((e: unknown) => setStalled(toContentKitError(e)));
  }, [auto, headReady, committing, stalled, commit]);
  const manualCommit = useCallback(async () => {
    setStalled(undefined);
    return commit();
  }, [commit]);

  // Each failed upload and queue-wide refusal is reported once; rows keep showing it.
  const seen = useRef(new Set<unknown>());
  useEffect(() => {
    for (const i of queue.items) {
      if (i.status !== "failed" || !i.error || seen.current.has(i.error)) continue;
      seen.current.add(i.error);
      report(i.error, "upload", i.file.name);
    }
    if (queue.blocked && !seen.current.has(queue.blocked)) {
      seen.current.add(queue.blocked);
      report(queue.blocked, "upload");
    }
  }, [queue.items, queue.blocked, report]);

  // Processing failures that appear after the first read.
  const failures = useRef<Set<string> | null>(null);
  useEffect(() => {
    if (!read.read) return;
    const failed = uploads.filter((f) => f.failed);
    if (!failures.current) {
      failures.current = new Set(failed.map((f) => `${f.path}:${f.failed!.of}`));
      return;
    }
    for (const f of failed) {
      const key = `${f.path}:${f.failed!.of}`;
      if (failures.current.has(key)) continue;
      failures.current.add(key);
      report(failureError(f.failed!), "folder.process", f.path.split("/").pop());
    }
  }, [read.read, uploads, report]);

  useEffect(() => void (read.error && report(read.error, "folder.load")), [read.error, report]);

  // Reads the latest groups, so callbacks stay stable.
  const groupOf = (path: string) => state.current.groups.find((g) => ruleFor([{ path: g.path }], path));
  return {
    read,
    groups,
    uploads,
    queue,
    add,
    refused,
    dismiss: useCallback(() => setRefused([]), []),
    move: useCallback((path, index) => update([{ op: "move", path, index }]), [update]),
    remove: useCallback((paths) => update(paths.map((path): Op => ({ op: "remove", path }))), [update]),
    rename: useCallback(
      (path, name) => {
        const g = groupOf(path);
        const to = (g ? ruleDir(g.path) : path.slice(0, path.lastIndexOf("/") + 1)) + name.trim();
        if (stem(to) === stem(path)) return Promise.resolve();
        if (g?.files.some((f) => stem(f.path) === stem(to))) {
          const err = new ContentKitError("conflict", `${name} exists`, { status: 409 });
          setError(err);
          return Promise.reject(err);
        }
        return update([{ op: "rename", path, to }]);
      },
      [update],
    ),
    edit: useCallback((path, edit) => update([{ op: "edit", path, ...(edit ? { edit } : {}) }]), [update]),
    replace: useCallback(
      (path, file) => update([], () => client.media.put(file, { ref: state.current.ref, path: stem(path), wait: false })),
      [update, client],
    ),
    commit: manualCommit,
    discard: useCallback(() => {
      for (const i of queue.queue.getSnapshot().items) queue.remove(i.id);
      queue.pause();
    }, [queue]),
    updating: updating > 0,
    committing,
    error: error ?? stalled,
  };
}
