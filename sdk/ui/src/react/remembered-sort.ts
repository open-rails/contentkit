import { useCallback, useEffect, useState } from "react";

const PREFIX = "ckui.sort.";

function stored(key: string): string | undefined {
  try {
    return globalThis.localStorage?.getItem(PREFIX + key) ?? undefined;
  } catch {
    return undefined;
  }
}

/**
 * A sort remembered per page type in this browser. A sort in the URL wins and
 * is remembered; otherwise the last choice, else the default.
 */
export function useRememberedSort<S extends string>(key: string, fromURL: S | undefined | null, fallback: S, allowed?: readonly S[]): [S, (sort: S) => void] {
  const [remembered, setRemembered] = useState(() => stored(key));
  const ok = (s: string | undefined | null): s is S => !!s && (!allowed || allowed.includes(s as S));
  const sort = ok(fromURL) ? fromURL : ok(remembered) ? remembered : fallback;
  const remember = useCallback(
    (next: S) => {
      try {
        globalThis.localStorage?.setItem(PREFIX + key, next);
      } catch {
        // Private mode: the URL still carries it.
      }
      setRemembered(next);
    },
    [key],
  );
  const linked = ok(fromURL) ? fromURL : undefined;
  useEffect(() => {
    if (linked !== undefined && linked !== stored(key)) remember(linked);
  }, [key, linked, remember]);
  return [sort, remember];
}
