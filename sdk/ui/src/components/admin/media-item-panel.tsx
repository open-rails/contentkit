import { useId, useRef, useState } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import type { FileInfo, RefBody, UploadRule } from "../../client/generated/wire.js";
import { isSubtitleType, isVideoType } from "../../client/gallery.js";
import { acceptOf, isPattern } from "../../client/media/rules.js";
import type { Progress } from "../../client/media/client.js";
import { samePath } from "../../client/media/client.js";
import { isProcessing } from "../../client/media/windows.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import { useFlowClient } from "../../react/admin.js";
import { useContentKitClient, useErrorReporter, useGuarded, type ContentKitErrorHandler } from "../../react/context.js";
import { usePublicImage } from "../../react/public.js";
import { useMediaRead } from "../../react/read.js";
import { Alert, AlertDescription } from "#ckui/ui/alert";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Switch } from "#ckui/ui/switch";
import { EncodeProgress } from "../encode-progress.js";
import { SlotImage } from "../slot-image.js";
import { VideoPosterPicker } from "../video-poster-picker.js";
import { ErrorLine, useAction } from "../social/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Rows, useConfirm, type AdminFlowProps } from "./shell.js";

type Choice = string | { value: string; label: string };

interface VideoPaths {
  /** The video's upload path. Default the kind's literal video upload. */
  source?: string;
  /** The subtitle uploads' path pattern; null hides them. Default the kind's subtitle pattern. */
  subtitles?: string | null;
  /** The poster's upload path (its frames come from the video); null hides it. Default the kind's upload framed from the video. */
  poster?: string | null;
  /** The poster's public preset. Default "poster". */
  posterPreset?: string;
  /** Subtitle languages offered: codes, or codes with labels. Without them the language is typed. */
  languages?: readonly Choice[];
}

export interface MediaItemPanelProps extends AdminFlowProps, VideoPaths {
  /** The video item. */
  item: RefBody;
}

/** A video item's upload paths from its kind's rules: the literal video upload, the image upload framed from it, a subtitle pattern. */
export function videoPaths(rules: readonly UploadRule[]): { source?: string; poster?: string; subtitles?: string } {
  const only = (r: UploadRule, is: (t: string) => boolean) => r.types.length > 0 && r.types.every(is);
  const source = rules.find((r) => !isPattern(r.path) && only(r, isVideoType))?.path;
  return {
    source,
    poster: source ? rules.find((r) => !isPattern(r.path) && r.frames === source)?.path : undefined,
    subtitles: rules.find((r) => isPattern(r.path) && only(r, isSubtitleType))?.path,
  };
}

type ItemState = "ready" | "processing" | "failed" | "empty";
const choices = (list?: readonly Choice[], name: (code: string) => string = (c) => c) => (list ?? []).map((c) => (typeof c === "string" ? { value: c, label: name(c) } : c));
const under = (pattern: string) => pattern.slice(0, pattern.indexOf("{name}"));

/**
 * One video item for its editors: its encode state and progress, the source
 * (replace it), subtitle sidecars (language, label, forced), the poster from
 * a frame or an image, and regenerate, which retries failed uploads.
 */
export function MediaItemPanel({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: MediaItemPanelProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="media-item-panel" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <VideoMediaEditor {...p} client={client} />
    </AdminRoot>
  );
}

export interface VideoMediaEditorProps extends VideoPaths {
  item: RefBody;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
}

/** MediaItemPanel's body, for a host's own editor (the content manager's version media). */
export function VideoMediaEditor({ item, client: own, source: givenSource, subtitles: givenSubtitles, poster: givenPoster, posterPreset = "poster", languages, onError }: VideoMediaEditorProps) {
  const m = useMessages();
  const { t } = m;
  const client = useContentKitClient(own);
  const r = useMediaRead(item, { editor: true, client });
  const regen = useAction("media.save", onError);
  if (!r.read) {
    if (r.error?.code === "not_found") return <NoAccess>{t("mediaPanel.noAccess")}</NoAccess>;
    return r.error ? <LoadFailed error={r.error} message={t("mediaPanel.loadFailed")} retry={r.reload} /> : <Rows count={2} />;
  }
  if (!r.read.uploads) return <NoAccess>{t("mediaPanel.noAccess")}</NoAccess>;
  const rules = r.read.uploads;
  const derived = videoPaths(rules);
  const source = givenSource ?? derived.source;
  const poster = givenPoster === undefined ? derived.poster : givenPoster;
  const subtitles = givenSubtitles === undefined ? derived.subtitles : givenSubtitles;
  const accept = (path: string) => acceptOf(rules.filter((x) => x.path === path));
  const uploads = r.read.files.filter((f) => f.upload && !f.unattached);
  const video = source ? uploads.find((f) => samePath(f.path, source)) : undefined;
  const failed = uploads.filter((f) => f.failed);
  const state: ItemState = !video ? "empty" : failed.length ? "failed" : uploads.some(isProcessing) ? "processing" : "ready";
  const subs = subtitles ? uploads.filter((f) => f.path.startsWith(under(subtitles))) : [];
  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="text-base font-semibold">{t("mediaPanel.title")}</h2>
        <Badge variant={state === "failed" ? "destructive" : state === "ready" ? "outline" : "secondary"} data-ckui="media-state" data-state={state}>
          {t(`mediaPanel.${state}`)}
        </Badge>
        <Button size="sm" variant="outline" className="ms-auto" disabled={regen.pending || !uploads.length} onClick={() => void regen.run(() => client.media.commit(item, [{ op: "regenerate" }]))}>
          {t("mediaPanel.regenerate")}
        </Button>
      </div>
      <ErrorLine error={regen.error} />
      {failed.map((f) => (
        <Alert key={f.path} variant="destructive" data-ckui="media-failure">
          <AlertDescription>
            {t("mediaPanel.failedFile", { name: f.path.split("/").pop()!, reason: m.error({ code: f.failed!.code, message: f.failed!.message, status: 422 }) })}
          </AlertDescription>
        </Alert>
      ))}
      {source && <SourceSection item={item} path={source} accept={accept(source) ?? "video/*"} video={video} client={client} onError={onError} />}
      {source && poster && video && <PosterSection item={item} path={poster} video={source} preset={posterPreset} client={client} onError={onError} />}
      {subtitles && <Subtitles item={item} pattern={subtitles} accept={accept(subtitles)} files={subs} languages={languages} client={client} onError={onError} />}
    </>
  );
}

type Inner = { client: ContentKitClient; onError?: ContentKitErrorHandler };

function bytes(n?: number): string {
  if (!n) return "";
  const mb = n / (1 << 20);
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb.toFixed(mb < 10 ? 1 : 0)} MB`;
}

function duration(s?: number): string {
  if (!s) return "";
  const m = Math.floor(s / 60);
  const sec = Math.round(s - m * 60);
  return `${m}:${String(sec).padStart(2, "0")}`;
}

function SourceSection({ item, path, accept, video, client, onError }: { item: RefBody; path: string; accept: string; video?: FileInfo } & Inner) {
  const { t } = useMessages();
  const file = useRef<HTMLInputElement>(null);
  const [progress, setProgress] = useState<Progress | null>(null);
  const report = useErrorReporter(onError);
  const guarded = useGuarded();
  const [error, setError] = useState<ContentKitError | null>(null);
  const replace = async (f: File) => {
    setError(null);
    try {
      await guarded(() => client.media.put(f, { ref: item, path, wait: false, onProgress: setProgress }));
    } catch (e) {
      const err = toContentKitError(e);
      setError(err);
      report(err, "media.save");
    } finally {
      setProgress(null);
    }
  };
  const facts = video ? [video.w && video.h ? `${video.w}×${video.h}` : "", duration(video.dur), bytes(video.size)].filter(Boolean).join(" · ") : "";
  return (
    <section className="grid gap-2" data-ckui="media-source">
      <h3 className="font-medium">{t("mediaPanel.source")}</h3>
      <div className="flex flex-wrap items-center gap-3">
        <span className="min-w-0 flex-1 text-muted-foreground">{video ? facts || video.path : t("mediaPanel.noSource")}</span>
        <Button size="sm" variant="outline" disabled={!!progress} onClick={() => file.current?.click()}>
          {t(video ? "mediaPanel.replace" : "mediaPanel.upload")}
        </Button>
        <input
          ref={file}
          type="file"
          accept={accept}
          hidden
          aria-label={t("mediaPanel.source")}
          onChange={(e) => {
            const f = e.target.files?.[0];
            e.target.value = "";
            if (f) void replace(f);
          }}
        />
      </div>
      {progress && (
        <div className="grid gap-1" role="status">
          <div className="h-1.5 overflow-hidden rounded-full bg-muted">
            <div className="h-full bg-primary transition-[width]" style={{ width: `${progress.total ? Math.round((progress.loaded / progress.total) * 100) : 0}%` }} />
          </div>
          <span className="text-xs text-muted-foreground">{t(`mediaPanel.phase_${progress.phase}`)}</span>
        </div>
      )}
      {video && isProcessing(video) && !progress && <EncodeProgress progress={video.progress} />}
      <ErrorLine error={error} />
    </section>
  );
}

function PosterSection({ item, path, video, preset, client, onError }: { item: RefBody; path: string; video: string; preset: string } & Inner) {
  const { t } = useMessages();
  const [open, setOpen] = useState(false);
  const { image, reload } = usePublicImage(item, preset, { client });
  return (
    <section className="grid gap-2" data-ckui="media-poster">
      <h3 className="font-medium">{t("mediaPanel.poster")}</h3>
      <div className="flex flex-wrap items-center gap-3">
        <div className="w-40 overflow-hidden rounded-md border border-border">
          <SlotImage image={image} fallback={<div className="aspect-video bg-muted" />} />
        </div>
        <Button size="sm" variant="outline" onClick={() => setOpen(true)}>
          {t("mediaPanel.choosePoster")}
        </Button>
      </div>
      <VideoPosterPicker open={open} onOpenChange={setOpen} item={item} video={video} path={path} image={image} client={client} onError={onError} onChange={() => reload()} />
    </section>
  );
}

function Subtitles({ item, pattern, accept, files, languages: offered, client, onError }: { item: RefBody; pattern: string; accept?: string; files: FileInfo[]; languages?: readonly Choice[] } & Inner) {
  const { t } = useMessages();
  const ids = useId();
  const languageName = useLanguageName();
  const languages = choices(offered, languageName);
  const [lang, setLang] = useState(languages[0]?.value ?? "");
  const [label, setLabel] = useState("");
  const [forced, setForced] = useState(false);
  const [file, setFile] = useState<File | null>(null);
  const input = useRef<HTMLInputElement>(null);
  const add = useAction("media.save", onError);
  const remove = useAction("media.save", onError);
  const { confirm, dialog } = useConfirm();
  const save = async () => {
    if (!file || !lang.trim()) return;
    const code = lang.trim();
    const name = `${code}${forced ? "-forced" : ""}`;
    const done = await add.run(() =>
      client.media.put(file, {
        ref: item,
        path: pattern.replace("{name}", name),
        wait: false,
        meta: { lang: code, label: label.trim() || (languages.find((l) => l.value === code)?.label ?? code), forced },
      }),
    );
    if (done) {
      setFile(null);
      if (input.current) input.current.value = "";
      setLabel("");
      setForced(false);
    }
  };
  const drop = async (f: FileInfo) => {
    if (await confirm({ title: t("mediaPanel.removeSubtitleTitle", { name: String(f.meta?.label ?? f.path) }), confirm: t("common.remove") })) {
      await remove.run(() => client.media.commit(item, [{ op: "remove", path: f.path }]));
    }
  };
  return (
    <section className="grid gap-2" data-ckui="media-subtitles">
      <h3 className="font-medium">{t("mediaPanel.subtitles")}</h3>
      {files.length ? (
        <ul className="grid divide-y divide-border rounded-lg border border-border">
          {files.map((f) => (
            <li key={f.path} className="flex flex-wrap items-center gap-2 p-2.5" data-ckui="subtitle">
              <Badge variant="outline">{String(f.meta?.lang ?? "?")}</Badge>
              <span className="min-w-0 flex-1 break-words">{String(f.meta?.label ?? f.path.split("/").pop())}</span>
              {f.meta?.forced === true && <Badge variant="secondary">{t("mediaPanel.forced")}</Badge>}
              {f.failed && <Badge variant="destructive">{t("mediaPanel.failed")}</Badge>}
              <Button size="xs" variant="ghost" disabled={remove.pending} onClick={() => void drop(f)}>
                {t("common.remove")}
              </Button>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-muted-foreground">{t("mediaPanel.noSubtitles")}</p>
      )}
      <form
        className="grid gap-2 rounded-lg border border-dashed border-border p-3 sm:grid-cols-2"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-file`}>{t("mediaPanel.subtitleFile")}</Label>
          <Input id={`${ids}-file`} ref={input} type="file" accept={accept ?? ".vtt,.srt,text/vtt,application/x-subrip"} onChange={(e) => setFile(e.target.files?.[0] ?? null)} />
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-lang`}>{t("mediaPanel.language")}</Label>
          {languages.length ? (
            <Select value={lang} onValueChange={(v) => setLang(String(v))}>
              <SelectTrigger id={`${ids}-lang`} className="w-full">
                <SelectValue>{(v: string) => languages.find((l) => l.value === v)?.label ?? v}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {languages.map((l) => (
                  <SelectItem key={l.value} value={l.value}>
                    {l.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : (
            <Input id={`${ids}-lang`} value={lang} placeholder="en" onChange={(e) => setLang(e.target.value)} />
          )}
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-label`}>{t("mediaPanel.label")}</Label>
          <Input id={`${ids}-label`} value={label} placeholder={t("mediaPanel.labelPlaceholder")} onChange={(e) => setLabel(e.target.value)} />
        </div>
        <div className="flex items-center gap-2 self-end">
          <Switch id={`${ids}-forced`} checked={forced} onCheckedChange={setForced} />
          <Label htmlFor={`${ids}-forced`}>{t("mediaPanel.forcedLabel")}</Label>
        </div>
        <ErrorLine error={add.error} className="sm:col-span-2" />
        <Button type="submit" size="sm" className="justify-self-start" disabled={add.pending || !file || !lang.trim()}>
          {add.pending ? t("common.saving") : t("mediaPanel.addSubtitle")}
        </Button>
      </form>
      <ErrorLine error={remove.error} />
      {dialog}
    </section>
  );
}
