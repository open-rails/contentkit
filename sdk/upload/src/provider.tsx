import { createContext, useCallback, useContext, useMemo, useRef, type ReactNode } from "react";
import type { UploadUiAppearance } from "./appearance.js";
import type { UploadClient } from "./client.js";
import { UploadError } from "./errors.js";
import { MessagesContext } from "./i18n/context.js";
import { createTranslator, resolveMessages, type UploadUiMessageBundle, type UploadUiTranslate } from "./i18n/messages.js";
import { AppearanceContext } from "./scope.js";
import { DensityContext } from "./components/rendition-img.js";
import { DEFAULT_DENSITY, type DensityRange } from "./rendition.js";
import { InlinePreviewContext } from "./inline-preview.js";

const ClientContext = createContext<UploadClient | null>(null);

/** What failed: a component's save, load or render step. */
export type UploadUiOperation =
  | "poster.load"
  | "poster.frame"
  | "poster.save"
  | "slot.load"
  | "slot.decode"
  | "slot.save"
  | "upload";

/**
 * Every failure a component shows is also reported here (aborts excepted), so
 * the host can toast it or log it. Components still show it in place.
 */
export type UploadUiErrorHandler = (error: UploadError, info: { operation: UploadUiOperation }) => void;

const ErrorContext = createContext<UploadUiErrorHandler | undefined>(undefined);

export const asUploadError = (e: unknown): UploadError =>
  e instanceof UploadError ? e : new UploadError("network", e instanceof Error ? e.message : String(e), 0, undefined, { cause: e });

/** A stable reporter: the component's own `onError`, else the provider's. */
export function useErrorReporter(own?: UploadUiErrorHandler): (e: unknown, operation: UploadUiOperation) => void {
  const ctx = useContext(ErrorContext);
  const handler = useRef(own ?? ctx);
  handler.current = own ?? ctx;
  return useCallback((e: unknown, operation: UploadUiOperation) => {
    const error = asUploadError(e);
    if (error.code !== "aborted") handler.current?.(error, { operation });
  }, []);
}

export interface UploadUiProviderProps {
  /** Default client for every component below; a component's own `client` prop wins. */
  client?: UploadClient;
  appearance?: UploadUiAppearance;
  /** Locale bundle(s) layered over English; later entries win. */
  messages?: UploadUiMessageBundle | readonly UploadUiMessageBundle[];
  /** Host translation hook, consulted before the bundles. */
  t?: UploadUiTranslate;
  /** Device-pixel density range images and covers are picked for. Default [2, 3]. */
  density?: DensityRange;
  /** Receives every failure the components show (a component's own `onError` wins). */
  onError?: UploadUiErrorHandler;
  /** Playable videos preview muted inline (hover, or in view on touch). Default true. */
  inlinePreview?: boolean;
  children?: ReactNode;
}

/** Renders no DOM; components create their own `.ckui` styling roots. */
export function UploadUiProvider({ client, appearance, messages, t, density = DEFAULT_DENSITY, onError, inlinePreview = true, children }: UploadUiProviderProps) {
  const translator = useMemo(() => createTranslator(resolveMessages(messages), t), [messages, t]);
  return (
    <ClientContext.Provider value={client ?? null}>
      <AppearanceContext.Provider value={appearance}>
        <MessagesContext.Provider value={translator}>
          <DensityContext.Provider value={density}>
            <ErrorContext.Provider value={onError}>
              <InlinePreviewContext.Provider value={inlinePreview}>{children}</InlinePreviewContext.Provider>
            </ErrorContext.Provider>
          </DensityContext.Provider>
        </MessagesContext.Provider>
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
