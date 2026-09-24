import { Alert02Icon, Camera01Icon, CropIcon, ImageUpload01Icon, Loading03Icon } from "@hugeicons/core-free-icons";
import { Button as ButtonPrimitive } from "@base-ui/react/button";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { createContext, useContext, useRef, type ReactElement, type ReactNode } from "react";
import type { UploadClient } from "../client.js";
import { useMessages } from "../i18n/context.js";
import type { CropSource } from "../image.js";
import { useUploadClient } from "../provider.js";
import { useScopeProps } from "../scope.js";
import { useSlotCrop, useSlotImage, type UseSlotCrop, type UseSlotImage } from "../slot-react.js";
import type { RefBody, SlotManifest } from "../wire.gen.js";
import { Button } from "#ckui/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "#ckui/ui/dropdown-menu";
import { ImageCropDialog } from "./image-crop-dialog.js";

export interface SlotEditorProps {
  /** The item that owns the slot. */
  item: RefBody;
  slot: string;
  client?: UploadClient;
  /** Current manifest from the host; otherwise fetched with client.getSlot. */
  manifest?: SlotManifest | null;
  /** Called with the new manifest after every save. */
  onChange?: (m: SlotManifest) => void;
  /** Width / height; default the manifest's aspect, else 1. */
  aspect?: number;
  /** Crops narrower than this many source pixels get a sharpness warning; default the widest output, else 512. */
  targetWidth?: number;
  /** Round crop mask; default aspect 1. */
  round?: boolean;
  /** Crop dialog title; default by aspect (avatar / cover). */
  title?: ReactNode;
  /** `accept` of the file input. Default "image/*". */
  accept?: string;
  disabled?: boolean;
  /** Replaces decodeImage (tests). */
  decode?: (file: File) => Promise<CropSource>;
  /** Triggers and the image, e.g. `<SlotEditMenu />`, or anything using useSlotEditor(). */
  children?: ReactNode;
}

export interface SlotEditorState {
  image: UseSlotImage;
  crop: UseSlotCrop;
  /** The slot has an image. */
  has: boolean;
  busy: boolean;
  disabled: boolean;
  /** A failure outside the dialog (unreadable file, manifest fetch), localized. */
  error?: string;
  /** Opens the file picker. */
  choose: () => void;
  /** Opens a file for cropping (drop targets). */
  pick: (file: File) => void;
  /** Re-crops the kept original; only when crop.canRecrop. */
  recrop: () => void;
}

const Ctx = createContext<SlotEditorState | null>(null);

/** The editor state of the enclosing SlotEditor. */
export function useSlotEditor(): SlotEditorState {
  const s = useContext(Ctx);
  if (!s) throw new Error("useSlotEditor must be used inside <SlotEditor>");
  return s;
}

/**
 * A slot's pick → crop → save and re-crop flow without layout: it renders a
 * hidden file input and the crop dialog, and its children draw the image and
 * triggers (SlotEditMenu, or custom ones through useSlotEditor).
 */
export function SlotEditor(p: SlotEditorProps) {
  const { t, error: errorText } = useMessages();
  const client = useUploadClient(p.client);
  const image = useSlotImage(client, { ref: p.item, slot: p.slot, manifest: p.manifest });
  const aspect = p.aspect ?? image.aspect;
  const onChange = useRef(p.onChange);
  onChange.current = p.onChange;
  const crop = useSlotCrop(client, {
    ref: p.item,
    slot: p.slot,
    manifest: image.manifest,
    aspect,
    decode: p.decode,
    onSaved: (m) => {
      image.set(m);
      onChange.current?.(m);
    },
  });
  const input = useRef<HTMLInputElement>(null);
  const busy = crop.status === "decoding" || crop.status === "saving";
  const disabled = !!p.disabled || busy;
  const round = p.round ?? aspect === 1;
  const target = p.targetWidth ?? image.manifest?.outputs.at(-1)?.w ?? 512;
  const error = crop.status === "error" && !crop.source ? errorText(crop.error) : image.error ? errorText(image.error) : undefined;
  const state: SlotEditorState = {
    image,
    crop,
    has: !!image.src,
    busy,
    disabled,
    error,
    choose: () => !disabled && input.current?.click(),
    pick: (f) => !disabled && void crop.pick(f),
    recrop: () => !disabled && void crop.recrop(),
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
 * One trigger for a SlotEditor: it picks a file, or, once the slot has a
 * kept original, opens a Change / Edit crop menu.
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
  if (!(s.has && s.crop.canRecrop)) {
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
        <DropdownMenuItem onClick={s.recrop} data-ckui="edit-crop">
          <HugeiconsIcon icon={CropIcon} strokeWidth={2} />
          {t("common.editCrop")}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/** The SlotEditor's error outside the dialog (unreadable file, fetch failure), if any. */
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
