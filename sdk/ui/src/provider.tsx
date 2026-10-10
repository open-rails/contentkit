import { useEffect, useMemo, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "./appearance.js";
import { DEFAULT_DENSITY, type DensityRange } from "./client/rendition.js";
import { DensityContext } from "./components/rendition-img.js";
import { MessagesContext } from "./i18n/context.js";
import { createTranslator, resolveMessages, type ContentKitUiMessageBundle, type ContentKitUiTranslate } from "./i18n/messages.js";
import { InlinePreviewContext } from "./react/inline-preview.js";
import { AppearanceContext } from "./scope.js";

/** Built-in bundles besides English, loaded on demand. */
const BUNDLES: Record<string, () => Promise<ContentKitUiMessageBundle>> = {
  de: () => import("./locales/de.js").then((m) => m.de),
  es: () => import("./locales/es.js").then((m) => m.es),
  ja: () => import("./locales/ja.js").then((m) => m.ja),
  ko: () => import("./locales/ko.js").then((m) => m.ko),
  zh: () => import("./locales/zh.js").then((m) => m.zh),
};

/** The built-in bundle's language for a BCP 47 tag ("ja-JP" → "ja"); undefined for English or one we lack. */
export function bundleLanguage(language: string | null | undefined): string | undefined {
  const primary = language?.toLowerCase().split(/[-_]/)[0];
  return primary && primary in BUNDLES ? primary : undefined;
}

export interface ContentKitUiProviderProps {
  appearance?: ContentKitUiAppearance;
  /** BCP 47 tag; its built-in bundle loads under `messages`. English until it arrives. */
  language?: string;
  /** Bundle(s) layered over English and the language's bundle; later entries win. */
  messages?: ContentKitUiMessageBundle | readonly ContentKitUiMessageBundle[];
  /** Host translation hook, consulted before the bundles. */
  t?: ContentKitUiTranslate;
  /** Device-pixel density range images and covers are picked for. Default [2, 3]. */
  density?: DensityRange;
  /** Playable videos preview muted inline (hover, or in view on touch). Default true. */
  inlinePreview?: boolean;
  children?: ReactNode;
}

/** Look and language for every component below. Renders no DOM; components create their own `.ckui` roots. */
export function ContentKitUiProvider({ appearance, language, messages, t, density = DEFAULT_DENSITY, inlinePreview = true, children }: ContentKitUiProviderProps) {
  const lang = bundleLanguage(language);
  const [loaded, setLoaded] = useState<{ lang: string; bundle: ContentKitUiMessageBundle } | null>(null);
  useEffect(() => {
    if (!lang) return;
    let live = true;
    BUNDLES[lang]!().then(
      (bundle) => live && setLoaded({ lang, bundle }),
      () => {},
    );
    return () => {
      live = false;
    };
  }, [lang]);
  const bundle = lang && loaded?.lang === lang ? loaded.bundle : undefined;
  const translator = useMemo(() => {
    const own = messages ? (Array.isArray(messages) ? messages : [messages]) : [];
    return createTranslator(resolveMessages(bundle ? [bundle, ...own] : own), t);
  }, [bundle, messages, t]);
  return (
    <AppearanceContext.Provider value={appearance}>
      <MessagesContext.Provider value={translator}>
        <DensityContext.Provider value={density}>
          <InlinePreviewContext.Provider value={inlinePreview}>{children}</InlinePreviewContext.Provider>
        </DensityContext.Provider>
      </MessagesContext.Provider>
    </AppearanceContext.Provider>
  );
}
