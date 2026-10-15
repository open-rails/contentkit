import { Settings01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { useContext, useId } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import { useMessages } from "../../i18n/context.js";
import { ContentKitContext, type ContentKitErrorHandler } from "../../react/context.js";
import { useContentFilters } from "../../react/discovery.js";
import { AppearanceContext, ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "#ckui/ui/dialog";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { ContentFilterSettings } from "./filters.js";

export interface ReaderSettingsProps {
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

/** One reader-settings dialog for language and the signed-in reader's filters. */
export function ReaderSettings(p: ReaderSettingsProps) {
  const { t } = useMessages();
  const id = useId();
  const viewer = useContext(ContentKitContext)?.viewer;
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
              <Select value={p.language} onValueChange={(value) => { if (value && value !== p.language) p.onLanguageChange(value); }}>
                <SelectTrigger id={`${id}-language`} className="w-full">
                  <SelectValue>{p.languages.find((item) => item.value === p.language)?.label ?? p.language}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {p.languages.map((item) => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            {viewer != null && <ReaderFilters language={p.language} client={p.client} onError={p.onError} />}
          </DialogContent>
        </Dialog>
      </ContentKitUiRoot>
    </AppearanceContext.Provider>
  );
}

function ReaderFilters(p: Pick<ReaderSettingsProps, "language" | "client" | "onError">) {
  const { t } = useMessages();
  const id = useId();
  const { filters } = useContentFilters({ language: p.language, client: p.client });
  if (filters?.filters.length === 0) return null;
  return (
    <section aria-labelledby={id} className="grid gap-3">
      <h3 id={id} className="font-medium">{t("readerSettings.filters")}</h3>
      <ContentFilterSettings {...p} includeDeleted={false} />
    </section>
  );
}
