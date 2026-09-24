import { createContext, useContext } from "react";
import { createTranslator, defaultMessages, type Translator } from "./messages.js";

export const MessagesContext = createContext<Translator>(createTranslator(defaultMessages));

/** English defaults when rendered outside an `UploadUiProvider`. */
export function useMessages(): Translator {
  return useContext(MessagesContext);
}
