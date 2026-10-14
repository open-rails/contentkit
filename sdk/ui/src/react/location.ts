import { useCallback, useContext, useSyncExternalStore } from "react";
import { versionParam, withVersion } from "../urls/index.js";
import { ContentKitContext } from "./context.js";

/** Dispatched after a history write, so location readers look again. */
export const MOVED = "ckui:navigate";

export const subscribeHistory = (cb: () => void) => {
  addEventListener("popstate", cb);
  addEventListener(MOVED, cb);
  return () => {
    removeEventListener("popstate", cb);
    removeEventListener(MOVED, cb);
  };
};

const here = () => location.pathname + location.search + location.hash;

/**
 * The page's ?v= version code (null when it names none) and a setter that
 * writes it, null removing it: through the provider's navigate (a replace),
 * else the browser history. location overrides window.location.
 */
export function useVersionParam(o: { location?: string } = {}): [string | null, (code: string | null) => void] {
  const navigate = useContext(ContentKitContext)?.navigate;
  const current = useSyncExternalStore(subscribeHistory, here, () => "");
  const at = o.location ?? current;
  const set = useCallback(
    (code: string | null) => {
      const to = withVersion(o.location ?? here(), code);
      if (navigate) navigate(to, { replace: true });
      else history.replaceState(history.state, "", to);
      dispatchEvent(new Event(MOVED));
    },
    [navigate, o.location],
  );
  return [versionParam(at), set];
}
