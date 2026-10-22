import { cn } from "cn";
import { useContext, useId, useMemo, useRef, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { Anchor, BackgroundCue, ForegroundCue, ReaderMeta } from "../../client/generated/wire.js";
import { emptyReaderMeta, hashPageNames, matchAudio, readerMetaProblems, type AudioMatch, type ReaderMetaProblem, type ReaderNames } from "../../client/reader-meta.js";
import { useMessages } from "../../i18n/context.js";
import { useContentKitClient, type ContentKitErrorHandler } from "../../react/context.js";
import { useMediaRead } from "../../react/read.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Switch } from "#ckui/ui/switch";
import { ErrorLine, useAction } from "../social/parts.js";
import { EditorSaveContext, useEditorSaveTask } from "../content/editor-save.js";
import { useDraft } from "../../react/use-draft.js";
import { LoadFailed, Rows } from "./shell.js";
import { ALL_EDITOR_FILES, AudioPreview, editorMedia, PagePicker, type PlanMedia } from "./page-picker.js";

export interface ReaderManifestEditorProps {
  /** The content's kind, which addresses the version's media item. */
  kind: string;
  content: string;
  version: string;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
  /** Show its title; off where the host's section already names it. Default true. */
  heading?: boolean;
}

const NONE = "__none__";

/** A page (its source hash) or an anchor ("anchor:id") as one picker value. */
const placeOf = (page?: string, anchor?: string) => (anchor !== undefined ? `anchor:${anchor}` : page !== undefined ? page : NONE);
const fromPlace = (v: string): { page?: string; anchor?: string } => (v.startsWith("anchor:") ? { anchor: v.slice(7) } : v === NONE ? {} : { page: v });

/** Only what the server stores: no empty layers, defaults or lists. */
function clean(meta: ReaderMeta): ReaderMeta {
  const auto = meta.defaults?.auto_advance_seconds;
  return {
    schema_version: 1,
    ...(auto !== undefined ? { defaults: { auto_advance_seconds: auto } } : {}),
    anchors: (meta.anchors ?? []).map((a) => ({ id: a.id.trim(), page: a.page, offset_percent: a.offset_percent, ...(a.duration_seconds !== undefined ? { duration_seconds: a.duration_seconds } : {}) })),
    background: (meta.background ?? []).map((c) => {
      const layer = c.layer?.trim();
      return { ...c, ...(layer ? { layer } : { layer: undefined }) };
    }),
    foreground: meta.foreground ?? [],
  };
}

const num = (v: string): number => (v.trim() === "" ? Number.NaN : Number(v));
const shown = (n: number | undefined) => (n === undefined || Number.isNaN(n) ? "" : String(n));

/**
 * Editors: a reader version's manifest over its current pages and audio
 * uploads (published or not), seeded from the stored manifest:
 * scroll anchors, foreground cues (one track at a page or anchor, matched
 * from file names in one step), background tracks over a range, and the
 * auto-advance default. Page order is the version's media order.
 */
export function ReaderManifestEditor({ kind, content, version, client: own, onError, className, appearance, heading = true }: ReaderManifestEditorProps) {
  const { t } = useMessages();
  const coordinated = useContext(EditorSaveContext) !== null;
  const client = useContentKitClient(own);
  const read = useMediaRead({ kind, id: content, version }, { editor: true, window: ALL_EDITOR_FILES, client });
  const media = useMemo(() => editorMedia(read.read), [read.read]);
  const names = useMemo<ReaderNames>(() => ({ pages: media.pages, files: media.files }), [media]);
  const seed = useMemo(() => {
    if (!read.read) return { meta: null };
    const stored = read.read.meta?.reader as ReaderMeta | undefined;
    return { meta: stored?.schema_version === 1 ? hashPageNames(structuredClone(stored), media.pages) : emptyReaderMeta() };
  }, [read.read, media.pages]);
  const draft = useDraft(seed);
  const meta = draft.value.meta;
  const dirty = draft.dirty.length > 0;
  const [saved, setSaved] = useState(false);
  const [matched, setMatched] = useState<AudioMatch | null>(null);
  const save = useAction("reader.save", onError);
  const problems = useMemo(() => (meta ? readerMetaProblems(meta, names) : []), [meta, names]);
  const edit = (fn: (m: ReaderMeta) => ReaderMeta) => {
    draft.edit((d) => ({ meta: d.meta ? fn(d.meta) : null }));
    setSaved(false);
  };
  const latest=useRef(meta);latest.current=meta;
  const submit=async()=>{
    if(!meta||problems.length)return false;
    setSaved(false);const snapshot=meta, settle=draft.checkpoint();
    const ok=await save.run(async()=>{await client.versions.setReader(content,version,clean(snapshot));return true;});
    if(!ok)return false;
    read.reload();
    if(latest.current!==snapshot)return false;
    if(!settle({meta:snapshot}))return false;
    setSaved(true);return true;
  };
  useEditorSaveTask(`reader-meta:${version}`,{label:t("readerManifest.title"),dirty,pending:save.pending,valid:!problems.length,save:submit,order:30});
  const body = (() => {
    if (read.error && !read.read) return <LoadFailed error={read.error} message={t("readerManifest.loadFailed")} retry={read.reload} />;
    if (!read.read) return <Rows count={2} />;
    if (!meta) return null;
    const anchors = meta.anchors ?? [];
    const anchorIds = anchors.map((a) => a.id).filter(Boolean);
    return (
      <form className="grid gap-4" onSubmit={(event)=>{event.preventDefault();if (coordinated) event.stopPropagation();else void submit();}}>
        <p className="text-muted-foreground">
          {t("readerManifest.summary", { pages: names.pages.length, files: names.files.length })} {!names.files.length && t("readerManifest.noAudio")}
        </p>
        <Defaults meta={meta} edit={edit} />
        <Group title={t("readerManifest.anchors")} help={t("readerManifest.anchorsHelp")} add={t("readerManifest.addAnchor")} disabled={!names.pages.length}
          onAdd={() => edit((m) => ({ ...m, anchors: [...(m.anchors ?? []), { id: nextId(m.anchors ?? []), page: names.pages[0]!.id, offset_percent: 0 }] }))}>
          {anchors.map((a, i) => (
            <AnchorRow key={i} index={i} anchor={a} media={media} onChange={(next) => edit((m) => ({ ...m, anchors: replace(m.anchors, i, next) }))}
              onRemove={() => edit((m) => ({ ...m, anchors: drop(m.anchors, i) }))} />
          ))}
        </Group>
        <Group title={t("readerManifest.foreground")} help={t("readerManifest.foregroundHelp")} add={t("readerManifest.addCue")} disabled={!names.files.length || !names.pages.length}
          onAdd={() => edit((m) => ({ ...m, foreground: [...(m.foreground ?? []), { file: names.files[0]!, page: names.pages[0]!.id }] }))}
          extra={
            <Button type="button" size="sm" variant="outline" disabled={!names.files.length} onClick={() => {
              const r = matchAudio(names, anchorIds);
              setMatched(r);
              if (r.foreground.length) edit((m) => ({ ...m, foreground: r.foreground }));
            }}>
              {t("readerManifest.match")}
            </Button>
          }>
          {matched && <MatchReport match={matched} />}
          {(meta.foreground ?? []).map((c, i) => (
            <ForegroundRow key={i} index={i} cue={c} names={names} media={media} anchors={anchorIds} onChange={(next) => edit((m) => ({ ...m, foreground: replace(m.foreground, i, next) }))}
              onRemove={() => edit((m) => ({ ...m, foreground: drop(m.foreground, i) }))} />
          ))}
        </Group>
        <Group title={t("readerManifest.background")} help={t("readerManifest.backgroundHelp")} add={t("readerManifest.addTrack")} disabled={!names.files.length}
          onAdd={() => edit((m) => ({ ...m, background: [...(m.background ?? []), { file: names.files[0]! }] }))}>
          {(meta.background ?? []).map((c, i) => (
            <BackgroundRow key={i} index={i} cue={c} names={names} media={media} anchors={anchorIds} onChange={(next) => edit((m) => ({ ...m, background: replace(m.background, i, next) }))}
              onRemove={() => edit((m) => ({ ...m, background: drop(m.background, i) }))} />
          ))}
        </Group>
        {problems.length > 0 && <Problems problems={problems} />}
        <ErrorLine error={save.error} />
        {!coordinated && <div className="flex flex-wrap items-center gap-3">
          <Button type="submit" size="sm" disabled={save.pending || !dirty || problems.length > 0}>
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
          {dirty && (
            <Button type="button" size="sm" variant="ghost" onClick={() => {
              draft.checkpoint()(seed);
              setMatched(null);
            }}>
              {t("readerManifest.revert")}
            </Button>
          )}
          {saved && <span role="status" className="text-muted-foreground">{t("readerManifest.saved")}</span>}
        </div>}
      </form>
    );
  })();
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-3 text-sm", className)} data-ckui="reader-manifest">
      {heading && <h4 className="font-medium">{t("readerManifest.title")}</h4>}
      <p className="text-pretty text-muted-foreground">{t("readerManifest.help")}</p>
      {body}
    </ContentKitUiRoot>
  );
}

function nextId(anchors: readonly Anchor[]): string {
  const taken = new Set(anchors.map((a) => a.id));
  let n = anchors.length + 1;
  while (taken.has(`a${n}`)) n++;
  return `a${n}`;
}

const replace = <T,>(list: T[] | undefined, i: number, next: T): T[] => (list ?? []).map((x, j) => (j === i ? next : x));
const drop = <T,>(list: T[] | undefined, i: number): T[] => (list ?? []).filter((_, j) => j !== i);

function Group({ title, help, add, disabled, onAdd, extra, children }: { title: string; help: string; add: string; disabled: boolean; onAdd: () => void; extra?: ReactNode; children: ReactNode }) {
  return (
    <fieldset className="grid gap-2 rounded-md border border-border p-3">
      <legend className="px-1 font-medium">{title}</legend>
      <p className="text-pretty text-xs text-muted-foreground">{help}</p>
      {children}
      <div className="flex flex-wrap gap-2">
        <Button type="button" size="sm" variant="outline" disabled={disabled} onClick={onAdd}>
          {add}
        </Button>
        {extra}
      </div>
    </fieldset>
  );
}

function Defaults({ meta, edit }: { meta: ReaderMeta; edit: (fn: (m: ReaderMeta) => ReaderMeta) => void }) {
  const { t } = useMessages();
  const id = useId();
  const auto = meta.defaults?.auto_advance_seconds;
  return (
    <div className="grid max-w-xs gap-1.5">
      <Label htmlFor={`${id}-auto`}>{t("readerManifest.autoAdvance")}</Label>
      <Input id={`${id}-auto`} className="h-8" type="number" min={0} step="any" value={shown(auto)}
        onChange={(e) => edit((m) => ({ ...m, defaults: e.target.value.trim() === "" ? undefined : { auto_advance_seconds: num(e.target.value) } }))} />
    </div>
  );
}

function NameSelect({ label, value, names, onChange }: { label: string; value: string; names: readonly string[]; onChange: (v: string) => void }) {
  return (
    <Select value={value} onValueChange={(v) => onChange(String(v))}>
      <SelectTrigger size="sm" aria-label={label}>
        <SelectValue>{(v: string) => v}</SelectValue>
      </SelectTrigger>
      <SelectContent>
        {names.map((n) => (
          <SelectItem key={n} value={n}>
            {n}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

function Place({ label, value, media, anchors, none, onChange }: { label: string; value: string; media: PlanMedia; anchors: readonly string[]; none?: string; onChange: (v: string) => void }) {
  const { t } = useMessages();
  const extras = [...(none !== undefined ? [{ value: NONE, label: none }] : []), ...anchors.map((a) => ({ value: `anchor:${a}`, label: t("readerManifest.anchorNamed", { id: a }) }))];
  return <PagePicker label={label} pages={media.pages} value={value} extras={extras} onChange={onChange} />;
}

function Remove({ label, onRemove }: { label: string; onRemove: () => void }) {
  const { t } = useMessages();
  return (
    <Button type="button" size="sm" variant="ghost" aria-label={label} onClick={onRemove}>
      {t("common.remove")}
    </Button>
  );
}

function AnchorRow({ index, anchor: a, media, onChange, onRemove }: { index: number; anchor: Anchor; media: PlanMedia; onChange: (a: Anchor) => void; onRemove: () => void }) {
  const { t } = useMessages();
  const n = index + 1;
  return (
    <div className="flex flex-wrap items-center gap-2" data-ckui="reader-anchor">
      <Input className="h-8 w-28" aria-label={t("readerManifest.anchorId", { n })} value={a.id} onChange={(e) => onChange({ ...a, id: e.target.value })} />
      <PagePicker label={t("readerManifest.anchorPage", { n })} pages={media.pages} value={a.page} onChange={(page) => onChange({ ...a, page })} />
      <Input className="h-8 w-24" type="number" min={0} max={100} step="any" aria-label={t("readerManifest.offset", { n })} value={shown(a.offset_percent)}
        onChange={(e) => onChange({ ...a, offset_percent: num(e.target.value) })} />
      <Input className="h-8 w-28" type="number" min={0} step="any" aria-label={t("readerManifest.duration", { n })} placeholder={t("readerManifest.durationPlaceholder")}
        value={shown(a.duration_seconds)} onChange={(e) => onChange({ ...a, duration_seconds: e.target.value.trim() === "" ? undefined : num(e.target.value) })} />
      <Remove label={t("readerManifest.removeAnchor", { n })} onRemove={onRemove} />
    </div>
  );
}

function ForegroundRow({ index, cue: c, names, media, anchors, onChange, onRemove }: { index: number; cue: ForegroundCue; names: ReaderNames; media: PlanMedia; anchors: readonly string[]; onChange: (c: ForegroundCue) => void; onRemove: () => void }) {
  const { t } = useMessages();
  const n = index + 1;
  return (
    <div className="flex flex-wrap items-center gap-2" data-ckui="reader-foreground">
      <NameSelect label={t("readerManifest.cueFile", { n })} value={c.file} names={names.files} onChange={(file) => onChange({ ...c, file })} />
      <AudioPreview url={media.audio.get(c.file)} label={c.file} />
      <Place label={t("readerManifest.cueAt", { n })} value={placeOf(c.page, c.anchor)} media={media} anchors={anchors} onChange={(v) => onChange({ file: c.file, ...fromPlace(v) })} />
      <Remove label={t("readerManifest.removeCue", { n })} onRemove={onRemove} />
    </div>
  );
}

function BackgroundRow({ index, cue: c, names, media, anchors, onChange, onRemove }: { index: number; cue: BackgroundCue; names: ReaderNames; media: PlanMedia; anchors: readonly string[]; onChange: (c: BackgroundCue) => void; onRemove: () => void }) {
  const { t } = useMessages();
  const n = index + 1;
  const from = fromPlace(placeOf(c.from_page, c.from_anchor));
  const to = fromPlace(placeOf(c.to_page, c.to_anchor));
  const set = (f: typeof from, tt: typeof to) => onChange({ file: c.file, loop: c.loop, layer: c.layer, from_page: f.page, from_anchor: f.anchor, to_page: tt.page, to_anchor: tt.anchor });
  return (
    <div className="flex flex-wrap items-center gap-2" data-ckui="reader-background">
      <NameSelect label={t("readerManifest.trackFile", { n })} value={c.file} names={names.files} onChange={(file) => onChange({ ...c, file })} />
      <AudioPreview url={media.audio.get(c.file)} label={c.file} />
      <Place label={t("readerManifest.trackFrom", { n })} none={t("readerManifest.start")} value={placeOf(c.from_page, c.from_anchor)} media={media} anchors={anchors}
        onChange={(v) => set(fromPlace(v), to)} />
      <Place label={t("readerManifest.trackTo", { n })} none={t("readerManifest.end")} value={placeOf(c.to_page, c.to_anchor)} media={media} anchors={anchors}
        onChange={(v) => set(from, fromPlace(v))} />
      <label className="flex items-center gap-1.5">
        <Switch size="sm" checked={!!c.loop} onCheckedChange={(on) => onChange({ ...c, loop: on || undefined })} aria-label={t("readerManifest.loopN", { n })} />
        {t("readerManifest.loop")}
      </label>
      <Input className="h-8 w-28" aria-label={t("readerManifest.layer", { n })} placeholder={t("readerManifest.layerPlaceholder")} value={c.layer ?? ""}
        onChange={(e) => onChange({ ...c, layer: e.target.value || undefined })} />
      <Remove label={t("readerManifest.removeTrack", { n })} onRemove={onRemove} />
    </div>
  );
}

function MatchReport({ match }: { match: AudioMatch }) {
  const { t } = useMessages();
  return (
    <div className="grid gap-1 rounded-md bg-muted p-2 text-xs" role="status">
      <p>{match.foreground.length ? t("readerManifest.matched", { count: match.foreground.length }) : t("readerManifest.matchedNone")}</p>
      {match.unmatchedPages.length > 0 && <p>{t("readerManifest.pagesWithoutAudio", { names: match.unmatchedPages.join(", ") })}</p>}
      {match.unmatchedFiles.length > 0 && <p>{t("readerManifest.audioWithoutPlace", { names: match.unmatchedFiles.join(", ") })}</p>}
    </div>
  );
}

function Problems({ problems }: { problems: ReaderMetaProblem[] }) {
  const { t } = useMessages();
  const where = (p: ReaderMetaProblem) => t(`readerManifest.at_${p.at}`, { n: p.index + 1 });
  return (
    <ul className="grid gap-0.5 text-destructive" role="alert" data-ckui="reader-problems">
      {problems.map((p, i) => (
        <li key={i}>{t(`readerManifest.problem_${p.problem}`, { where: where(p) })}</li>
      ))}
    </ul>
  );
}
