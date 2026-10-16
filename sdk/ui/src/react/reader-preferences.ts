import { useSyncExternalStore } from "react";
import type { ContentKitClient } from "../client/client.js";
import { useContentKitClient } from "./context.js";

/** Stored preference state; changing a route's language never writes it. */
export function useReaderPreferences(o: { client?: ContentKitClient } = {}) {
  const preferences = useContentKitClient(o.client).preferences;
  const state = useSyncExternalStore(preferences.subscribe, preferences.snapshot, preferences.snapshot);
  return { ...state, setLanguage: (language: string) => preferences.setLanguage(language), retry: () => preferences.retry() };
}
