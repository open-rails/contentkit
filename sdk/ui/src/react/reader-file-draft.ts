import { useEffect, useMemo, useRef, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import { ContentKitError } from "../client/errors.js";
import type { FileInfo, Op } from "../client/generated/wire.js";
import { ruleFor } from "../client/media/rules.js";
import { planFileRenames } from "../client/media/rename-plan.js";
import { useDraft } from "./use-draft.js";
import type { UseMediaFolder } from "./folder.js";

/** Page order, removal and filenames stay local until the containing editor saves. */
export function useReaderFileDraft(source: UseMediaFolder, client: ContentKitClient) {
  const key = `${source.ref?.id ?? ""}:${source.ref?.version ?? ""}`;
  const orderKey = JSON.stringify(Object.fromEntries(source.groups.filter((g) => g.rule?.page).map((g) => [g.path, g.files.map((f) => f.path)])));
  const seed = useMemo(() => ({ key, order: JSON.parse(orderKey) as Record<string, string[]>, removed: [] as string[], names: {} as Record<string, string> }), [key, orderKey]);
  const draft = useDraft(seed);
  const [error, setError] = useState<ContentKitError>();
  const basis = useRef<{ key: string; groups: Record<string, FileInfo[]> } | null>(null);
  const awaitingRead = useRef(false);
  const [waitingForRead, setWaitingForRead] = useState(false);
  const requireFreshRead = () => {
    if (awaitingRead.current) throw new ContentKitError("conflict", "Wait for the saved gallery files to refresh before editing them again.");
  };
  const captureBasis = () => {
    if (awaitingRead.current && basis.current?.key === key) return;
    if (!draft.dirty.length || basis.current?.key !== key) basis.current = { key, groups: Object.fromEntries(source.groups.filter((g) => g.rule?.page).map((g) => [g.path, g.files])) };
  };
  useEffect(() => {
    if (!awaitingRead.current || basis.current?.key !== key) return;
    if (Object.entries(basis.current.groups).every(([path, files]) => {
      const read = source.groups.find((g) => g.path === path)?.files ?? [];
      return files.length <= read.length && files.every((file, index) => file.path === read[index]?.path && (!file.page_id || file.page_id === read[index]?.page_id));
    })) { awaitingRead.current = false; setWaitingForRead(false); }
  }, [key, source.groups, waitingForRead]);
  const priorKey = useRef(key);
  useEffect(() => {
    if (priorKey.current === key) return;
    priorKey.current = key;
    basis.current = null;
    awaitingRead.current = false;
    setWaitingForRead(false);
    setError(undefined);
    draft.checkpoint()(seed);
  }, [key, seed, draft]);
  const value = draft.value.key === key ? draft.value : seed;
  const groups = source.groups.map((g) => {
    if (!g.rule?.page) return g;
    const files = new Map(g.files.map((f) => [f.path, f]));
    const paths = (value.order[g.path] ?? []).map((path) => files.has(path) ? path : value.names[path] ? path.slice(0, path.lastIndexOf("/") + 1) + value.names[path] : path);
    return { ...g, files: [...paths.flatMap((p) => files.get(p) ? [files.get(p)!] : []), ...g.files.filter((f) => !paths.includes(f.path))].filter((f) => !value.removed.includes(f.path)) };
  });
  const folder: UseMediaFolder = {
    ...source, groups, displayNames: value.names, error: error ?? source.error,
    busy: source.busy || waitingForRead, updating: source.updating || waitingForRead,
    move: async (path, index) => {
      const group = groups.find((g) => g.rule?.page && g.files.some((f) => f.path === path));
      if (!group) return source.move(path, index);
      requireFreshRead();
      captureBasis();
      setError(undefined);
      const paths = group.files.map((f) => f.path);
      const from = paths.indexOf(path);
      paths.splice(from, 1);
      paths.splice(index, 0, path);
      draft.edit((d) => ({ ...d, order: { ...d.order, [group.path]: paths } }));
    },
    remove: async (paths) => {
      const pagePaths = paths.filter((path) => source.groups.some((g) => g.rule?.page && g.files.some((f) => f.path === path)));
      if (pagePaths.length) requireFreshRead();
      if (pagePaths.length) captureBasis();
      setError(undefined);
      if (pagePaths.length) draft.edit((d) => ({ ...d, removed: [...new Set([...d.removed, ...pagePaths])] }));
      const rest = paths.filter((path) => !pagePaths.includes(path));
      if (rest.length) await source.remove(rest);
    },
    rename: async (path, requested) => {
      if (!source.groups.some((g) => g.rule?.page && g.files.some((f) => f.path === path))) return source.rename(path, requested);
      requireFreshRead();
      captureBasis();
      let name = requested.trim();
      if (!name || /[/\\]/.test(name) || [...name].some((c) => c.charCodeAt(0) < 32) || name === "." || name === "..") {
        const refusal = new ContentKitError("invalid_request", "Enter a filename without folder separators.");
        setError(refusal);
        throw refusal;
      }
      if (!/\.[^.]+$/.test(name)) name += /\.[^.]+$/.exec(path)?.[0] ?? "";
      setError(undefined);
      draft.edit((d) => ({ ...d, names: { ...d.names, [path]: name } }));
    },
  };
  const commit = async () => {
    if (!source.ref || !draft.dirty.length) return;
    requireFreshRead();
    const state = draft.get();
    const files: FileInfo[] = [];
    let offset = 0, total: number;
    do {
      const read = await client.media.read(source.ref, { editor: true, offset, limit: 200 });
      files.push(...read.files.filter((f) => f.upload));
      total = read.total;
      offset += read.limit || 200;
    } while (offset < total);
    // The API has no caller-supplied manifest revision. Refuse a stale draft
    // before issuing mutations; appended files can safely remain appended.
    for (const g of source.groups.filter((g) => g.rule?.page)) {
      const before = basis.current?.groups[g.path] ?? [];
      const fresh = files.filter((f) => ruleFor([g.rule!], f.path));
      if (fresh.length < before.length || before.some((file, i) => file.path !== fresh[i]?.path || file.page_id && file.page_id !== fresh[i]?.page_id)) {
        throw new ContentKitError("conflict", "The gallery files changed on the server. Reload before saving these edits.");
      }
    }
    const exists = new Set(files.map((f) => f.path));
    const ops: Op[] = state.removed.filter((p) => exists.has(p)).map((path) => ({ op: "remove", path }));
    const target = (path: string) => state.names[path] ? path.slice(0, path.lastIndexOf("/") + 1) + state.names[path] : path;
    const survivors = files.filter((f) => !state.removed.includes(f.path)).map((f) => f.path);
    const renames = Object.fromEntries(Object.keys(state.names).filter((path) => !state.removed.includes(path)).map((path) => [path, target(path)]));
    ops.push(...planFileRenames(survivors, renames));
    for (const g of source.groups.filter((g) => g.rule?.page)) {
      const current = files.filter((f) => ruleFor([g.rule!], f.path) && !state.removed.includes(f.path)).map((f) => target(f.path));
      const desired = [...(state.order[g.path] ?? []).filter((p) => !state.removed.includes(p)).map(target).filter((p) => current.includes(p)), ...current.filter((p) => !(state.order[g.path] ?? []).map(target).includes(p))];
      desired.forEach((path, index) => {
        const from = current.indexOf(path);
        if (from === index) return;
        ops.push({ op: "move", path, index });
        current.splice(from, 1);
        current.splice(index, 0, path);
      });
    }
    if (ops.length) {
      const committed = await client.media.commit(source.ref, ops);
      basis.current = { key, groups: Object.fromEntries(source.groups.filter((g) => g.rule?.page).map((g) => [g.path, committed.filter((f) => ruleFor([g.rule!], f.path))])) };
      awaitingRead.current = true;
      setWaitingForRead(true);
      source.read.reload();
    }
  };
  return {
    folder, dirty: draft.dirty.length > 0, commit,
    checkpoint: () => {
      const snapshot = draft.get(), settle = draft.checkpoint();
      return () => settle(snapshot);
    },
    discard: () => { basis.current = null; awaitingRead.current = false; setWaitingForRead(false); setError(undefined); return draft.checkpoint()(seed); },
  };
}
