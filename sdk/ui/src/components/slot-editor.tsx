import { ratio, type AspectRatio } from "../client/aspect.js";
import { Alert02Icon, Camera01Icon, CropIcon, Delete02Icon, ImageUpload01Icon, Loading03Icon } from "@hugeicons/core-free-icons";
import { Button as ButtonPrimitive } from "@base-ui/react/button";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { createContext, useContext, useEffect, useRef, useState, type ReactElement, type ReactNode } from "react";
import type { ContentKitClient } from "../client/client.js";
import { useMessages } from "../i18n/context.js";
import type { CropSource } from "../client/image.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import { useContentKitClient, useErrorReporter, type ContentKitErrorHandler } from "../react/context.js";
import { useScopeProps } from "../scope.js";
import type { PublicPreset } from "../client/public.js";
import { useSlotCrop, useSlotImage, type UseSlotCrop, type UseSlotImage } from "../react/slot.js";
import type { FileInfo, ReadResult, RefBody } from "../client/generated/wire.js";
import { Button } from "#ckui/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "#ckui/ui/dropdown-menu";
import { ImageCropDialog } from "./image-crop-dialog.js";

export interface SlotEditorProps {
  /** The item that owns the upload. */
  item: RefBody;
  /** The upload path, e.g. "cover" or "avatar". */
  path: string;
  /** The public preset showing the upload: children draw it, saves refetch it. */
  image?: PublicPreset | null;
  client?: ContentKitClient;
  /** An editor read of the item from the host; otherwise fetched. */
  read?: ReadResult | null;
  /** Called with the processed upload after every save, null after a removal. */
  onChange?: (file: FileInfo | null) => void;
  /** Every failure (load, decode, save or render); default the provider's. */
  onError?: ContentKitErrorHandler;
  /** The output's "W:H"; default the preset's aspect, else "1:1". */
  aspect?: AspectRatio;
  /** The narrowest edit the server accepts (the preset's Image.MinWidth). */
  minWidth?: number;
  /** "reject" refuses animated images before uploading (the preset's Image.Animation). */
  animation?: "reject";
  /** Crops narrower than this many source pixels get a sharpness warning; default the preset's widest, else 512. */
  targetWidth?: number;
  /** Round crop mask; default aspect 1. */
  round?: boolean;
  /** Crop dialog title; default by aspect (avatar / cover). */
  title?: ReactNode;
  /** `accept` of the file input. Default "image/*". */
  accept?: string;
  disabled?: boolean;
  /** Offers Remove (a remove op) once the path has an upload; default false. */
  removable?: boolean;
  /** Replaces decodeImage (tests). */
  decode?: (file: File) => Promise<CropSource>;
  /** Triggers and the image, e.g. `<SlotEditMenu />`, or anything using useSlotEditor(). */
  children?: ReactNode;
}

export interface SlotEditorState {
  image: UseSlotImage;
  crop: UseSlotCrop;
  /** The path has an upload. */
  has: boolean;
  busy: boolean;
  disabled: boolean;
  /** A failure outside the dialog (unreadable file, read failure), localized. */
  error?: string;
  /** Opens the file picker. */
  choose: () => void;
  /** Opens a file for cropping (drop targets). */
  pick: (file: File) => void;
  /** Re-crops the kept upload; only when crop.canRecrop. */
  recrop: () => void;
  /** Remove is offered (SlotEditorProps.removable). */
  removable: boolean;
  /** Removes the upload; only when removable. */
  remove: () => void;
}

const Ctx = createContext<SlotEditorState | null>(null);

/** The editor state of the enclosing SlotEditor. */
export function useSlotEditor(): SlotEditorState {
  const s = useContext(Ctx);
  if (!s) throw new Error("useSlotEditor must be used inside <SlotEditor>");
  return s;
}

/**
 * An image upload's pick → crop → save and re-crop flow without layout: it renders a
 * hidden file input and the crop dialog, and its children draw the image and
 * triggers (SlotEditMenu, or custom ones through useSlotEditor).
 */
export function SlotEditor(p: SlotEditorProps) {
  const { t, error: errorText } = useMessages();
  const client = useContentKitClient(p.client);
  const report = useErrorReporter(p.onError);
  const image = useSlotImage({ client, ref: p.item, path: p.path, image: p.image, read: p.read });
  useEffect(() => void (image.error && report(image.error, "slot.load")), [image.error, report]);
  const aspect = p.aspect ?? image.aspect;
  const onChange = useRef(p.onChange);
  onChange.current = p.onChange;
  const crop = useSlotCrop({
    client,
    ref: p.item,
    path: p.path,
    file: image.file,
    aspect,
    animation: p.animation,
    decode: p.decode,
    onError: report,
    onSaved: (f) => {
      image.set(f);
      image.reload();
      onChange.current?.(f);
    },
  });
  const input = useRef<HTMLInputElement>(null);
  const [removing, setRemoving] = useState(false);
  const [removeError, setRemoveError] = useState<ContentKitError>();
  const busy = crop.status === "decoding" || crop.status === "saving" || removing;
  const disabled = !!p.disabled || busy;
  const round = p.round ?? ratio(aspect) === 1;
  const target = p.targetWidth ?? image.renditions.at(-1)?.w ?? 512;
  const error =
    crop.status === "error" && !crop.source
      ? errorText(crop.error)
      : image.error
        ? errorText(image.error)
        : removeError
          ? errorText(removeError)
          : undefined;
  const remove = async () => {
    setRemoving(true);
    setRemoveError(undefined);
    try {
      if (image.file) await client.media.commit(p.item, [{ op: "remove", path: image.file.path }]);
      image.set(null);
      image.reload();
      onChange.current?.(null);
    } catch (e) {
      setRemoveError(toContentKitError(e));
      report(e, "slot.remove");
    } finally {
      setRemoving(false);
    }
  };
  const state: SlotEditorState = {
    image,
    crop,
    has: !!image.file,
    busy,
    disabled,
    error,
    choose: () => !disabled && input.current?.click(),
    pick: (f) => !disabled && void crop.pick(f),
    recrop: () => !disabled && void crop.recrop(),
    removable: !!p.removable,
    remove: () => !disabled && !!p.removable && void remove(),
  };
  return (
    <Ctx.Provider value={state}>
      {p.children}
      <input
        ref={input}
        type="file"
        accept={p.accept ?? "image/*"}
        hidden
        data-ckui="file"
        onChange={(e) => {
          const f = e.target.files?.[0];
          e.target.value = "";
          if (f) void crop.pick(f);
        }}
      />
      <ImageCropDialog
        open={"source" in crop && !!crop.source}
        onOpenChange={(o) => !o && crop.cancel()}
        source={"source" in crop ? (crop.source ?? null) : null}
        aspect={aspect}
        round={round}
        initialEdit={"edit" in crop && crop.mode === "recrop" ? crop.edit : undefined}
        targetWidth={target}
        minWidth={p.minWidth}
        title={p.title ?? t(round ? "crop.avatarTitle" : "crop.coverTitle")}
        busy={crop.status === "saving"}
        progress={crop.status === "saving" ? crop.progress : undefined}
        rendering={crop.status === "saving" && crop.rendering}
        error={crop.status === "error" && crop.source ? errorText(crop.error) : undefined}
        onEditChange={crop.setEdit}
        onConfirm={(e) => void crop.save(e)}
      />
    </Ctx.Provider>
  );
}

export interface SlotEditMenuProps {
  /** Accessible name and text; default "Change" (or "Upload" when empty). */
  label?: string;
  /** Shows only the icon; the label becomes its aria-label. */
  iconOnly?: boolean;
  /**
   * The trigger element (Base UI `render`), e.g. `<button className="…" />`
   * styled by the host; it receives the icon and label unless it has children.
   * Default: the kit's outline button.
   */
  render?: ReactElement<Record<string, unknown>>;
  className?: string;
  align?: "start" | "center" | "end";
}

/**
 * One trigger for a SlotEditor: it picks a file, or, once the path has an
 * upload, opens a Change / Edit crop / Remove menu (each when available).
 */
export function SlotEditMenu({ label, iconOnly, render, className, align = "end" }: SlotEditMenuProps) {
  const { t } = useMessages();
  const s = useSlotEditor();
  const scope = useScopeProps();
  const name = label ?? (s.has ? t("common.change") : t("common.upload"));
  const content = (
    <>
      <HugeiconsIcon icon={s.busy ? Loading03Icon : Camera01Icon} size={16} strokeWidth={2} data-icon="inline-start" className={s.busy ? "animate-spin" : undefined} />
      {!iconOnly && name}
    </>
  );
  // The kit's button carries the style scope itself: no wrapper to break host layouts.
  const trigger = render ?? <Button variant="outline" size={iconOnly ? "icon-sm" : "sm"} {...scope} className={cn(scope.className, className)} style={scope.style} />;
  const props = {
    render: trigger,
    disabled: s.disabled,
    title: name,
    "aria-label": iconOnly ? name : undefined,
    "data-ckui": "slot-edit",
    "data-busy": s.busy || undefined,
    className: render ? className : undefined,
  };
  if (!(s.has && (s.crop.canRecrop || s.removable))) {
    return (
      <ButtonPrimitive {...props} onClick={s.choose}>
        {content}
      </ButtonPrimitive>
    );
  }
  return (
    <DropdownMenu>
      <DropdownMenuTrigger {...props}>{content}</DropdownMenuTrigger>
      <DropdownMenuContent align={align}>
        <DropdownMenuItem onClick={s.choose} data-ckui="change">
          <HugeiconsIcon icon={ImageUpload01Icon} strokeWidth={2} />
          {t("common.change")}
        </DropdownMenuItem>
        {s.crop.canRecrop && (
          <DropdownMenuItem onClick={s.recrop} data-ckui="edit-crop">
            <HugeiconsIcon icon={CropIcon} strokeWidth={2} />
            {t("common.editCrop")}
          </DropdownMenuItem>
        )}
        {s.removable && (
          <DropdownMenuItem onClick={s.remove} data-ckui="remove">
            <HugeiconsIcon icon={Delete02Icon} strokeWidth={2} />
            {t("common.remove")}
          </DropdownMenuItem>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/** The SlotEditor's error outside the dialog (unreadable file, read failure), if any. */
export function SlotEditError({ className }: { className?: string }) {
  const { error } = useSlotEditor();
  const scope = useScopeProps();
  if (!error) return null;
  return (
    <p role="alert" data-ckui="slot-error" {...scope} className={cn(scope.className, "flex items-center gap-1.5 text-xs text-destructive", className)}>
      <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} className="size-3.5 shrink-0" />
      {error}
    </p>
  );
}
