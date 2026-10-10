import {
  Alert02Icon,
  CropIcon,
  Delete02Icon,
  Edit02Icon,
  File01Icon,
  Image01Icon,
  ImageAdd01Icon,
  Loading03Icon,
  MusicNote01Icon,
  Refresh01Icon,
  RepeatIcon,
  Video01Icon,
} from "@hugeicons/core-free-icons";
import { HugeiconsIcon, type IconSvgElement } from "@hugeicons/react";
import { cn } from "cn";
import { useEffect, useImperativeHandle, useMemo, useRef, useState, type DragEvent, type ReactNode, type Ref } from "react";
import type { ContentKitUiAppearance } from "../appearance.js";
import type { AspectRatio } from "../client/aspect.js";
import { failureError } from "../client/errors.js";
import { isAudioType, isImageType, isSubtitleType, isVideoType } from "../client/gallery.js";
import type { FileInfo, RefBody, UploadRule } from "../client/generated/wire.js";
import { stem } from "../client/media/client.js";
import type { QueueItem } from "../client/media/queue.js";
import { acceptOf, ratioLabel } from "../client/media/rules.js";
import { isProcessing } from "../client/media/windows.js";
import { useMessages } from "../i18n/context.js";
import { megabytes, type Translator } from "../i18n/messages.js";
import { useEditorCrop } from "../react/crop.js";
import { nameIn, thumbnailOf, useMediaFolder, type FolderGroup, type MediaFolderOptions, type UseMediaFolder } from "../react/folder.js";
import { ContentKitUiRoot } from "../scope.js";
import { Alert, AlertDescription, AlertTitle } from "#ckui/ui/alert";
import { Button } from "#ckui/ui/button";
import { Progress } from "#ckui/ui/progress";
import { EncodeProgress } from "./encode-progress.js";
import { ImageCropDialog } from "./image-crop-dialog.js";
import { progressLabel, progressValue } from "./progress.js";
import { SortableList } from "./sortable-list.js";

export interface MediaFolderEditorHandle {
  /** Stops every upload and empties the queue (before deleting a draft). */
  discard: () => void;
  /** Commits the uploaded files (commit="manual"). */
  commit: () => Promise<FileInfo[]>;
  /** The folder's state and updates. */
  folder: UseMediaFolder;
}

export interface MediaFolderEditorViewProps {
  ref?: Ref<MediaFolderEditorHandle>;
  /** Heading; default "Media". */
  label?: ReactNode;
  /** Beside "Add files" in the header, e.g. a ZIP import. */
  toolbar?: ReactNode;
  /** Extra actions on each upload, beside crop, rename, replace and remove. */
  rowActions?: (file: FileInfo) => ReactNode;
  /** Below the lists, e.g. what readers without access see. */
  footer?: ReactNode;
  /** Shapes offered when cropping an image whose path has no preset shape; "" is the original's. */
  aspects?: readonly AspectRatio[];
  /** Asked before removing selected uploads; default the browser's confirm; false skips it. */
  confirmRemove?: ((count: number) => boolean | Promise<boolean>) | false;
  disabled?: boolean;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

/**
 * Either the item, and the editor opens its folder with the options; or a
 * folder the host opened (`folder={useMediaFolder(ref, options)}`), whose
 * state the host reads and renders with.
 */
export type MediaFolderEditorProps = MediaFolderEditorViewProps &
  (
    | ({
        /** The item whose folder this edits. */
        item: RefBody;
        folder?: never;
      } & MediaFolderOptions)
    | ({
        /** The host's folder (useMediaFolder); its ref may be null until the item exists. */
        folder: UseMediaFolder;
        item?: never;
      } & { [K in keyof MediaFolderOptions]?: never })
  );

const ASPECTS: readonly AspectRatio[] = ["", "1:1", "4:5", "16:9"];

type Kind = "image" | "video" | "audio" | "subtitle" | "file";

function kindOf(type: string | undefined): Kind {
  if (isImageType(type)) return "image";
  if (isVideoType(type)) return "video";
  if (isAudioType(type)) return "audio";
  if (isSubtitleType(type)) return "subtitle";
  return "file";
}

const ICONS: Record<Kind, IconSvgElement> = { image: Image01Icon, video: Video01Icon, audio: MusicNote01Icon, subtitle: File01Icon, file: File01Icon };

/** What a rule takes: one kind when every type is of it, else files. */
function ruleKind(rule: UploadRule | undefined): Kind {
  const kinds = new Set((rule?.types ?? []).map(kindOf));
  return kinds.size === 1 ? [...kinds][0]! : "file";
}

/** The rules as a sentence for the drop zone: sizes, caps and video shapes. */
export function describeRules(t: Translator["t"], rules: readonly UploadRule[]): string {
  const parts: string[] = [];
  for (const r of rules) {
    if (!r.types.length) continue;
    const what = t(`folder.kinds.${ruleKind(r)}`);
    const size = r.max_bytes > 0 ? t("folder.limitSize", { what, size: megabytes(r.max_bytes) }) : what;
    parts.push(r.max ? `${size}, ${t("folder.limitMax", { max: r.max })}` : size);
    if (r.min_aspect && r.max_aspect) parts.push(t("folder.limitAspect", { min: ratioLabel(r.min_aspect), max: ratioLabel(r.max_aspect) }));
  }
  return parts.join(" · ");
}

/**
 * An item's folder for its editors: drop or pick files (screened against the
 * kind's upload rules), a queue with progress, retry and remove, and each
 * upload path's uploads as a sortable list with thumbnail, name, edited
 * badge, encode progress and failures; crop, rename, replace and bulk
 * remove. commit="manual" adds the queue with "Add N"; "auto" commits as
 * files finish (a draft; discard() through the ref).
 */
export function MediaFolderEditor(p: MediaFolderEditorProps) {
  // Editor state (selection, crop, rename) belongs to one item.
  if (p.folder) {
    const { folder, ...view } = p;
    return <FolderView key={folder.ref ? `${folder.ref.kind}/${folder.ref.id}` : ""} folder={folder} {...view} />;
  }
  // The queue belongs to one item: another item gets a fresh editor.
  return <OwnFolder key={`${p.item.kind}/${p.item.id}`} {...p} />;
}

function OwnFolder(p: MediaFolderEditorViewProps & MediaFolderOptions & { item: RefBody }) {
  const { item, ref, label, toolbar, rowActions, footer, aspects, confirmRemove, disabled, className, appearance, ...options } = p;
  const folder = useMediaFolder(item, options);
  return <FolderView {...{ folder, ref, label, toolbar, rowActions, footer, aspects, confirmRemove, disabled, className, appearance }} />;
}

const NO_ITEM: RefBody = { kind: "", id: "" };

function FolderView(p: MediaFolderEditorViewProps & { folder: UseMediaFolder }) {
  const { folder, ref, label, toolbar, rowActions, footer, aspects = ASPECTS, confirmRemove, disabled, className, appearance } = p;
  const { t, error: errorText } = useMessages();
  const { groups, queue, read, options } = folder;
  useImperativeHandle(ref, () => ({ discard: folder.discard, commit: folder.commit, folder }), [folder]);
  const auto = options.commit === "auto";
  const rules = groups.flatMap((g) => (g.rule ? [g.rule] : []));
  const accept = acceptOf(rules);
  const count = folder.uploads.length + queue.items.length + folder.waiting.length;
  const empty = count === 0;
  const off = !!disabled;

  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  useEffect(() => {
    const paths = new Set(folder.uploads.map((f) => f.path));
    setSelected((s) => ([...s].every((x) => paths.has(x)) ? s : new Set([...s].filter((x) => paths.has(x)))));
  }, [folder.uploads]);
  const [cropping, setCropping] = useState<string | null>(null);
  const crop = useEditorCrop(folder.ref ?? NO_ITEM, cropping, { client: options.client, onError: options.onError, onSaved: () => setCropping(null) });
  const cropRule = cropping ? groups.find((g) => g.files.some((f) => f.path === cropping))?.rule : undefined;
  const [renaming, setRenaming] = useState<string | null>(null);
  const replaceInput = useRef<HTMLInputElement>(null);
  const [replacing, setReplacing] = useState<string | null>(null);
  const [over, setOver] = useState(false);

  const drop = {
    onDragOver: (e: DragEvent) => {
      if (off || !e.dataTransfer.types.includes("Files")) return;
      e.preventDefault();
      setOver(true);
    },
    onDragLeave: (e: DragEvent) => {
      if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setOver(false);
    },
    onDrop: (e: DragEvent) => {
      if (off || !e.dataTransfer.files.length) return;
      e.preventDefault();
      setOver(false);
      folder.add(e.dataTransfer.files);
    },
  };

  const removeSelected = async () => {
    const paths = [...selected];
    const ok = confirmRemove === false ? true : confirmRemove ? await confirmRemove(paths.length) : globalThis.confirm?.(t("folder.confirmRemove", { count: paths.length })) ?? true;
    if (!ok) return;
    await folder.remove(paths).then(() => setSelected(new Set()), () => {});
  };

  const uploaded = queue.items.filter((i) => i.status === "uploaded").length;
  const picker = (big: boolean) => (
    <label
      className={cn(
        big
          ? "flex cursor-pointer flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground transition-colors hover:border-primary hover:bg-primary/5"
          : "inline-flex h-8 cursor-pointer items-center gap-1.5 rounded-md border border-border bg-background px-2.5 text-sm font-medium transition-colors hover:bg-muted focus-within:ring-3 focus-within:ring-ring/50",
        big && over && "border-primary bg-primary/5",
        off && "pointer-events-none opacity-50",
      )}
      data-ckui={big ? "folder-drop" : "folder-add"}
    >
      <HugeiconsIcon icon={ImageAdd01Icon} strokeWidth={1.75} className={big ? "size-7" : "size-4"} />
      {big ? <strong className="font-medium text-foreground">{t("folder.drop")}</strong> : t("folder.add")}
      {big && rules.length > 0 && <span className="text-xs">{describeRules(t, rules)}</span>}
      <input
        type="file"
        multiple
        accept={accept}
        className="sr-only"
        disabled={off}
        data-ckui="file"
        onChange={(e) => {
          folder.add([...(e.target.files ?? [])]);
          e.target.value = "";
        }}
      />
    </label>
  );

  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-3", className)} data-ckui="media-folder" data-over={over ? "" : undefined} {...drop}>
      <div className="flex flex-wrap items-center justify-between gap-2" data-ckui="folder-header">
        <h3 className="text-sm font-semibold">{label ?? t("folder.label")}</h3>
        <div className="flex flex-wrap items-center gap-2">
          {toolbar}
          {!empty && picker(false)}
        </div>
      </div>
      {empty && (read.read || !folder.ref) && picker(true)}
      {empty && !read.read && read.loading && (
        <div className="flex justify-center py-6 text-muted-foreground" data-ckui="folder-loading">
          <HugeiconsIcon icon={Loading03Icon} strokeWidth={2} className="size-5 motion-safe:animate-spin" />
        </div>
      )}

      {folder.refused.length > 0 && (
        <Alert variant="destructive" role="alert" data-ckui="folder-refused">
          <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} />
          <AlertDescription>
            <ul className="grid gap-0.5">
              {folder.refused.map((r, i) => (
                <li key={i}>
                  <span className="font-medium">{r.file.name}</span>: {errorText(r.error)}
                </li>
              ))}
            </ul>
            <Button variant="outline" size="xs" className="mt-1 w-fit" onClick={folder.dismiss}>
              {t("folder.dismiss")}
            </Button>
          </AlertDescription>
        </Alert>
      )}
      {queue.blocked && (
        <Alert variant="destructive" role="alert" data-ckui="folder-paused">
          <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} />
          <AlertTitle>{t("folder.paused")}</AlertTitle>
          <AlertDescription>
            {errorText(queue.blocked)}
            <Button variant="outline" size="xs" className="mt-1 w-fit" onClick={() => queue.start()}>
              {t("folder.resume")}
            </Button>
          </AlertDescription>
        </Alert>
      )}
      {folder.error && (
        <Alert variant="destructive" role="alert" data-ckui="folder-error">
          <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} />
          <AlertDescription>{errorText(folder.error)}</AlertDescription>
        </Alert>
      )}

      {selected.size > 0 && (
        <div className="flex flex-wrap items-center gap-2 rounded-md bg-muted px-3 py-1.5 text-sm" data-ckui="folder-selection">
          <span className="tabular-nums">{t("folder.selected", { count: selected.size })}</span>
          <Button variant="destructive" size="xs" disabled={off || folder.updating} onClick={() => void removeSelected()}>
            <HugeiconsIcon icon={Delete02Icon} strokeWidth={2} />
            {t("folder.removeSelected")}
          </Button>
          <Button variant="ghost" size="xs" onClick={() => setSelected(new Set())}>
            {t("folder.clearSelection")}
          </Button>
        </div>
      )}

      {groups.map((g) =>
        g.files.length ? (
          <Group
            key={g.path}
            group={g}
            titled={groups.length > 1}
            folder={folder}
            disabled={off}
            selected={selected}
            onSelect={(path, on) => setSelected((s) => (on ? new Set([...s, path]) : new Set([...s].filter((x) => x !== path))))}
            renaming={renaming}
            onRename={setRenaming}
            onCrop={setCropping}
            onReplace={(path) => {
              setReplacing(path);
              replaceInput.current?.click();
            }}
            rowActions={rowActions}
          />
        ) : null,
      )}

      {folder.waiting.length > 0 && (
        <ul className="grid" aria-label={t("folder.label")}>
          {folder.waiting.map((f, i) => (
            <li key={i} className="flex min-h-11 items-center gap-2 px-1" data-ckui="queue-row" data-status="waiting">
              <Thumb kind={kindOf(f.type)} />
              <div className="grid min-w-0 flex-1 gap-1">
                <span className="truncate text-sm">{f.name}</span>
                <span className="text-xs text-muted-foreground" data-ckui="queue-status">
                  {t("folder.queued")}
                </span>
              </div>
            </li>
          ))}
        </ul>
      )}

      {queue.items.length > 0 && (
        <SortableList
          items={queue.items}
          id={(i) => i.id}
          name={(i) => i.file.name}
          disabled={auto || off}
          label={t("folder.label")}
          onMove={(from, to) => queue.move(queue.items[from]!.id, to)}
          row={(i) => ({ className: "flex min-h-11 items-center gap-2 px-1", "data-ckui": "queue-row", "data-status": i.status })}
        >
          {(i, handle) => <QueueRow item={i} handle={auto ? null : handle} auto={auto} folder={folder} />}
        </SortableList>
      )}

      {((!auto && uploaded > 0) || (auto && folder.error && uploaded > 0)) && (
        <Button className="w-fit" disabled={off || !queue.ready || folder.committing} onClick={() => void folder.commit().catch(() => {})} data-ckui="folder-commit">
          {folder.committing && <HugeiconsIcon icon={Loading03Icon} strokeWidth={2} className="motion-safe:animate-spin" />}
          {auto ? t("folder.commitAgain") : t("folder.commit", { count: queue.items.filter((i) => i.status !== "committed").length })}
        </Button>
      )}
      {footer}

      <input
        ref={replaceInput}
        type="file"
        accept={accept}
        className="sr-only"
        tabIndex={-1}
        aria-hidden
        onChange={(e) => {
          const f = e.target.files?.[0];
          e.target.value = "";
          if (f && replacing) void folder.replace(replacing, f).catch(() => {});
          setReplacing(null);
        }}
      />
      <ImageCropDialog
        open={!!cropping}
        onOpenChange={(o) => !o && crop.status !== "saving" && setCropping(null)}
        source={"source" in crop ? (crop.source ?? null) : null}
        aspect={cropRule?.aspect ?? ""}
        aspects={cropRule?.aspect ? undefined : aspects}
        initialEdit={"edit" in crop ? crop.edit : null}
        minWidth={cropRule?.min_width}
        title={t("folder.crop")}
        busy={crop.status === "saving"}
        error={crop.status === "error" ? errorText(crop.error) : undefined}
        onConfirm={(edit) => void crop.save(edit)}
      />
    </ContentKitUiRoot>
  );
}

function Group({
  group,
  titled,
  folder,
  disabled,
  selected,
  onSelect,
  renaming,
  onRename,
  onCrop,
  onReplace,
  rowActions,
}: {
  group: FolderGroup;
  titled: boolean;
  folder: UseMediaFolder;
  disabled: boolean;
  selected: ReadonlySet<string>;
  onSelect: (path: string, on: boolean) => void;
  renaming: string | null;
  onRename: (path: string | null) => void;
  onCrop: (path: string) => void;
  onReplace: (path: string) => void;
  rowActions?: (file: FileInfo) => ReactNode;
}) {
  const { t } = useMessages();
  const kind = ruleKind(group.rule);
  const title = t(`folder.kinds.${kind}`);
  const max = group.rule?.max;
  return (
    <section className="grid gap-1" data-ckui="folder-group" data-path={group.path}>
      {titled && (
        <h4 className="px-1 text-xs font-medium text-muted-foreground tabular-nums">
          {title} · {max ? `${group.files.length} / ${max}` : group.files.length}
        </h4>
      )}
      <SortableList
        items={group.files}
        id={(f) => f.path}
        name={(f) => nameIn(group, f.path)}
        disabled={disabled || folder.updating}
        label={title}
        onMove={(from, to) => void folder.move(group.files[from]!.path, to).catch(() => {})}
        row={(f) => ({ className: "flex min-h-12 items-center gap-2 px-1", "data-ckui": "upload-row", "data-path": f.path })}
      >
        {(f, handle) => (
          <UploadRow
            file={f}
            group={group}
            handle={handle}
            folder={folder}
            disabled={disabled}
            selected={selected.has(f.path)}
            onSelect={(on) => onSelect(f.path, on)}
            renaming={renaming === f.path}
            onRename={onRename}
            onCrop={onCrop}
            onReplace={onReplace}
            actions={rowActions?.(f)}
          />
        )}
      </SortableList>
    </section>
  );
}

function Thumb({ kind, url, busy }: { kind: Kind; url?: string; busy?: boolean }) {
  return (
    <span className="relative flex size-10 shrink-0 items-center justify-center overflow-hidden rounded-md bg-muted text-muted-foreground" data-ckui="thumb">
      {url ? <img src={url} alt="" loading="lazy" decoding="async" className="size-full object-cover" /> : <HugeiconsIcon icon={ICONS[kind]} strokeWidth={1.75} className="size-4.5" />}
      {busy && !url && <span aria-hidden className="absolute inset-1.5 rounded-full border-2 border-current/25 border-t-current motion-safe:animate-spin" />}
    </span>
  );
}

function UploadRow({
  file: f,
  group,
  handle,
  folder,
  disabled,
  selected,
  onSelect,
  renaming,
  onRename,
  onCrop,
  onReplace,
  actions,
}: {
  file: FileInfo;
  group: FolderGroup;
  handle: ReactNode;
  folder: UseMediaFolder;
  disabled: boolean;
  selected: boolean;
  onSelect: (on: boolean) => void;
  renaming: boolean;
  onRename: (path: string | null) => void;
  onCrop: (path: string) => void;
  onReplace: (path: string) => void;
  actions?: ReactNode;
}) {
  const { t, error: errorText } = useMessages();
  const kind = kindOf(f.type);
  const name = nameIn(group, f.path);
  const busy = isProcessing(f);
  const thumb = useMemo(() => thumbnailOf(folder.read.read, f), [folder.read.read, f]);
  const locked = disabled || folder.updating;
  const failure = f.failed ? errorText(failureError(f.failed)) : undefined;
  return (
    <>
      {handle}
      <input
        type="checkbox"
        className="size-4 shrink-0 accent-primary"
        checked={selected}
        disabled={disabled}
        aria-label={t("folder.select", { name })}
        onChange={(e) => onSelect(e.target.checked)}
      />
      <Thumb kind={kind} url={thumb?.url} busy={busy} />
      <div className="grid min-w-0 flex-1 gap-1">
        <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
          {renaming ? (
            <RenameInput name={stem(name)} label={t("folder.renameLabel", { name })} onDone={(to) => (to === null ? onRename(null) : folder.rename(f.path, to).then(() => onRename(null)))} />
          ) : (
            <span className="max-w-full truncate text-sm" title={f.path} data-ckui="upload-name">
              {name}
            </span>
          )}
          {f.edit && (
            <span className="shrink-0 rounded-sm border border-border px-1.5 text-[11px] text-muted-foreground" data-ckui="edited">
              {t("folder.edited")}
            </span>
          )}
          {failure && (
            <span className="shrink-0 rounded-sm bg-destructive/10 px-1.5 text-[11px] font-medium text-destructive" title={failure} data-ckui="failed">
              {kind === "video" ? t("folder.failedVideo") : t("folder.failed")}
            </span>
          )}
        </div>
        {failure && <p className="text-xs text-destructive">{failure}</p>}
        {busy && kind === "video" && <EncodeProgress progress={f.progress} className="max-w-sm text-xs" />}
        {busy && kind !== "video" && <span className="text-xs text-muted-foreground">{t("folder.processing")}</span>}
      </div>
      <span className="flex shrink-0 items-center gap-0.5" data-ckui="upload-actions">
        {actions}
        {kind === "image" && (
          <Button variant="ghost" size="icon-sm" aria-label={t("folder.crop")} title={t("folder.crop")} disabled={locked || !!f.staged || !f.size} onClick={() => onCrop(f.path)}>
            <HugeiconsIcon icon={CropIcon} strokeWidth={2} />
          </Button>
        )}
        {!group.rule?.named && (
          <Button variant="ghost" size="icon-sm" aria-label={t("common.rename")} title={t("common.rename")} disabled={locked} onClick={() => onRename(f.path)}>
            <HugeiconsIcon icon={Edit02Icon} strokeWidth={2} />
          </Button>
        )}
        <Button variant="ghost" size="icon-sm" aria-label={t("common.replace")} title={t("common.replace")} disabled={locked} onClick={() => onReplace(f.path)}>
          <HugeiconsIcon icon={RepeatIcon} strokeWidth={2} />
        </Button>
        <Button variant="ghost" size="icon-sm" aria-label={t("common.remove")} title={t("common.remove")} disabled={locked} onClick={() => void folder.remove([f.path]).catch(() => {})}>
          <HugeiconsIcon icon={Delete02Icon} strokeWidth={2} />
        </Button>
      </span>
    </>
  );
}

function RenameInput({ name, label, onDone }: { name: string; label: string; onDone: (to: string | null) => unknown }) {
  const [value, setValue] = useState(name);
  const done = useRef(false);
  const finish = (to: string | null) => {
    if (done.current) return;
    done.current = true;
    Promise.resolve(onDone(to && to.trim() && to.trim() !== name ? to.trim() : null)).catch(() => (done.current = false));
  };
  return (
    <input
      autoFocus
      aria-label={label}
      value={value}
      className="h-7 min-w-0 flex-1 rounded-md border border-input bg-background px-2 text-sm outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
      onChange={(e) => setValue(e.target.value)}
      onKeyDown={(e) => {
        if (e.key === "Enter") {
          e.preventDefault();
          finish(value);
        } else if (e.key === "Escape") {
          e.preventDefault();
          finish(null);
        }
      }}
      onBlur={() => finish(value)}
      data-ckui="rename"
    />
  );
}

function queueLabel(t: Translator["t"], errorText: Translator["error"], i: QueueItem, auto: boolean): string {
  if (i.status === "failed") return errorText(i.error);
  if (i.status === "uploading") return progressLabel(t, i.progress);
  if (i.status === "uploaded") {
    const p = i.processing;
    if (p?.failed) return errorText(failureError(p.failed));
    if (i.unattached && !i.processed) return p?.progress && p.progress.phase !== "queued" ? t("folder.processingPercent", { percent: Math.round(p.progress.percent) }) : t("folder.processing");
    return auto ? t("folder.adding") : t("folder.uploaded");
  }
  return i.status === "committed" ? t("folder.adding") : t("folder.queued");
}

function QueueRow({ item: i, handle, auto, folder }: { item: QueueItem; handle: ReactNode; auto: boolean; folder: UseMediaFolder }) {
  const { t, error: errorText } = useMessages();
  const { queue } = folder;
  const kind = kindOf(i.file.type);
  const label = queueLabel(t, errorText, i, auto);
  const value = i.status === "uploading" ? progressValue(i.progress) : i.unattached && !i.processed && i.processing?.progress ? i.processing.progress.percent : null;
  return (
    <>
      {handle}
      <Thumb kind={kind} busy={i.status === "uploading" || (!!i.unattached && !i.processed)} />
      <div className="grid min-w-0 flex-1 gap-1">
        <span className="truncate text-sm" title={i.path}>
          {i.file.name}
        </span>
        {value !== null && <Progress value={value} aria-label={label} className="max-w-sm" />}
        <span className={cn("text-xs", i.status === "failed" ? "text-destructive" : "text-muted-foreground")} data-ckui="queue-status">
          {label}
        </span>
      </div>
      <span className="flex shrink-0 items-center gap-0.5">
        {i.status === "failed" && (
          <Button variant="ghost" size="icon-sm" aria-label={t("common.retry")} title={t("common.retry")} onClick={() => queue.retry(i.id)}>
            <HugeiconsIcon icon={Refresh01Icon} strokeWidth={2} />
          </Button>
        )}
        <Button variant="ghost" size="icon-sm" aria-label={t("common.remove")} title={t("common.remove")} onClick={() => queue.remove(i.id)}>
          <HugeiconsIcon icon={Delete02Icon} strokeWidth={2} />
        </Button>
      </span>
    </>
  );
}
