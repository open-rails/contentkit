import { cn } from "cn";
import type { Avatar, EntitiesCover } from "../client/generated/wire.js";
import { useScopeProps } from "../scope.js";
import { RenditionImg } from "./rendition-img.js";

export interface ProfileImageValue { name: string; slug: string; cover?: EntitiesCover; avatar?: Avatar }
export interface ProfileImageProps { value: ProfileImageValue; src?: string; className?: string; slot: "cover" | "avatar"; marker: string }

/** Shared image rendering; channel ownership and metadata associations stay outside this visual. */
export function ProfileImage({ value, src, className, slot, marker }: ProfileImageProps) {
  const scope = useScopeProps();
  const box = cn(scope.className, "overflow-hidden bg-muted", slot === "avatar" ? "aspect-square rounded-full" : "rounded-md", className);
  const root = { "data-ckui-theme": scope["data-ckui-theme"], style: scope.style, "data-ckui": marker };
  const initial = <span {...root} className={cn(box, "grid place-items-center font-semibold text-muted-foreground", slot === "cover" && "text-lg")}>
    {(value.name || value.slug).trim().charAt(0).toUpperCase() || "?"}
  </span>;
  if (src) return <img {...root} src={src} alt="" className={cn(box, "object-cover")} />;
  const renditions = value[slot]?.renditions;
  return renditions?.length ? <RenditionImg {...root} outputs={renditions} fit="cover" skeleton className={box} fallback={initial} /> : initial;
}
