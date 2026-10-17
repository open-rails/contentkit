import { useContext, useState, type ReactNode } from "react";
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
  // Native modal dialogs live above body portals. Keep the opening surface
  // through preference reloads, even when they remove the triggering tile.
  const [portal, setPortal] = useState<{ open: boolean; container?: HTMLDialogElement }>({ open: false });
  let placement = portal;
  if (portal.open !== open) {
    placement = { open, container: open ? returnFocus?.()?.closest<HTMLDialogElement>("dialog[open]") ?? undefined : portal.container };
    setPortal(placement);
  }
  return <AppearanceContext.Provider value={settings.appearance ?? inheritedAppearance}>
    <Dialog open={open} onOpenChange={(next) => onOpenChange(next)}>
      <DialogContent
        finalFocus={returnFocus}
        portalContainer={placement.container}
        className="max-h-[calc(100dvh-2rem)] overflow-y-auto"
      >
        <DialogHeader>
          <DialogTitle>{t("readerSettings.filterTitle")}</DialogTitle>
          <DialogDescription>{description ?? t("readerSettings.filterDescription")}</DialogDescription>
        </DialogHeader>
        <ContentFilterSettings {...settings} includeDeleted={settings.includeDeleted ?? false} />
      </DialogContent>
    </Dialog>
  </AppearanceContext.Provider>;
}
