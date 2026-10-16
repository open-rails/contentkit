import { Settings01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { useContext, useId, useSyncExternalStore } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import { useMessages } from "../../i18n/context.js";
import { useOptionalContentKitClient, type ContentKitErrorHandler } from "../../react/context.js";
import { useContentFilters } from "../../react/discovery.js";
import { AppearanceContext, ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "#ckui/ui/dialog";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { ContentFilterSettings, type ContentFilterSettingsProps } from "./filters.js";
import { ErrorLine, useAction } from "../social/parts.js";
import type { ReaderPreferenceState } from "../../client/reader-preferences.js";

const EMPTY_PREFERENCES: ReaderPreferenceState = Object.freeze({ loading: false, saving: false, pending: false });
const noPreferences = () => EMPTY_PREFERENCES;
const noSubscription = () => () => {};

export interface ReaderSettingsProps {
  /** Content kinds offered in this dialog; omitted shows all configured kinds. */
  kinds?: ContentFilterSettingsProps["kinds"];
  labels?: ContentFilterSettingsProps["labels"];
  languages: readonly { value: string; label: string }[];
  /** The current language, controlled by the host. */
  language: string;
  /** The host applies its own route and preference changes. */
  onLanguageChange: (language: string) => void;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/** Language preferences and filters for both guests and signed-in readers. */
export function ReaderSettings(p: ReaderSettingsProps) {
  const messages = useMessages();
  const { t } = messages;
  const id = useId();
  const client = useOptionalContentKitClient(p.client);
  const preferences = client?.preferences;
  const state = useSyncExternalStore(preferences?.subscribe ?? noSubscription, preferences?.snapshot ?? noPreferences, noPreferences);
  // The selector describes the current view. A saved default must not make a
  // localized deep link look selected in a language it is not displaying.
  const selectedLanguage = p.language;
  const save = useAction("preferences.save", p.onError);
  const inheritedAppearance = useContext(AppearanceContext);
  const appearance = p.appearance ?? inheritedAppearance;
  return (
    <AppearanceContext.Provider value={appearance}>
      <ContentKitUiRoot className={p.className} appearance={appearance} data-ckui="reader-settings">
        <Dialog>
          <DialogTrigger render={<Button type="button" variant="ghost" size="sm" aria-label={t("readerSettings.title")} data-ckui="reader-settings-trigger" />}>
            <HugeiconsIcon icon={Settings01Icon} strokeWidth={2} className="size-4" aria-hidden />
            <span>{p.language.toUpperCase()}</span>
          </DialogTrigger>
          <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto">
            <DialogHeader>
              <DialogTitle>{t("readerSettings.title")}</DialogTitle>
              <DialogDescription className="sr-only">{t("readerSettings.description")}</DialogDescription>
            </DialogHeader>
            <div className="grid gap-3">
              <Label htmlFor={`${id}-language`}>{t("listing.language")}</Label>
              <Select value={selectedLanguage} disabled={save.pending || state.loading} onValueChange={(value) => {
                if (!value || value === selectedLanguage) return;
                void save.run(async () => { await preferences?.setLanguage(value); p.onLanguageChange(value); });
              }}>
                <SelectTrigger id={`${id}-language`} className="w-full">
                  <SelectValue>{p.languages.find((item) => item.value === selectedLanguage)?.label ?? selectedLanguage}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {p.languages.map((item) => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            {client && <ReaderFilters language={p.language} kinds={p.kinds} labels={p.labels} client={client} onError={p.onError} />}
            {(state.pending || save.pending) && !state.error && <p className="text-sm text-muted-foreground" role="status">{t("readerSettings.savingPreferences")}</p>}
            <ErrorLine>{state.error || save.error ? t("readerSettings.preferencesNotSaved") : null}</ErrorLine>
            {state.error && preferences && <Button type="button" variant="outline" onClick={() => { void save.run(() => preferences.retry()); }}>{t("common.retry")}</Button>}
          </DialogContent>
        </Dialog>
      </ContentKitUiRoot>
    </AppearanceContext.Provider>
  );
}

function ReaderFilters(p: Pick<ReaderSettingsProps, "language" | "client" | "onError" | "kinds" | "labels">) {
  const { t } = useMessages();
  const id = useId();
  const { filters } = useContentFilters({ language: p.language, client: p.client });
  if (filters && !Object.entries(filters.kinds).some(([kind, rows]) => rows.length && (!p.kinds || p.kinds.includes(kind)))) return null;
  return (
    <section aria-labelledby={id} className="grid gap-3">
      <h3 id={id} className="font-medium">{t("readerSettings.filters")}</h3>
      <ContentFilterSettings {...p} includeDeleted={false} />
    </section>
  );
}
