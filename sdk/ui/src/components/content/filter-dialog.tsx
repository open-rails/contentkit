import { useContext, type ReactNode } from "react";
import { useMessages } from "../../i18n/context.js";
import { AppearanceContext } from "../../scope.js";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "#ckui/ui/dialog";
import { ContentFilterSettings, type ContentFilterSettingsProps } from "./filters.js";

export interface ContentFilterDialogProps extends ContentFilterSettingsProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  description?: ReactNode;
  /** A stable focus target when a preference change removes the opening tile. */
  returnFocus?: () => HTMLElement | null;
}

/** The same filter controls as ReaderSettings, without another language picker. */
export function ContentFilterDialog({ open, onOpenChange, description, returnFocus, ...settings }: ContentFilterDialogProps) {
  const { t } = useMessages();
  const inheritedAppearance = useContext(AppearanceContext);
  return <AppearanceContext.Provider value={settings.appearance ?? inheritedAppearance}>
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent finalFocus={returnFocus} className="max-h-[calc(100dvh-2rem)] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{t("readerSettings.filterTitle")}</DialogTitle>
          <DialogDescription>{description ?? t("readerSettings.filterDescription")}</DialogDescription>
        </DialogHeader>
        <ContentFilterSettings {...settings} includeDeleted={settings.includeDeleted ?? false} />
      </DialogContent>
    </Dialog>
  </AppearanceContext.Provider>;
}
