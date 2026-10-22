import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from "react";

interface Change {
  key: string;
  at: number;
  undo: () => void;
  redo: () => void;
}

interface DraftHistory {
  record: (change: Omit<Change, "at">) => void;
  clear: (preserveKey?: string) => void;
  undo: () => void;
  redo: () => void;
  canUndo: boolean;
  canRedo: boolean;
}

const Context = createContext<DraftHistory | null>(null);

/** One local edit history shared by a composed editor's independent drafts. */
export function DraftHistoryProvider({ children }: { children: ReactNode }) {
  const history = useRef<{ changes: Change[]; position: number }>({ changes: [], position: 0 });
  const [, refresh] = useState(0);
  const record = useCallback((change: Omit<Change, "at">) => {
    const state = history.current;
    const at = Date.now();
    state.changes.splice(state.position);
    const last = state.changes.at(-1);
    // Continuous typing in one field is one undo; unrelated edits stay separate.
    if (last?.key === change.key && at - last.at < 500) {
      last.redo = change.redo;
      last.at = at;
    } else {
      state.changes.push({ ...change, at });
      if (state.changes.length > 100) state.changes.shift();
    }
    state.position = state.changes.length;
    refresh((n) => n + 1);
  }, []);
  const clear = useCallback((preserveKey?: string) => {
    const state = history.current;
    const keep = (change: Change) => preserveKey !== undefined && change.key.startsWith(`${preserveKey}:`);
    history.current = { changes: state.changes.filter(keep), position: state.changes.slice(0, state.position).filter(keep).length };
    refresh((n) => n + 1);
  }, []);
  const undo = useCallback(() => {
    const state = history.current;
    if (state.position === 0) return;
    state.changes[--state.position]!.undo();
    refresh((n) => n + 1);
  }, []);
  const redo = useCallback(() => {
    const state = history.current;
    if (state.position === state.changes.length) return;
    state.changes[state.position++]!.redo();
    refresh((n) => n + 1);
  }, []);
  const value = { record, clear, undo, redo, canUndo: history.current.position > 0, canRedo: history.current.position < history.current.changes.length };
  return <Context.Provider value={value}>{children}</Context.Provider>;
}

export const useDraftHistory = () => useContext(Context);
