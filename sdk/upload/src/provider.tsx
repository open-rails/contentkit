import { createContext, useContext, useMemo, type ReactNode } from "react";
import type { UploadUiAppearance } from "./appearance.js";
import type { UploadClient } from "./client.js";
import { MessagesContext } from "./i18n/context.js";
import { createTranslator, resolveMessages, type UploadUiMessageBundle, type UploadUiTranslate } from "./i18n/messages.js";
import { AppearanceContext } from "./scope.js";

const ClientContext = createContext<UploadClient | null>(null);

export interface UploadUiProviderProps {
  /** Default client for every component below; a component's own `client` prop wins. */
  client?: UploadClient;
  appearance?: UploadUiAppearance;
  /** Locale bundle(s) layered over English; later entries win. */
  messages?: UploadUiMessageBundle | readonly UploadUiMessageBundle[];
  /** Host translation hook, consulted before the bundles. */
  t?: UploadUiTranslate;
  children?: ReactNode;
}

/** Renders no DOM; components create their own `.ckui` styling roots. */
export function UploadUiProvider({ client, appearance, messages, t, children }: UploadUiProviderProps) {
  const translator = useMemo(() => createTranslator(resolveMessages(messages), t), [messages, t]);
  return (
    <ClientContext.Provider value={client ?? null}>
      <AppearanceContext.Provider value={appearance}>
        <MessagesContext.Provider value={translator}>{children}</MessagesContext.Provider>
      </AppearanceContext.Provider>
    </ClientContext.Provider>
  );
}

export function useUploadClient(own?: UploadClient): UploadClient {
  const ctx = useContext(ClientContext);
  const c = own ?? ctx;
  if (!c) throw new Error("contentkit-upload: pass `client` or render inside <UploadUiProvider client>");
  return c;
}

export function useOptionalUploadClient(own?: UploadClient): UploadClient | null {
  const ctx = useContext(ClientContext);
  return own ?? ctx;
}
