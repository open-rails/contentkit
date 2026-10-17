import { createContext, useCallback, useContext, useLayoutEffect, useRef, useState, type RefObject } from "react";
import type { ContentKitClient } from "../../client/client.js";
import type { RefBody } from "../../client/generated/wire.js";
import { isProcessing } from "../../client/media/windows.js";

interface EditorTask {
  label: string;
  dirty: boolean;
  pending: boolean;
  valid: boolean;
  save: () => Promise<boolean>;
  order?: number;
}

interface EditorTasks {
  register: (id: string, task: RefObject<EditorTask>) => () => void;
  changed: () => void;
}

/** Optional coordination for an editor's existing independently usable tools. */
export const EditorSaveContext = createContext<EditorTasks | null>(null);

export function useEditorSaveTask(id: string, task: EditorTask) {
  const context = useContext(EditorSaveContext);
  const current = useRef(task);
  current.current = task;
  useLayoutEffect(() => context?.register(id, current), [context?.register, id]);
  useLayoutEffect(() => context?.changed(), [context?.changed, task.dirty, task.pending, task.valid]);
}

/** Save only dirty tools, before publication; a refused save never publishes. */
export function useEditorSaveCoordinator() {
  const tasks = useRef(new Map<string, RefObject<EditorTask>>());
  const [, refresh] = useState(0);
  const changed = useCallback(() => refresh((value) => value + 1), []);
  const register = useCallback(
    (id: string, task: RefObject<EditorTask>) => {
      tasks.current.set(id, task);
      changed();
      return () => {
        tasks.current.delete(id);
        changed();
      };
    },
    [changed],
  );
  const problem = () => [...tasks.current.values()].map((r) => r.current).find((t) => t.pending || (t.dirty && !t.valid))?.label;
  const save = async (current: () => boolean) => {
    const invalid = problem();
    if (invalid) return invalid;
    const ordered = [...tasks.current.values()].sort((a, b) => (a.current.order ?? 0) - (b.current.order ?? 0));
    for (const ref of ordered) {
      if (!current()) return ref.current.label;
      if (ref.current.dirty && !(await ref.current.save())) return ref.current.label;
    }
    return undefined;
  };
  return {
    context: { register, changed },
    problem,
    save,
    pending: [...tasks.current.values()].some((t) => t.current.pending),
    dirty: [...tasks.current.values()].some((t) => t.current.dirty),
  };
}

/** Check every metadata window, not only the first page of a long gallery. */
export async function readerMediaReady(client: ContentKitClient, ref: RefBody): Promise<boolean> {
  if (!ref.version) return false;
  const pages = await client.versions.pages(ref.id, ref.version);
  if (!pages.selected) return false;
  let offset = 0;
  do {
    const read = await client.media.read(ref, { editor: true, offset, limit: 200 });
    if (read.files.some((file) => !!file.failed || isProcessing(file))) return false;
    if (offset + (read.limit || 200) >= read.total) return read.total > 0;
    offset += read.limit || 200;
  } while (true);
}
