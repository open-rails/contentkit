import { cn } from "cn";
import { useRef, useState } from "react";
import type { Edit, Avatar, EntitiesCover } from "../client/generated/wire.js";
import { decodeImage, type CropSource } from "../client/image.js";
import { useMessages } from "../i18n/context.js";
import type { ContentKitErrorHandler, SocialOperation } from "../react/context.js";
import { Button } from "#ckui/ui/button";
import { ImageCropDialog } from "./image-crop-dialog.js";
import { ErrorLine, useAction } from "./social/parts.js";
import { ProfileImage, type ProfileImageValue } from "./profile-image.js";

export interface ProfileImageEditorProps {
  value: ProfileImageValue;
  slot: "cover" | "avatar";
  crop?: boolean;
  preview?: string;
  uploadImage: (slot: "cover" | "avatar", file: Blob & { name?: string }, options: { wait?: boolean; edit?: Edit }) => Promise<unknown>;
  update: (patch: { cover?: EntitiesCover; avatar?: Avatar }) => Promise<unknown>;
  onError?: ContentKitErrorHandler;
  className?: string;
  operation: SocialOperation;
  marker: string;
}

/** A entity's cover or avatar for its editors: pick (and crop) an image, upload it and select it; or go back to none (a cover: automatic). */
export function ProfileImageEditor({ value, slot, crop = true, uploadImage, update, preview, onError, className, operation, marker }: ProfileImageEditorProps) {
  const { t } = useMessages();
  const input = useRef<HTMLInputElement>(null);
  const save = useAction(operation, onError);
  const [source, setSource] = useState<CropSource | null>(null);
  const [file, setFile] = useState<File | null>(null);
  const shown = value;
  const set = slot === "cover" ? !!shown.cover?.image || (!!shown.cover?.content_id && !shown.cover.auto) : !!shown.avatar?.image;
  const upload = (f: Blob & { name?: string }, edit?: Edit) => save.run(() => uploadImage(slot, f, { wait: false, edit }));
  const close = () => {
    source?.revoke?.();
    setSource(null);
    setFile(null);
  };
  return (
    <div className={cn("flex flex-wrap items-center gap-3", className)} data-ckui={marker} data-slot={slot}>
      {slot === "avatar" ? <ProfileImage value={shown} slot="avatar" marker={`${marker}-avatar`} src={preview} className="size-14 shrink-0" /> : <ProfileImage value={shown} slot="cover" marker={`${marker}-cover`} src={preview} className="aspect-[3/1] w-72 max-w-full" />}
      <div className="grid gap-2">
        <div className="flex flex-wrap gap-2">
          <Button type="button" size="sm" variant="outline" disabled={save.pending} onClick={() => input.current?.click()}>
            {save.pending ? t("entityEditor.uploading") : t(slot === "avatar" ? "entityEditor.uploadAvatar" : "entityEditor.uploadCover")}
          </Button>
          {set && (
            <Button type="button" size="sm" variant="ghost" disabled={save.pending} onClick={() => void save.run(() => update(slot === "cover" ? { cover: {} } : { avatar: {} }))}>
              {t(slot === "avatar" ? "entityEditor.removeAvatar" : "entityEditor.coverUseAuto")}
            </Button>
          )}
        </div>
        <input
          ref={input}
          type="file"
          accept="image/*"
          hidden
          aria-label={t(slot === "avatar" ? "entityEditor.uploadAvatar" : "entityEditor.uploadCover")}
          onChange={(e) => {
            const f = e.target.files?.[0];
            e.target.value = "";
            if (!f) return;
            if (!crop) return void upload(f);
            decodeImage(f).then(
              (s) => {
                setFile(f);
                setSource(s);
              },
              () => void upload(f),
            );
          }}
        />
        <ErrorLine error={save.error} />
      </div>
      <ImageCropDialog
        open={!!source}
        onOpenChange={(open) => !open && close()}
        source={source}
        aspect={slot === "avatar" ? "1:1" : "3:1"}
        aspects={slot === "avatar" ? undefined : ["3:1", "16:9", ""]}
        round={slot === "avatar"}
        onConfirm={(edit) => {
          const f = file;
          close();
          if (f) void upload(f, edit ?? undefined);
        }}
      />
    </div>
  );
}
