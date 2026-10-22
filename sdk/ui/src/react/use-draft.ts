import { useEffect, useId, useRef, useState, type SetStateAction } from "react";
import { useDraftHistory } from "./draft-history.js";

const equal = (a: unknown, b: unknown) => Object.is(a, b) || JSON.stringify(a) === JSON.stringify(b);

// A fresh record rebases clean fields; only an explicit save/discard settles edits.
// Per-field counters also protect edits made while that operation is pending.
export function useDraft<T extends object>(seed: T) {
  const id = useId();
  const history = useDraftHistory();
  const baseline = useRef(seed);
  const [value, setValue] = useState(seed);
  const draft = useRef({ value: seed, dirty: new Set<keyof T>(), edits: new Map<keyof T, number>() });
  useEffect(() => {
    baseline.current = seed;
    const next = { ...seed };
    for (const key of draft.current.dirty) next[key] = draft.current.value[key];
    draft.current.value = next;
    setValue(next);
  }, [seed]);
  const apply = (next: T) => {
    const d = draft.current;
    for (const key of new Set([...Object.keys(d.value), ...Object.keys(next)] as (keyof T)[])) {
      if (!Object.is(d.value[key], next[key])) {
        if (equal(baseline.current[key], next[key])) d.dirty.delete(key);
        else d.dirty.add(key);
        d.edits.set(key, (d.edits.get(key) ?? 0) + 1);
      }
    }
    d.value = next;
    setValue(next);
  };
  const edit = (update: SetStateAction<T>) => {
    const before = draft.current.value;
    const next = typeof update === "function" ? (update as (value: T) => T)(before) : update;
    const keys = [...new Set([...Object.keys(before), ...Object.keys(next)])].filter((key) => !equal(before[key as keyof T], next[key as keyof T]));
    if (!keys.length) return;
    const restore = (saved: T) => {
      const restored = { ...draft.current.value };
      for (const name of keys) {
        const key = name as keyof T;
        if (key in saved) restored[key] = saved[key];
        else Reflect.deleteProperty(restored, key);
      }
      apply(restored);
    };
    history?.record({ key: `${id}:${keys.join(",")}`, undo: () => restore(before), redo: () => restore(next) });
    apply(next);
  };
  const checkpoint = () => {
    const edits = new Map(draft.current.edits);
    return (canonical: T) => {
      baseline.current = canonical;
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
  return { value, edit, checkpoint, historyKey: id, get: () => draft.current.value, dirty: [...draft.current.dirty] };
}
