import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import { ContentKitError, failureError, toContentKitError } from "../client/errors.js";
import type { Edit, FileInfo, Op, ReadResult, RefBody, UploadRule } from "../client/generated/wire.js";
import { stem } from "../client/media/client.js";
import type { QueueItem } from "../client/media/queue.js";
import { filesFor, isPattern, ruleDir, ruleFor, screenFiles, uniqueName, uploadRules, type Screened } from "../client/media/rules.js";
import { useContentKitClient, useErrorReporter, type ContentKitErrorHandler } from "./context.js";
import { useMediaRead, type UseMediaRead } from "./read.js";
import { useUploadQueue, type UseUploadQueue } from "./upload.js";

export interface MediaFolderOptions {
  /**
   * The upload paths shown and added to, e.g. ["images/{name}",
   * "videos/{name}"]: one group each. Default every pattern path of the kind
   * the server does not name. A file goes to the first path whose rule takes
   * its type.
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
  /** The item; null until the host has one (files added meanwhile wait for it). */
  ref: RefBody | null;
  /** The options the folder was opened with. */
  options: MediaFolderOptions;
  /** The editor read of the item (every window, polled while processing). */
  read: UseMediaRead;
  groups: FolderGroup[];
  /** Every group's uploads. */
  uploads: FileInfo[];
  /** The upload queue; a file leaves it once the editor read lists it. */
  queue: UseUploadQueue;
  /**
   * Files added before the item and its editor read (the upload rules) were
   * known. They are screened and queued when the rules arrive, or refused
   * when the read fails or states no rules (the actor may not edit the item).
   */
  waiting: readonly File[];
  /** Screens files against the rules and queues the rest (once the rules are known); refusals land in refused. */
  add: (files: Iterable<File>) => void;
  /** The last screening's refusals. */
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
  /** Commits the uploaded files in queue order. */
  commit: () => Promise<FileInfo[]>;
  /** Stops every upload, aborts its multipart upload and empties the queue (before deleting a draft). */
  discard: () => void;
  /** An update (move, remove, rename, edit, replace) is running. */
  updating: boolean;
  committing: boolean;
  /**
   * Work is in flight: files wait for the rules, uploads are queued or
   * running, an automatic commit is due, or a commit or update runs.
   * Leaving the page now interrupts it.
   */
  busy: boolean;
  /** The item's uploads plus the files on their way in (waiting, queued, uploading, uploaded, being added); failed uploads are not counted. */
  fileCount: number;
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

const NO_ITEM: RefBody = { kind: "", id: "" };
const keyOf = (ref: RefBody | null) => (ref ? `${ref.kind}/${ref.id}` : "");

const noRules = new WeakMap<ReadResult, ContentKitError>();
/** The refusal for files added to an item whose read carries no upload rules (one per read). */
function notEditor(read: ReadResult): ContentKitError {
  let e = noRules.get(read);
  if (!e) noRules.set(read, (e = new ContentKitError("forbidden", "the editor read states no upload rules", { status: 403 })));
  return e;
}

/** Queue items still on their way in: not failed, and not yet listed by the read. */
function inbound(items: readonly QueueItem[], listed: ReadonlySet<string>) {
  return items.filter((i) => i.status !== "failed" && !(i.status === "committed" && listed.has(i.path)));
}

/**
 * An item's folder for its editors: the editor read, an upload queue, the
 * kind's upload rules screening files before they upload, and the updates
 * (move, remove, rename, edit, replace). Files added before the editor read
 * arrives wait for its rules; with a null ref they wait for the item too (a
 * draft created on the first drop). Uploads the worker fails after the read
 * are reported once ("folder.process"). Another ref gets a fresh queue.
 */
export function useMediaFolder(ref: RefBody | null | undefined, o: MediaFolderOptions = {}): UseMediaFolder {
  const client = useContentKitClient(o.client);
  const report = useErrorReporter(o.onError);
  const item = ref ?? null;
  const key = keyOf(item);
  const target = item ?? NO_ITEM;
  // Without an item there is nothing to read: a supplied null read never fetches.
  const read = useMediaRead(target, { editor: true, window: ALL, poll: o.poll, client, read: item ? undefined : null });
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
  const listed = useMemo(() => new Set(uploads.map((f) => f.path)), [uploads]);

  const raw = useUploadQueue({ client, ref: target, path: `${ruleDir(groupPaths[0] ?? "{name}")}{name}`, concurrency: o.concurrency });
  // The UploadQueue itself is stable per item; the hook's wrapper is new every render.
  const q = raw.queue;
  // A committed file stays in the queue until the read lists it, so it never drops out of view.
  const items = useMemo(() => raw.items.filter((i) => !(i.status === "committed" && listed.has(i.path))), [raw.items, listed]);
  const queue = { ...raw, items };
  const [refused, setRefused] = useState<Screened["refused"]>([]);
  const [waiting, setWaiting] = useState<readonly File[]>([]);
  const pending = useRef<File[]>([]);
  const [updating, setUpdating] = useState(0);
  const [committing, setCommitting] = useState(false);
  const [error, setError] = useState<ContentKitError>();
  // An automatic commit that failed: no more until commit() or new files.
  const [stall, setStall] = useState<{ key: string; error: ContentKitError }>();
  const stalled = stall?.key === key ? stall.error : undefined;
  const opts = useRef(o);
  opts.current = o;
  // The rules are known once the item's editor read states them. A failed read
  // refuses what waits, and so does one without rules: the server answered a
  // viewer's read, so nothing uploads here.
  const ready = !!item && !!read.read?.uploads;
  const loadError = !item ? undefined : !read.read ? read.error : !read.read.uploads ? notEditor(read.read) : undefined;
  const live = useRef({ groups, ref: target, listed, ready, loadError });
  live.current = { groups, ref: target, listed, ready, loadError };

  const place = useCallback(
    (files: File[]) => {
      const { groups, listed } = live.current;
      const gs = groups.map((g) => g.rule ?? { path: g.path, types: [], max_bytes: 0 });
      const queued = inbound(q.getSnapshot().items, listed);
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
        q.add(paths.keys(), { path: (f) => paths.get(f)! });
        // New files lift a block and restart a queue discard() paused.
        q.start();
        setStall(undefined);
      }
      setRefused(s.refused);
      for (const r of s.refused) report(r.error, "upload", r.file.name);
    },
    [q, report],
  );

  // Screens what waits once the rules are known; a failed read refuses it (the read failure is reported once, as folder.load).
  const flush = useCallback(() => {
    const files = pending.current;
    const { ready, loadError } = live.current;
    if (!files.length || (!ready && !loadError)) return false;
    pending.current = [];
    setWaiting([]);
    if (ready) place(files);
    else setRefused(files.map((file) => ({ file, error: loadError! })));
    return true;
  }, [place]);

  const add = useCallback(
    (files: Iterable<File>) => {
      const list = [...files];
      if (!list.length) return;
      pending.current = [...pending.current, ...list];
      if (!flush()) setWaiting(pending.current);
    },
    [flush],
  );
  useEffect(() => void flush(), [flush, ready, loadError]);

  const update = useCallback(
    async (ops: Op[], run?: () => Promise<unknown>) => {
      setUpdating((n) => n + 1);
      try {
        if (run) await run();
        else await client.media.commit(live.current.ref, ops);
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
      const files = await q.commit(undefined, { head: opts.current.commit === "auto" });
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
  }, [q, report]);

  // Committed files leave the queue once the read lists them, or once a read lands without them.
  useEffect(() => {
    for (const i of raw.items) if (i.status === "committed" && (listed.has(i.path) || !read.loading)) q.remove(i.id);
  }, [raw.items, listed, read.loading, q]);

  // A draft commits the uploaded head of the queue as it finishes; a failed commit waits for commit().
  const auto = o.commit === "auto";
  const headReady = items.find((i) => i.status !== "committed")?.status === "uploaded";
  useEffect(() => {
    if (!auto || !headReady || committing || stalled) return;
    commit().catch((e: unknown) => setStall({ key, error: toContentKitError(e) }));
  }, [auto, headReady, committing, stalled, commit, key]);
  const manualCommit = useCallback(async () => {
    setStall(undefined);
    return commit();
  }, [commit]);

  // Each failed upload and queue-wide refusal is reported once; rows keep showing it.
  const seen = useRef(new Set<unknown>());
  useEffect(() => {
    for (const i of raw.items) {
      if (i.status !== "failed" || !i.error || seen.current.has(i.error)) continue;
      seen.current.add(i.error);
      report(i.error, "upload", i.file.name);
    }
    if (raw.blocked && !seen.current.has(raw.blocked)) {
      seen.current.add(raw.blocked);
      report(raw.blocked, "upload");
    }
  }, [raw.items, raw.blocked, report]);

  // Processing failures that appear after the item's first read.
  const failures = useRef<{ key: string; seen: Set<string> } | null>(null);
  useEffect(() => {
    if (!read.read) return;
    const failed = uploads.filter((f) => f.failed);
    if (failures.current?.key !== key) {
      failures.current = { key, seen: new Set(failed.map((f) => `${f.path}:${f.failed!.of}`)) };
      return;
    }
    for (const f of failed) {
      const id = `${f.path}:${f.failed!.of}`;
      if (failures.current.seen.has(id)) continue;
      failures.current.seen.add(id);
      report(failureError(f.failed!), "folder.process", f.path.split("/").pop());
    }
  }, [read.read, uploads, report, key]);

  useEffect(() => void (read.error && report(read.error, "folder.load")), [read.error, report]);

  const busy =
    waiting.length > 0 ||
    committing ||
    updating > 0 ||
    (auto && headReady && !stalled) ||
    items.some((i) => i.status === "uploading" || (i.status === "queued" && !raw.blocked));
  const fileCount = uploads.length + waiting.length + inbound(items, listed).length;

  // Reads the latest groups, so callbacks stay stable.
  const groupOf = (path: string) => live.current.groups.find((g) => ruleFor([{ path: g.path }], path));
  return {
    ref: item,
    options: o,
    read,
    groups,
    uploads,
    queue,
    waiting,
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
      (path, file) => update([], () => client.media.put(file, { ref: live.current.ref, path: stem(path), wait: false })),
      [update, client],
    ),
    commit: manualCommit,
    discard: useCallback(() => {
      pending.current = [];
      setWaiting([]);
      for (const i of q.getSnapshot().items) q.remove(i.id);
      q.pause();
    }, [q]),
    updating: updating > 0,
    committing,
    busy,
    fileCount,
    error: error ?? stalled,
  };
}
