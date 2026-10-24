import { useMemo } from "react";
import type { ContentKitClient } from "../client/client.js";
import type { FileInfo, RefBody } from "../client/generated/wire.js";
import { isImageType } from "../client/gallery.js";
import { ruleFor } from "../client/media/rules.js";
import { useMessages } from "../i18n/context.js";
import { useMediaRead } from "../react/read.js";
import { MediaReadinessNotice } from "./media-readiness-notice.js";

export interface MediaProcessingPanelProps {
  item: RefBody;
  client?: ContentKitClient;
}

/** Current processing counts and a bounded preview of actual completed images.
 * Both reads recover from the server on reopen; upload queue memory is irrelevant.
 */
export function MediaProcessingPanel({ item, client }: MediaProcessingPanelProps) {
  const { t } = useMessages();
  const status = useMediaRead(item, { editor: true, status: true, client });
  const metadata = useMediaRead(item, { editor: true, limit: 1, client });
  const previews = useMemo(() => {
    const read = metadata.read;
    const sources = (read?.files ?? []).filter((f) => f.upload && !f.unattached && isImageType(f.type));
    const pages = sources.filter((f) => ruleFor(read?.uploads ?? [], f.path)?.page);
    return (pages.length ? pages : sources).slice(0, 12).map((source) => {
      // Choose a completed derivative, never an original or a browser blob URL.
      let thumbnail: FileInfo | undefined;
      for (const file of read?.files ?? []) {
        if (file.upload || file.from !== source.path || !isImageType(file.type)) continue;
        if (!thumbnail || (file.w ?? Infinity) < (thumbnail.w ?? Infinity)) thumbnail = file;
      }
      return { source, path: thumbnail?.path };
    });
  }, [metadata.read]);
  const paths = previews.flatMap((preview) => preview.path ? [preview.path] : []);
  const images = useMediaRead(item, { editor: true, paths, client });
  const state = status.read?.status?.state;
  return <div className="grid gap-3" data-ckui="media-processing" data-state={state}>
    <MediaReadinessNotice item={item} client={client} />
    {state === "ready" && <p role="status" className="text-sm text-muted-foreground">{t("readiness.ready")}</p>}
    {previews.length > 0 && <div className="grid grid-cols-4 gap-2 sm:grid-cols-6" aria-label={t("readiness.previews")}>
      {previews.map(({ source, path }) => {
        const image = images.read?.files.find((file) => file.path === path && file.url);
        const name = source.path.split("/").at(-1) ?? "";
        return <div key={source.path} data-ckui="media-progress-preview" data-state={image ? "ready" : "pending"} className="aspect-[3/4] overflow-hidden rounded-md bg-muted">
          {image ? <img src={image.url} alt={name} loading="lazy" className="size-full object-cover" /> : <span role="img" aria-label={name} className="block size-full motion-safe:animate-pulse" />}
        </div>;
      })}
    </div>}
  </div>;
}
