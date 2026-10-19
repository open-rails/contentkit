import { Cancel01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useContext, type ReactNode } from "react";
import { useMessages } from "../../i18n/context.js";
import { AppearanceContext } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "#ckui/ui/dialog";
import { SearchBox, type SearchBoxProps } from "./search.js";

export interface SearchDialogProps extends Omit<SearchBoxProps, "presentation"> {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title?: ReactNode;
  description?: ReactNode;
  /** A stable focus target for hosts that conditionally mount the dialog. */
  returnFocus?: () => HTMLElement | null;
}

/** Search with an in-flow, scrollable suggestions panel and a separate close control. */
export function SearchDialog({ open, onOpenChange, title, description, returnFocus, className, onSelect, ...search }: SearchDialogProps) {
  const { t } = useMessages();
  const inheritedAppearance = useContext(AppearanceContext);
  return <AppearanceContext.Provider value={search.appearance ?? inheritedAppearance}>
    <Dialog open={open} onOpenChange={(next) => onOpenChange(next)}>
      <DialogContent
        data-ckui="search-dialog"
        showCloseButton={false}
        finalFocus={returnFocus}
        className={cn("top-4 flex max-h-[calc(100dvh-2rem)] w-[calc(100%-2rem)] max-w-2xl translate-y-0 flex-col gap-5 overflow-hidden p-5 sm:top-[12dvh] sm:max-h-[calc(88dvh-1rem)] sm:max-w-2xl sm:p-6", className)}
      >
        <DialogHeader className="shrink-0 gap-1.5 pr-10">
          <DialogTitle className="text-lg font-semibold">{title ?? t("contents.search")}</DialogTitle>
          <DialogDescription>{description ?? t("contents.searchHint")}</DialogDescription>
        </DialogHeader>
        <SearchBox {...search} presentation="dialog" onSelect={() => { onSelect?.(); onOpenChange(false); }} />
        <DialogClose render={<Button type="button" variant="ghost" size="icon" className="absolute top-3 right-3 size-11" />}>
          <HugeiconsIcon icon={Cancel01Icon} strokeWidth={2} aria-hidden />
          <span className="sr-only">{t("common.close")}</span>
        </DialogClose>
      </DialogContent>
    </Dialog>
  </AppearanceContext.Provider>;
}
