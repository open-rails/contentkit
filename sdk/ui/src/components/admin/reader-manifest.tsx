import { cn } from "cn";
import { useId, useMemo, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import type { Anchor, BackgroundCue, ForegroundCue, ReaderMeta, ReaderPlan } from "../../client/generated/wire.js";
import { emptyReaderMeta, matchAudio, readerMetaProblems, readerNames, type AudioMatch, type ReaderMetaProblem, type ReaderNames } from "../../client/reader-meta.js";
import { useMessages } from "../../i18n/context.js";
import { useContentKitClient, type ContentKitErrorHandler } from "../../react/context.js";
import { useReaderPlan } from "../../react/versions.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Switch } from "#ckui/ui/switch";
import { ErrorLine, useAction } from "../social/parts.js";
import { LoadFailed, Rows } from "./shell.js";

export interface ReaderManifestEditorProps {
  content: string;
  version: string;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}

const NONE = "__none__";

/** A page or an anchor as one select value. */
const placeOf = (page?: string, anchor?: string) => (anchor !== undefined ? `anchor:${anchor}` : page !== undefined ? `page:${page}` : NONE);
const fromPlace = (v: string): { page?: string; anchor?: string } =>
  v.startsWith("anchor:") ? { anchor: v.slice(7) } : v.startsWith("page:") ? { page: v.slice(5) } : {};

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
 * Editors: a reader version's manifest over its published pages and audio:
 * scroll anchors, foreground cues (one track at a page or anchor, matched
 * from file names in one step), background tracks over a range, and the
 * auto-advance default. Page order is the version's media order.
 */
export function ReaderManifestEditor({ content, version, client: own, onError, className, appearance }: ReaderManifestEditorProps) {
  const { t } = useMessages();
  const client = useContentKitClient(own);
  const plan = useReaderPlan(content, version, { client });
  const names = useMemo<ReaderNames>(() => (plan.plan ? readerNames(plan.plan) : { pages: [], files: [] }), [plan.plan]);
  const [basis, setBasis] = useState<ReaderPlan | null>(null);
  const [meta, setMeta] = useState<ReaderMeta | null>(null);
  const [dirty, setDirty] = useState(false);
  const [saved, setSaved] = useState(false);
  const [matched, setMatched] = useState<AudioMatch | null>(null);
  const save = useAction("reader.save", onError);
  // A fresh plan replaces the draft unless it has unsaved edits.
  if (plan.plan && plan.plan !== basis && !dirty) {
    setBasis(plan.plan);
    setMeta(plan.plan.reader ? structuredClone(plan.plan.reader) : emptyReaderMeta());
  }
  const problems = useMemo(() => (meta ? readerMetaProblems(meta, names) : []), [meta, names]);
  const edit = (fn: (m: ReaderMeta) => ReaderMeta) => {
    setMeta((m) => (m ? fn(m) : m));
    setDirty(true);
    setSaved(false);
  };
  const body = (() => {
    if (plan.loading && !plan.plan && !plan.pending) return <Rows count={2} />;
    if (plan.pending) return <p className="text-muted-foreground">{t("readerManifest.pending")}</p>;
    if (plan.error && !plan.plan) return <LoadFailed error={plan.error} message={t("readerManifest.loadFailed")} retry={plan.reload} />;
    if (!meta) return null;
    const anchors = meta.anchors ?? [];
    const anchorIds = anchors.map((a) => a.id).filter(Boolean);
    return (
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (problems.length) return;
          setSaved(false);
          void save
            .run(async () => {
              await client.versions.setReader(content, version, clean(meta));
              return true;
            })
            .then((ok) => {
              if (!ok) return;
              setDirty(false);
              setSaved(true);
            });
        }}
      >
        <p className="text-muted-foreground">{t("readerManifest.summary", { pages: names.pages.length, files: names.files.length })}</p>
        <Defaults meta={meta} edit={edit} />
        <Group title={t("readerManifest.anchors")} add={t("readerManifest.addAnchor")} disabled={!names.pages.length}
          onAdd={() => edit((m) => ({ ...m, anchors: [...(m.anchors ?? []), { id: nextId(m.anchors ?? []), page: names.pages[0]!, offset_percent: 0 }] }))}>
          {anchors.map((a, i) => (
            <AnchorRow key={i} index={i} anchor={a} pages={names.pages} onChange={(next) => edit((m) => ({ ...m, anchors: replace(m.anchors, i, next) }))}
              onRemove={() => edit((m) => ({ ...m, anchors: drop(m.anchors, i) }))} />
          ))}
        </Group>
        <Group title={t("readerManifest.foreground")} add={t("readerManifest.addCue")} disabled={!names.files.length || !names.pages.length}
          onAdd={() => edit((m) => ({ ...m, foreground: [...(m.foreground ?? []), { file: names.files[0]!, page: names.pages[0]! }] }))}
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
            <ForegroundRow key={i} index={i} cue={c} names={names} anchors={anchorIds} onChange={(next) => edit((m) => ({ ...m, foreground: replace(m.foreground, i, next) }))}
              onRemove={() => edit((m) => ({ ...m, foreground: drop(m.foreground, i) }))} />
          ))}
        </Group>
        <Group title={t("readerManifest.background")} add={t("readerManifest.addTrack")} disabled={!names.files.length}
          onAdd={() => edit((m) => ({ ...m, background: [...(m.background ?? []), { file: names.files[0]! }] }))}>
          {(meta.background ?? []).map((c, i) => (
            <BackgroundRow key={i} index={i} cue={c} names={names} anchors={anchorIds} onChange={(next) => edit((m) => ({ ...m, background: replace(m.background, i, next) }))}
              onRemove={() => edit((m) => ({ ...m, background: drop(m.background, i) }))} />
          ))}
        </Group>
        {problems.length > 0 && <Problems problems={problems} />}
        <ErrorLine error={save.error} />
        <div className="flex flex-wrap items-center gap-3">
          <Button type="submit" size="sm" disabled={save.pending || !dirty || problems.length > 0}>
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
          {dirty && (
            <Button type="button" size="sm" variant="ghost" onClick={() => {
              setDirty(false);
              setBasis(null);
              setMatched(null);
            }}>
              {t("readerManifest.revert")}
            </Button>
          )}
          {saved && <span role="status" className="text-muted-foreground">{t("readerManifest.saved")}</span>}
        </div>
      </form>
    );
  })();
  return (
    <ContentKitUiRoot appearance={appearance} className={cn("grid gap-3 text-sm", className)} data-ckui="reader-manifest">
      <h4 className="font-medium">{t("readerManifest.title")}</h4>
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

function Group({ title, add, disabled, onAdd, extra, children }: { title: string; add: string; disabled: boolean; onAdd: () => void; extra?: ReactNode; children: ReactNode }) {
  return (
    <fieldset className="grid gap-2 rounded-md border border-border p-3">
      <legend className="px-1 font-medium">{title}</legend>
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

function PlaceSelect({ label, value, pages, anchors, none, onChange }: { label: string; value: string; pages: readonly string[]; anchors: readonly string[]; none?: string; onChange: (v: string) => void }) {
  const { t } = useMessages();
  const text = (v: string) => (v === NONE ? (none ?? "") : v.startsWith("anchor:") ? t("readerManifest.anchorNamed", { id: v.slice(7) }) : t("readerManifest.pageNamed", { name: v.slice(5) }));
  return (
    <Select value={value} onValueChange={(v) => onChange(String(v))}>
      <SelectTrigger size="sm" aria-label={label}>
        <SelectValue>{text}</SelectValue>
      </SelectTrigger>
      <SelectContent>
        {none !== undefined && <SelectItem value={NONE}>{none}</SelectItem>}
        {anchors.map((a) => (
          <SelectItem key={`a-${a}`} value={`anchor:${a}`}>
            {text(`anchor:${a}`)}
          </SelectItem>
        ))}
        {pages.map((p) => (
          <SelectItem key={`p-${p}`} value={`page:${p}`}>
            {text(`page:${p}`)}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

function Remove({ label, onRemove }: { label: string; onRemove: () => void }) {
  const { t } = useMessages();
  return (
    <Button type="button" size="sm" variant="ghost" aria-label={label} onClick={onRemove}>
      {t("common.remove")}
    </Button>
  );
}

function AnchorRow({ index, anchor: a, pages, onChange, onRemove }: { index: number; anchor: Anchor; pages: readonly string[]; onChange: (a: Anchor) => void; onRemove: () => void }) {
  const { t } = useMessages();
  const n = index + 1;
  return (
    <div className="flex flex-wrap items-center gap-2" data-ckui="reader-anchor">
      <Input className="h-8 w-28" aria-label={t("readerManifest.anchorId", { n })} value={a.id} onChange={(e) => onChange({ ...a, id: e.target.value })} />
      <NameSelect label={t("readerManifest.anchorPage", { n })} value={a.page} names={pages} onChange={(page) => onChange({ ...a, page })} />
      <Input className="h-8 w-24" type="number" min={0} max={100} step="any" aria-label={t("readerManifest.offset", { n })} value={shown(a.offset_percent)}
        onChange={(e) => onChange({ ...a, offset_percent: num(e.target.value) })} />
      <Input className="h-8 w-28" type="number" min={0} step="any" aria-label={t("readerManifest.duration", { n })} placeholder={t("readerManifest.durationPlaceholder")}
        value={shown(a.duration_seconds)} onChange={(e) => onChange({ ...a, duration_seconds: e.target.value.trim() === "" ? undefined : num(e.target.value) })} />
      <Remove label={t("readerManifest.removeAnchor", { n })} onRemove={onRemove} />
    </div>
  );
}

function ForegroundRow({ index, cue: c, names, anchors, onChange, onRemove }: { index: number; cue: ForegroundCue; names: ReaderNames; anchors: readonly string[]; onChange: (c: ForegroundCue) => void; onRemove: () => void }) {
  const { t } = useMessages();
  const n = index + 1;
  return (
    <div className="flex flex-wrap items-center gap-2" data-ckui="reader-foreground">
      <NameSelect label={t("readerManifest.cueFile", { n })} value={c.file} names={names.files} onChange={(file) => onChange({ ...c, file })} />
      <PlaceSelect label={t("readerManifest.cueAt", { n })} value={placeOf(c.page, c.anchor)} pages={names.pages} anchors={anchors} onChange={(v) => onChange({ file: c.file, ...fromPlace(v) })} />
      <Remove label={t("readerManifest.removeCue", { n })} onRemove={onRemove} />
    </div>
  );
}

function BackgroundRow({ index, cue: c, names, anchors, onChange, onRemove }: { index: number; cue: BackgroundCue; names: ReaderNames; anchors: readonly string[]; onChange: (c: BackgroundCue) => void; onRemove: () => void }) {
  const { t } = useMessages();
  const n = index + 1;
  const from = fromPlace(placeOf(c.from_page, c.from_anchor));
  const to = fromPlace(placeOf(c.to_page, c.to_anchor));
  const set = (f: typeof from, tt: typeof to) => onChange({ file: c.file, loop: c.loop, layer: c.layer, from_page: f.page, from_anchor: f.anchor, to_page: tt.page, to_anchor: tt.anchor });
  return (
    <div className="flex flex-wrap items-center gap-2" data-ckui="reader-background">
      <NameSelect label={t("readerManifest.trackFile", { n })} value={c.file} names={names.files} onChange={(file) => onChange({ ...c, file })} />
      <PlaceSelect label={t("readerManifest.trackFrom", { n })} none={t("readerManifest.start")} value={placeOf(c.from_page, c.from_anchor)} pages={names.pages} anchors={anchors}
        onChange={(v) => set(fromPlace(v), to)} />
      <PlaceSelect label={t("readerManifest.trackTo", { n })} none={t("readerManifest.end")} value={placeOf(c.to_page, c.to_anchor)} pages={names.pages} anchors={anchors}
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
