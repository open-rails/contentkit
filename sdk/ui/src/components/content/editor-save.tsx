import { createContext, useCallback, useContext, useLayoutEffect, useRef, useState, type RefObject } from "react";

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
  const register = context?.register;
  const changed = context?.changed;
  const current = useRef(task);
  current.current = task;
  useLayoutEffect(() => register?.(id, current), [register, id]);
  useLayoutEffect(() => changed?.(), [changed, task.dirty, task.pending, task.valid]);
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
