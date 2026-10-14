import { createContext, useContext, useMemo } from "react";
import { createTranslator, defaultMessages, type Translator } from "./messages.js";

export const MessagesContext = createContext<Translator>(createTranslator(defaultMessages));

/** English defaults when rendered outside a `ContentKitUiProvider`. */
export function useMessages(): Translator {
  return useContext(MessagesContext);
}

/** A language's name in the UI's language ("ja" → "Japanese"). */
export function useLanguageName(): (code: string) => string {
  const { language } = useMessages();
  return useMemo(() => {
    let names: Intl.DisplayNames | undefined;
    try {
      names = new Intl.DisplayNames(language ? [language] : undefined, { type: "language" });
    } catch {
      names = undefined;
    }
    return (code: string) => {
      try {
        return names?.of(code) ?? code;
      } catch {
        return code;
      }
    };
  }, [language]);
}
