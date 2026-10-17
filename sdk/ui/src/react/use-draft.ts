import { useEffect, useRef, useState, type SetStateAction } from "react";

// A fresh record rebases clean fields; only an explicit save/discard settles edits.
// Per-field counters also protect edits made while that operation is pending.
export function useDraft<T extends object>(seed: T) {
  const [value, setValue] = useState(seed);
  const draft = useRef({ value: seed, dirty: new Set<keyof T>(), edits: new Map<keyof T, number>() });
  useEffect(() => {
    const next = { ...seed };
    for (const key of draft.current.dirty) next[key] = draft.current.value[key];
    draft.current.value = next;
    setValue(next);
  }, [seed]);
  const edit = (update: SetStateAction<T>) => {
    const d = draft.current;
    const next = typeof update === "function" ? (update as (value: T) => T)(d.value) : update;
    for (const key of new Set([...Object.keys(d.value), ...Object.keys(next)] as (keyof T)[])) {
      if (!Object.is(d.value[key], next[key])) {
        d.dirty.add(key);
        d.edits.set(key, (d.edits.get(key) ?? 0) + 1);
      }
    }
    d.value = next;
    setValue(next);
  };
  const checkpoint = () => {
    const edits = new Map(draft.current.edits);
    return (canonical: T) => {
      const d = draft.current;
      const next = { ...canonical };
      for (const key of new Set([...Object.keys(d.value), ...Object.keys(canonical)] as (keyof T)[])) {
        if (d.edits.get(key) !== edits.get(key)) next[key] = d.value[key];
        else d.dirty.delete(key);
      }
      d.value = next;
      setValue(next);
      return d.dirty.size === 0;
    };
  };
  return { value, edit, checkpoint, get: () => draft.current.value, dirty: [...draft.current.dirty] };
}
