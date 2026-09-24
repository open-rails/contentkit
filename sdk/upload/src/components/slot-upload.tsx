import { Camera01Icon, CropIcon, ImageUpload01Icon, Loading03Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useState, type DragEvent, type ReactNode } from "react";
import type { UploadClient } from "../client.js";
import { useMessages } from "../i18n/context.js";
import type { CropSource } from "../image.js";
import { UploadUiRoot } from "../scope.js";
import type { RefBody, SlotManifest } from "../wire.gen.js";
import { Button } from "#ckui/ui/button";
import { SlotEditError, SlotEditor, useSlotEditor } from "./slot-editor.js";
import { SlotImage } from "./slot-image.js";

export interface SlotUploadProps {
  /** The item that owns the slot. */
  item: RefBody;
  slot?: string;
  client?: UploadClient;
  /** Current manifest from the host; otherwise fetched with client.getSlot. */
  manifest?: SlotManifest | null;
  /** Called with the new manifest after every save. */
  onChange?: (m: SlotManifest) => void;
  /** Width / height; default the manifest's aspect, else 1 (avatar) or 3 (cover). */
  aspect?: number;
  /** Crops narrower than this many source pixels get a sharpness warning; default the largest output. */
  targetWidth?: number;
  /** `accept` of the file input. Default "image/*". */
  accept?: string;
  /** `sizes` for the rendered srcset. */
  sizes?: string;
  disabled?: boolean;
  label?: ReactNode;
  hint?: ReactNode;
  className?: string;
  /** Replaces decodeImage (tests). */
  decode?: (file: File) => Promise<CropSource>;
}

type Variant = "avatar" | "cover";

/** Round 1:1 avatar field: image, label, hint, Change / Edit crop, crop dialog, progress, errors. For custom layouts use SlotEditor. */
export function AvatarUpload(p: SlotUploadProps) {
  return <SlotUpload {...p} variant="avatar" />;
}

/** Wide cover field at the slot's aspect (e.g. 3:1), with drop-to-upload. For custom layouts use SlotEditor. */
export function CoverUpload(p: SlotUploadProps) {
  return <SlotUpload {...p} variant="cover" />;
}

function SlotUpload({ variant, ...p }: SlotUploadProps & { variant: Variant }) {
  const slot = p.slot ?? variant;
  return (
    <UploadUiRoot className={cn("text-sm", p.className)} data-ckui={variant === "avatar" ? "avatar-upload" : "cover-upload"}>
    <SlotEditor
        item={p.item}
        slot={slot}
        client={p.client}
        manifest={p.manifest}
        onChange={p.onChange}
        aspect={p.aspect ?? (p.manifest === undefined ? undefined : (p.manifest?.aspect ?? (variant === "avatar" ? 1 : 3)))}
        targetWidth={p.targetWidth ?? (variant === "avatar" ? 512 : 3000)}
        round={variant === "avatar"}
        accept={p.accept}
        disabled={p.disabled}
        decode={p.decode}
      >
        <SlotUploadLayout {...p} variant={variant} />
      </SlotEditor>
    </UploadUiRoot>
  );
}

function SlotUploadLayout({ variant, ...p }: SlotUploadProps & { variant: Variant }) {
  const { t } = useMessages();
  const s = useSlotEditor();
  const { image, crop, has, busy, disabled } = s;
  const [dragging, setDragging] = useState(false);
  const aspect = crop.aspect;
  const target = p.targetWidth ?? (variant === "avatar" ? 512 : 3000);
  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    setDragging(false);
    const f = [...e.dataTransfer.files].find((f) => f.type.startsWith("image/")) ?? e.dataTransfer.files[0];
    if (f) s.pick(f);
  };

  const hint =
    p.hint ?? (variant === "avatar" ? t("avatar.hint", { size: target }) : t("cover.hint", { width: target, height: Math.round(target / aspect) }));
  const label = p.label ?? t(variant === "avatar" ? "avatar.label" : "cover.label");

  // overlay: buttons on top of the cover image; otherwise a plain row.
  const actions = (overlay: boolean) => (
    <>
      <Button variant={overlay && has ? "secondary" : "outline"} size="sm" disabled={disabled} onClick={s.choose} data-ckui="change">
        <HugeiconsIcon icon={busy ? Loading03Icon : Camera01Icon} strokeWidth={2} data-icon="inline-start" className={busy ? "animate-spin" : undefined} />
        {has ? t("common.change") : t("common.upload")}
      </Button>
      {has && crop.canRecrop && (
        <Button variant={overlay ? "secondary" : "ghost"} size="sm" disabled={disabled} onClick={s.recrop} data-ckui="edit-crop">
          <HugeiconsIcon icon={CropIcon} strokeWidth={2} data-icon="inline-start" />
          {t("common.editCrop")}
        </Button>
      )}
    </>
  );

  return variant === "avatar" ? (
    <div className="flex items-center gap-4">
      <div className="relative size-20 shrink-0">
        <SlotImage manifest={image.manifest} round sizes={p.sizes ?? "80px"} emptyLabel={t("avatar.empty")} className="size-full ring-1 ring-foreground/10" />
        {busy && <BusyOverlay round label={t(crop.status === "decoding" ? "progress.decoding" : "common.saving")} />}
      </div>
      <div className="grid min-w-0 gap-1">
        <div className="font-medium">{label}</div>
        <div className="text-xs text-muted-foreground">{hint}</div>
        <div className="mt-1.5 flex flex-wrap gap-2">{actions(false)}</div>
        <SlotEditError />
      </div>
    </div>
  ) : (
    <div className="grid gap-2">
      <div
        className={cn(
          "group/cover relative overflow-hidden rounded-xl ring-1 ring-foreground/10 transition-shadow",
          dragging && "ring-2 ring-ring",
        )}
        onDragOver={(e) => {
          e.preventDefault();
          if (!disabled) setDragging(true);
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={onDrop}
      >
        <SlotImage
          manifest={image.manifest}
          aspect={aspect}
          sizes={p.sizes ?? "(min-width: 768px) 768px, 100vw"}
          className="w-full rounded-none"
          placeholder={
            <div aria-label={t("cover.empty")} role="group" className="absolute inset-0 flex flex-col items-center justify-center gap-1.5 bg-[repeating-linear-gradient(135deg,transparent_0_10px,color-mix(in_oklch,var(--ckui-foreground)_3%,transparent)_10px_20px)] p-4 text-center">
              <HugeiconsIcon icon={ImageUpload01Icon} strokeWidth={1.5} className="size-7 text-muted-foreground" />
              <span className="hidden text-xs text-muted-foreground sm:block">{t("cover.drop")}</span>
              <div className="mt-1 flex gap-2">{actions(false)}</div>
            </div>
          }
        />
        {busy && <BusyOverlay label={t(crop.status === "decoding" ? "progress.decoding" : "common.saving")} />}
        {has && (
          <div className="absolute right-2 bottom-2 flex gap-2 max-sm:hidden [&_button]:bg-background/80 [&_button]:backdrop-blur-sm">
            {actions(true)}
          </div>
        )}
      </div>
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1.5">
        <div className="grid min-w-0 gap-0.5 sm:flex sm:flex-1 sm:items-baseline sm:justify-between sm:gap-3">
          <div className="font-medium">{label}</div>
          <div className="text-xs text-muted-foreground sm:truncate">{hint}</div>
        </div>
        {has && <div className="flex gap-2 sm:hidden">{actions(false)}</div>}
      </div>
      <SlotEditError />
    </div>
  );
}

function BusyOverlay({ round, label }: { round?: boolean; label: string }) {
  return (
    <div role="status" aria-label={label} className={cn("absolute inset-0 flex items-center justify-center bg-background/60 backdrop-blur-[1px]", round && "rounded-full")}>
      <HugeiconsIcon icon={Loading03Icon} strokeWidth={2} className="size-5 animate-spin text-foreground" />
    </div>
  );
}
