import { cn } from "cn";
import { createPortal } from "react-dom";
import { useCallback, useContext, useEffect, useId, useMemo, useRef, useState, type ReactNode, type SetStateAction } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { fromLocalInput, parseAttributes, toLocalInput } from "../../client/group-fields.js";
import type { Content, ContentModel, Rule, Segment, SegmentInput, Version, VersionState } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import { useContentKitClient, type ContentKitErrorHandler } from "../../react/context.js";
import { useKinds } from "../../react/groups.js";
import { useMediaRead } from "../../react/read.js";
import { usePresets } from "../../react/public.js";
import { useContentScope } from "../../react/use-resource.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Switch } from "#ckui/ui/switch";
import { ReaderManifestEditor } from "../admin/reader-manifest.js";
import { PageNotices } from "../admin/page-notices.js";
import { ALL_EDITOR_FILES, editorMedia, PagePicker, usePlanMedia } from "../admin/page-picker.js";
import { useReaderPlan } from "../../react/versions.js";
import { VideoMediaEditor, videoPaths } from "../admin/media-item-panel.js";
import { Skeleton } from "#ckui/ui/skeleton";
import { MediaFolderEditor } from "../media-folder-editor.js";
import { SortableList } from "../sortable-list.js";
import { ErrorLine, useAction } from "../social/parts.js";
import { GroupPicker, labelOf, useGroupNames, type Labels } from "../groups/parts.js";
import { AttributeFields, formOf } from "../groups/fields.js";
import { MembershipEditor } from "../groups/memberships.js";
import { useDraft } from "../../react/use-draft.js";
import { EditorSaveContext, useEditorSaveTask } from "./editor-save.js";
import { OfferFields } from "./offer-fields.js";

interface Common {
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  labels?: Labels;
}

const NONE = "__none__";
const COMMENTS = ["open", "closed", "hidden"] as const;
type Comments = (typeof COMMENTS)[number];
const LEVELS: readonly Rule["level"][] = ["public", "tier", "ppv", "group", "group_ppv"];
const grouped = (l: Rule["level"]) => l === "group" || l === "group_ppv";

/** Prefer public access, otherwise the kind's first allowed level. */
export function defaultAccessRule(model: ContentModel, kind: string): Rule {
  const levels = model.kinds[kind]?.access_levels;
  const level = (!levels?.length || levels.includes("public") ? "public" : levels[0]) as Rule["level"];
  return { level, ...(level === "tier" ? { tiers: model.tiers.slice(0, 1) } : {}) };
}

/** The host's content model controls individual sale controls everywhere. */
export function allowsIndividualSales(model: ContentModel | null, kind: string): boolean {
  const spec = model?.kinds[kind];
  return !!spec && (!spec.access_levels?.length || spec.access_levels.some((level) => level === "ppv" || level === "group_ppv"));
}

/** Whether staff choose a kind's access: it allows more than one level (none listed allows all). */
export function offersAccessChoice(spec: ContentModel["kinds"][string] | undefined): boolean {
  return !!spec && (!spec.access_levels?.length || spec.access_levels.length > 1);
}

function Section({ title, ckui, children }: { title: ReactNode; ckui: string; children: ReactNode }) {
  return (
    <section className="grid gap-3 border-t border-border pt-4" data-ckui={ckui}>
      <h3 className="font-medium">{title}</h3>
      {children}
    </section>
  );
}

function Saved({ show }: { show: boolean }) {
  const { t } = useMessages();
  return show ? (
    <span role="status" className="text-muted-foreground">
      {t("contentEditor.saved")}
    </span>
  ) : null;
}

/** The content model's kind, when it loaded. */
function useKind(kind: string, client?: ContentKitClient): { model: ContentModel | null; spec: ContentModel["kinds"][string] | undefined } {
  const { kinds } = useKinds({ client });
  return { model: kinds, spec: kinds?.kinds[kind] };
}


function useEditorKey(id: string, client: ContentKitClient, scope: string) {
  const [identity, setIdentity] = useState({ id, client, scope, key: 0 });
  if (identity.id !== id || identity.client !== client || identity.scope !== scope) {
    const next = { id, client, scope, key: identity.key + 1 };
    setIdentity(next);
    return next.key;
  }
  return identity.key;
}

function useEditorReload(content: Content, own?: ContentKitClient) {
  const { client, store, scope, language } = useContentScope(own);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);
  return async () => {
    const fresh = await client.contents.get(content.id, language ?? content.language);
    let latest = fresh;
    if (mounted.current) store.patch<Content | null>(
      (tag) => tag.type === "content" && tag.id === content.id && (tag.readScope ?? "{}") === scope,
      (current) => {
        if (current && current.revision > latest.revision) latest = current;
        return latest;
      },
    );
    return latest;
  };
}

/** Comment policy and the kind's declared attributes. */
export function ContentDetailsEditor(p: Common & { content: Content }) {
  const { client, scope } = useContentScope(p.client);
  const key = useEditorKey(p.content.id, client, scope);
  return <ContentDetailsForm key={key} {...p} client={client} />;
}

function ContentDetailsForm({ content, ...p }: Common & { content: Content }) {
  const { t } = useMessages();
  const client = useContentKitClient(p.client);
  const { spec } = useKind(content.kind, p.client);
  const declared = useMemo(() => spec?.attributes ?? {}, [spec]);
  const fields = useDraft(useMemo(() => ({ comments: content.comments }), [content.comments]));
  const attrs = useDraft(useMemo(() => formOf(declared, content.attributes), [declared, content.attributes]));
  const { comments } = fields.value;
  const reload = useEditorReload(content, p.client);
  const [invalid, setInvalid] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const save = useAction("content.save", p.onError);
  return (
    <Section title={t("contentEditor.details")} ckui="content-details">
      <form
        className="grid gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          setInvalid(null);
          setSaved(false);
          let attributes: Record<string, unknown>;
          try {
            attributes = parseAttributes(declared, attrs.value);
          } catch (err) {
            return setInvalid((err as Error).message);
          }
          const settleFields = fields.checkpoint(), settleAttrs = attrs.checkpoint();
          void save.run(() => client.contents.update(content.id, { comments, attributes, revision: content.revision })).then((d) => {
            if (!d) return;
            const cleanFields = settleFields({ comments: d.comments });
            const cleanAttrs = settleAttrs(formOf(declared, d.attributes));
            setSaved(cleanFields && cleanAttrs);
          });
        }}
      >
        {spec?.comment_modes?.length !== 1 && (
          <div className="grid gap-1.5 sm:max-w-xs">
            <Label>{t("contentEditor.comments")}</Label>
            <Select value={comments} onValueChange={(v) => { fields.edit((f) => ({ ...f, comments: v as Comments })); setSaved(false); }}>
              <SelectTrigger size="sm" aria-label={t("contentEditor.comments")}>
                <SelectValue>{(v: string) => t(`contentEditor.comments_${v as Comments}`)}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {COMMENTS.filter((mode) => !spec?.comment_modes?.length || spec.comment_modes.includes(mode)).map((c) => (
                  <SelectItem key={c} value={c}>
                    {t(`contentEditor.comments_${c}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        )}
        <AttributeFields declared={declared} form={attrs.value} onChange={(n, v) => { attrs.edit((a) => ({ ...a, [n]: v })); setSaved(false); }} labels={p.labels} />
        {invalid && <ErrorLine>{invalid}</ErrorLine>}
        <ErrorLine error={save.error} />
        <div className="flex items-center gap-3">
          <Button type="submit" size="sm" disabled={save.pending}>
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
          <Button type="button" variant="ghost" size="sm" disabled={save.pending} onClick={() => {
            const settleFields = fields.checkpoint(), settleAttrs = attrs.checkpoint();
            void save.run(reload).then((d) => {
              if (!d) return;
              settleFields({ comments: d.comments });
              settleAttrs(formOf(declared, d.attributes));
              setInvalid(null);
              setSaved(false);
            });
          }}>{t("common.cancel")}</Button>
          <Saved show={saved} />
        </div>
      </form>
    </Section>
  );
}

/** A rule's form: level, tiers, group (the content's owner group by default), the preview its kind has and when it turns free. */
export function RuleFields({ value, onChange, model, kind, owner, labels, client }: { value: Rule; onChange: (r: Rule) => void; model: ContentModel | null; kind: string; owner?: string; labels?: Labels; client: ContentKitClient }) {
  const { t } = useMessages();
  const id = useId();
  const tiers = model?.tiers ?? [];
  const spec = model?.kinds[kind];
  const declaredLevels = spec?.access_levels;
  const fixedTier = !!declaredLevels?.length && tiers.length === 1;
  const levels = declaredLevels?.length ? LEVELS.filter((level) => declaredLevels.includes(level)) : LEVELS;
  const groupKinds = Object.entries(model?.entity_kinds ?? {})
    .filter(([, g]) => g.sellable || g.auth_scope)
    .map(([k]) => k);
  // Picked once the model has loaded: its first group kind until one is chosen.
  const [chosenKind, setGroupKind] = useState<string>();
  const groupKind = chosenKind ?? groupKinds[0] ?? "";
  const named = useGroupNames(value.group_id ? [value.group_id] : [], { client });
  const group = value.group_id ? named.get(value.group_id) : undefined;
  const set = (patch: Partial<Rule>) => onChange({ ...value, ...patch });
  const pages = spec?.presentation === "reader";
  const segments = !!spec?.segments;
  return (
    <div className="grid gap-3" data-ckui="rule-fields">
      <div className="grid gap-1.5 sm:max-w-xs">
        <Label>{t("contentEditor.access")}</Label>
        <Select
          value={value.level}
          onValueChange={(v) => {
            const level = v as Rule["level"];
            onChange({ level, tiers: level === "tier" ? (fixedTier ? [...tiers] : value.tiers) : undefined, group_id: grouped(level) ? value.group_id || owner : undefined, preview: value.preview, free_after: value.free_after });
          }}
        >
          <SelectTrigger size="sm" aria-label={t("contentEditor.access")}>
            <SelectValue>{(v: string) => t(`contentEditor.level_${v as Rule["level"]}`)}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {levels.filter((l) => (l !== "tier" || tiers.length) && (!l.startsWith("group") || groupKinds.length)).map((l) => (
              <SelectItem key={l} value={l}>
                {t(`contentEditor.level_${l}`)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      {value.level === "tier" && !fixedTier && (
        <fieldset className="flex flex-wrap gap-3">
          <legend className="sr-only">{t("contentEditor.tiers")}</legend>
          {tiers.map((tier) => (
            <label key={tier} className="flex items-center gap-2">
              <Switch
                checked={value.tiers?.includes(tier) ?? false}
                onCheckedChange={(on) => set({ tiers: on ? [...(value.tiers ?? []), tier] : (value.tiers ?? []).filter((x) => x !== tier) })}
                aria-label={labelOf(labels, tier)}
              />
              {labelOf(labels, tier)}
            </label>
          ))}
        </fieldset>
      )}
      {grouped(value.level) && (
        <div className="flex flex-wrap items-end gap-2">
          {group || value.group_id ? (
            <span className="rounded-md border border-border px-2 py-0.5" data-ckui="rule-group">
              {group ? group.name || group.slug : value.group_id}
            </span>
          ) : null}
          {groupKinds.length > 1 && (
            <Select value={groupKind} onValueChange={(v) => setGroupKind(String(v))}>
              <SelectTrigger size="sm" aria-label={t("groupEditor.edgeKind")}>
                <SelectValue>{(v: string) => labelOf(labels, v)}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {groupKinds.map((k) => (
                  <SelectItem key={k} value={k}>
                    {labelOf(labels, k)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          {groupKind && <GroupPicker key={groupKind} kind={groupKind} label={t("contentEditor.group")} client={client} onPick={(g) => set({ group_id: g.id })} />}
        </div>
      )}
      {value.level !== "public" && (
        <div className="grid gap-3 sm:grid-cols-3">
          {pages && (
            <div className="grid gap-1.5">
              <Label htmlFor={`${id}-pages`}>{t("contentEditor.previewPages")}</Label>
              <Input
                id={`${id}-pages`}
                className="h-8"
                type="number"
                min={0}
                value={value.preview?.pages ?? ""}
                onChange={(e) => set({ preview: { ...value.preview, pages: e.target.value ? Math.max(0, Math.trunc(Number(e.target.value))) : undefined } })}
              />
            </div>
          )}
          {segments && (
            <div className="grid gap-1.5">
              <Label htmlFor={`${id}-segments`}>{t("contentEditor.previewSegments")}</Label>
              <Input
                id={`${id}-segments`}
                className="h-8"
                type="number"
                min={0}
                value={value.preview?.segments ?? ""}
                onChange={(e) => set({ preview: { ...value.preview, segments: e.target.value ? Math.max(0, Math.trunc(Number(e.target.value))) : undefined } })}
              />
            </div>
          )}
          <div className="grid gap-1.5">
            <Label htmlFor={`${id}-free`}>{t("contentEditor.freeAfter")}</Label>
            <Input id={`${id}-free`} className="h-8" type="datetime-local" value={toLocalInput(value.free_after)} onChange={(e) => set({ free_after: fromLocalInput(e.target.value) })} />
          </div>
        </div>
      )}
    </div>
  );
}

/** A rule the server takes: a group level names its group. */
export const complete = (r: Rule) => !grouped(r.level) || !!r.group_id;

/** A rule as stored: empty previews and absent fields left out. */
export function cleanRule(r: Rule): Rule {
  const preview = r.preview && (r.preview.pages || r.preview.segments) ? { pages: r.preview.pages || undefined, segments: r.preview.segments || undefined } : undefined;
  return {
    level: r.level,
    tiers: r.level === "tier" ? r.tiers : undefined,
    group_id: r.level === "group" || r.level === "group_ppv" ? r.group_id : undefined,
    preview: r.level === "public" ? undefined : preview,
    free_after: r.level === "public" ? undefined : r.free_after,
  };
}

/** Who may open the content and what it sells for. */
export function AccessEditor(p: Common & { content: Content }) {
  const { client, scope } = useContentScope(p.client);
  const key = useEditorKey(p.content.id, client, scope);
  return <AccessForm key={key} {...p} client={client} />;
}

function AccessForm({ content, ...p }: Common & { content: Content }) {
  const { t } = useMessages();
  const client = useContentKitClient(p.client);
  const { model } = useKind(content.kind, p.client);
  const rule = useDraft(content.access);
  const reload = useEditorReload(content, p.client);
  const [saved, setSaved] = useState(false);
  const save = useAction("content.save", p.onError);
  const offer = useAction("offer.save", p.onError);
  return (
    <Section title={t(allowsIndividualSales(model, content.kind) ? "contentEditor.accessTitle" : "contentEditor.accessOnlyTitle")} ckui="access-editor">
      <form
        className="grid gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          setSaved(false);
          const settle = rule.checkpoint();
          void save.run(() => client.contents.update(content.id, { access: cleanRule(rule.value), revision: content.revision })).then((d) => d && setSaved(settle(d.access)));
        }}
      >
        <RuleFields value={rule.value} onChange={(r) => { rule.edit(r); setSaved(false); }} model={model} kind={content.kind} owner={content.owner_group_id} labels={p.labels} client={client} />
        <ErrorLine error={save.error} />
        <div className="flex items-center gap-3">
          <Button type="submit" size="sm" disabled={save.pending || !complete(rule.value)}>
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
          <Button type="button" variant="ghost" size="sm" disabled={save.pending} onClick={() => {
            const settle = rule.checkpoint();
            void save.run(reload).then((d) => {
              if (!d) return;
              settle(d.access);
              setSaved(false);
            });
          }}>{t("common.cancel")}</Button>
          <Saved show={saved} />
        </div>
      </form>
      {allowsIndividualSales(model, content.kind) && <OfferFields
        offer={content.offer}
        pending={offer.pending}
        onSell={(terms) => offer.run(() => client.offers.sell({ content: content.id }, terms))}
        onWithdraw={() => void offer.run(() => client.offers.withdraw({ content: content.id }))}
      />}
      <ErrorLine error={offer.error} />
    </Section>
  );
}

/** A version's name in the editor: its label, else its languages and traits. */
export function versionLabel(v: Version, languageName: (l: string) => string, labels?: Labels): string {
  return v.label || [...v.languages.map(languageName), ...v.traits.map((x) => labelOf(labels, x))].join(" · ");
}

/**
 * A content's versions: order, languages, traits, label, state and
 * schedule, an access override and its own offer, segments (chapters) and
 * the version's media folder; add and delete. A kind whose one version is
 * hidden shows only that version's segments and media.
 */
export function VersionsEditor({ content, media = true, activeVersion, ...p }: Common & { content: Content; media?: boolean; activeVersion?: string }) {
  const { t } = useMessages();
  const client = useContentKitClient(p.client);
  const languageName = useLanguageName();
  const { model, spec } = useKind(content.kind, p.client);
  const order = useAction("content.save", p.onError);
  const add = useAction("content.save", p.onError);
  const [open, setOpen] = useState<string | null>(null);
  const [opened, setOpened] = useState<string[]>([]);
  const reveal = (id: string) => { setOpen(id); setOpened((ids) => ids.includes(id) ? ids : [...ids,id]); };
  useEffect(() => { if (activeVersion) { setOpen(activeVersion); setOpened((ids)=>ids.includes(activeVersion)?ids:[...ids,activeVersion]); } }, [activeVersion]);
  useEditorSaveTask(`versions:${content.id}`, {label:t("contentEditor.versions"),dirty:false,pending:order.pending||add.pending,valid:true,save:async()=>true});
  const versions = useMemo(() => [...content.versions].sort((a, b) => a.position - b.position), [content.versions]);
  if (!spec) return null;
  if (spec.versions.hidden) {
    const only = versions[0];
    if (!only || (!media && !spec.segments)) return null;
    return (
      <Section title={t("contentEditor.media")} ckui="version-media">
        {spec.segments && <SegmentsEditor content={content} version={only} {...p} />}
        {media && <VersionMedia content={content} version={only} model={model} {...p} />}
      </Section>
    );
  }
  const languages = model?.languages ?? [];
  const activeVersions = versions.filter((version) => version.state !== "deleted");
  const deletedVersions = versions.filter((version) => version.state === "deleted");
  const versionRow = (v: Version, handle?: ReactNode) => (
    <div className="grid gap-2 rounded-md border border-border p-2" data-ckui="version-row" data-version={v.id}>
      <div className="flex flex-wrap items-center gap-2">
        {activeVersions.length > 1 && handle}
        <span className="min-w-0 flex-1 font-medium break-words">{versionLabel(v, languageName, p.labels)}</span>
        <Badge variant={v.live ? "default" : "outline"}>{t(v.state === "deleted" ? "contentManager.deleted" : v.live ? "contentEditor.live" : v.state === "draft" ? "contentManager.draft" : "contentManager.scheduled")}</Badge>
        {v.access && <Badge variant="secondary">{t(`contentEditor.level_${v.access.level}`)}</Badge>}
        {v.pages ? <span className="text-xs text-muted-foreground">{t("contentEditor.pageCount", { count: v.pages })}</span> : null}
        <Button type="button" size="xs" variant="ghost" aria-expanded={open === v.id} onClick={() => open === v.id ? setOpen(null) : reveal(v.id)}>
          {t(open === v.id ? "contentEditor.close" : "contentEditor.edit")}
        </Button>
      </div>
      {opened.includes(v.id) && <div hidden={open !== v.id}><VersionForm content={content} version={v} model={model} media={media} {...p} /></div>}
    </div>
  );
  return (
    <Section title={t("contentEditor.versions")} ckui="versions-editor">
      <SortableList
        items={activeVersions}
        id={(v) => v.id}
        name={(v) => versionLabel(v, languageName, p.labels)}
        label={t("contentEditor.versions")}
        disabled={order.pending || activeVersions.length < 2}
        trailing={deletedVersions.map((v) => <li key={v.id} data-ckui="deleted-version">{versionRow(v)}</li>)}
        onMove={(from, to) => {
          const ids = activeVersions.map((v) => v.id);
          const [moved] = ids.splice(from, 1);
          ids.splice(to, 0, moved!);
          void order.run(() => client.contents.orderVersions(content.id, ids));
        }}
      >
        {versionRow}
      </SortableList>
      <ErrorLine error={order.error} />
      <AddVersion
        languages={languages}
        many={spec?.versions.languages === "many"}
        pending={add.pending}
        // A new version is a draft: open it for its media.
        onAdd={(input) => void add.run(() => client.contents.addVersion(content.id, input)).then((v) => v && reveal(v.id))}
      />
      <ErrorLine error={add.error} />
    </Section>
  );
}

function LanguageFields({ languages, value, many, onChange }: { languages: readonly string[]; value: readonly string[]; many: boolean; onChange: (v: string[]) => void }) {
  const { t } = useMessages();
  const languageName = useLanguageName();
  if (!many)
    return (
      <Select value={value[0] ?? NONE} onValueChange={(v) => onChange(v === NONE ? [] : [String(v)])}>
        <SelectTrigger size="sm" aria-label={t("contentEditor.versionLanguage")}>
          <SelectValue>{(v: string) => (v === NONE ? t("contentEditor.chooseLanguage") : languageName(v))}</SelectValue>
        </SelectTrigger>
        <SelectContent>
          {languages.map((l) => (
            <SelectItem key={l} value={l}>
              {languageName(l)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    );
  return (
    <fieldset className="flex flex-wrap gap-3">
      <legend className="sr-only">{t("contentEditor.versionLanguages")}</legend>
      {languages.map((l) => (
        <label key={l} className="flex items-center gap-1.5">
          <Switch size="sm" checked={value.includes(l)} onCheckedChange={(on) => onChange(on ? [...value, l] : value.filter((x) => x !== l))} aria-label={languageName(l)} />
          {languageName(l)}
        </label>
      ))}
    </fieldset>
  );
}

/** A sibling a new version may start from, and what it has to copy. */
export interface VersionSource {
  id: string;
  label: string;
  audio: boolean;
  chapters: boolean;
}

export function AddVersion({ languages, many, pending, sources, onAdd }: {
  languages: readonly string[];
  many: boolean;
  pending: boolean;
  /** Offers "Start from" one of these: the new version copies its files, and its audio and chapters when ticked. */
  sources?: readonly VersionSource[];
  onAdd: (v: { languages: string[] }, from?: { from: string; playback: boolean; segments: boolean }) => void;
}) {
  const { t } = useMessages();
  const id = useId();
  const [langs, setLangs] = useState<string[]>([]);
  const [from, setFrom] = useState(NONE);
  const [audio, setAudio] = useState(true);
  const [chapters, setChapters] = useState(false);
  const source = sources?.find((s) => s.id === from);
  return (
    <div className="flex flex-wrap items-center gap-2" data-ckui="add-version">
      <LanguageFields languages={languages} value={langs} many={many} onChange={setLangs} />
      {!!sources?.length && (
        <div className="grid w-full gap-2" data-ckui="add-version-source">
          <Label htmlFor={`${id}-from`}>{t("readerEditor.startFrom")}</Label>
          <Select value={from} onValueChange={(v) => setFrom(String(v ?? NONE))}>
            <SelectTrigger id={`${id}-from`} size="sm" className="w-fit">
              <SelectValue>{(v: string) => sources.find((s) => s.id === v)?.label ?? t("readerEditor.startEmpty")}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={NONE}>{t("readerEditor.startEmpty")}</SelectItem>
              {sources.map((s) => (
                <SelectItem key={s.id} value={s.id}>
                  {s.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <p className="text-sm text-pretty text-muted-foreground">{t("readerEditor.startFromHelp")}</p>
          {source && (source.audio || source.chapters) && (
            <div className="flex flex-wrap gap-x-4 gap-y-2 text-sm">
              {source.audio && (
                <label className="flex items-center gap-2">
                  <input type="checkbox" className="size-4 accent-primary" checked={audio} onChange={(e) => setAudio(e.target.checked)} />
                  {t("readerEditor.copyAudio")}
                </label>
              )}
              {source.chapters && (
                <label className="flex items-center gap-2">
                  <input type="checkbox" className="size-4 accent-primary" checked={chapters} onChange={(e) => setChapters(e.target.checked)} />
                  {t("readerEditor.copyChapters")}
                </label>
              )}
            </div>
          )}
        </div>
      )}
      <Button
        type="button"
        size="sm"
        variant="outline"
        disabled={pending || !langs.length}
        onClick={() => {
          onAdd({ languages: langs }, source && { from: source.id, playback: source.audio && audio, segments: source.chapters && chapters });
          setLangs([]);
        }}
      >
        {t("contentEditor.addVersion")}
      </Button>
    </div>
  );
}

export interface VersionEditorProps extends Common {
  content: Content;
  version: Version;
  /** Render the edition's media tools (default true). */
  media?: boolean;
  /** Render chapter ranges with the edition settings (default true). */
  chapters?: boolean;
  /** Let the host confirm discarding other unsaved fields before deleting this edition. */
  beforeDelete?: () => boolean | Promise<boolean>;
  onDeleted?: (version: Version) => void;
  onRestored?: (version: Version) => void;
  /** Optional enabled slot outside a deleted edition's read-only fieldset. */
  restoreTarget?: HTMLElement | null;
  /** Optional header slot for this edition's staged publication time. */
  scheduleTarget?: HTMLElement | null;
  onScheduleChange?: (at: string) => void;
  /** Reflect this edition's staged language before the containing editor saves. */
  onLanguagesChange?: (languages: readonly string[]) => void;
  scheduleDisabled?: boolean;
  showTraits?: boolean;
  /** Optional footer slot for the destructive version action. */
  actionsTarget?: HTMLElement | null;
  /** Reader tools slot for the staged continuous-page layout, when declared. */
  readerToolsTarget?: HTMLElement | null;
}

/** One edition's settings, reusable independently of the all-editions manager. */
export function VersionEditor({ content, version, media = true, chapters = true, ...p }: VersionEditorProps) {
  const { client, scope } = useContentScope(p.client);
  const key = useEditorKey(`${content.id}:${version.id}`, client, scope);
  const { model } = useKind(content.kind, client);
  return <VersionForm key={key} content={content} version={version} model={model} media={media} chapters={chapters} {...p} client={client} />;
}

function VersionForm({ content, version: v, model, media, chapters = true, beforeDelete, onDeleted, onRestored, restoreTarget, scheduleTarget, onScheduleChange, onLanguagesChange, scheduleDisabled, showTraits = true, actionsTarget, readerToolsTarget, ...p }: Common & { content: Content; version: Version; model: ContentModel | null; media: boolean; chapters?: boolean; beforeDelete?: () => boolean | Promise<boolean>; onDeleted?: (version: Version) => void; onRestored?: (version: Version) => void; restoreTarget?: HTMLElement | null; scheduleTarget?: HTMLElement | null; onScheduleChange?: (at: string) => void; onLanguagesChange?: (languages: readonly string[]) => void; scheduleDisabled?: boolean; showTraits?: boolean; actionsTarget?: HTMLElement | null; readerToolsTarget?: HTMLElement | null }) {
  const { t } = useMessages();
  const id = useId();
  const client = useContentKitClient(p.client);
  const spec = model?.kinds[content.kind];
  const coordinated = useContext(EditorSaveContext) !== null;
  const seed = useCallback((v: Version) => ({langs:v.languages,traits:v.traits,at:toLocalInput(v.publish_at),own:!!v.access,rule:v.access??content.access}),[content.access]);
  const fields = useDraft(useMemo(()=>seed(v),[seed,v]));
  const {langs,traits,at,own,rule}=fields.value;
  const directAccess = spec?.access_scope === "version";
  useEffect(() => { onScheduleChange?.(at); }, [at, onScheduleChange]);
  useEffect(() => { onLanguagesChange?.(langs); }, [langs, onLanguagesChange]);
  const edit=(patch:Partial<typeof fields.value>)=>{fields.edit((old)=>({...old,...patch}));setSaved(false);};
  const [saved, setSaved] = useState(false);
  const save = useAction("content.save", p.onError);
  const remove = useAction("content.save", p.onError);
  const offer = useAction("offer.save", p.onError);
  const valid=langs.length>0&&(!(directAccess||own)||complete(rule));
  const submit=async(state?: VersionState)=>{
    if(!valid)return false;
    setSaved(false);
    const settle=fields.checkpoint();
    const accessChanged = fields.dirty.includes("own") || fields.dirty.includes("rule");
    const out=await save.run(()=>client.contents.updateVersion(content.id,v.id,{languages:langs,traits,publish_at:fromLocalInput(at),...(state?{state}:{}),...(accessChanged?(directAccess||own?{access:cleanRule(rule)}:{clear_access:true}):{})}));
    if(!out)return false;
    const clean=settle(seed(out));setSaved(clean);return clean;
  };
  useEditorSaveTask(`version:${v.id}`,{label:t("contentEditor.versions"),dirty:v.state!=="deleted"&&fields.dirty.length>0,pending:save.pending||remove.pending,valid,save:submit,order:10});
  const schedule = <div className="grid gap-1.5">
    <Label htmlFor={`${id}-at`}>{t("contentEditor.versionPublishAt")}</Label>
    <Input id={`${id}-at`} className="h-8" type="datetime-local" value={at} onChange={(e)=>edit({at:e.target.value})} disabled={save.pending || scheduleDisabled} />
  </div>;
  const readingLayout = spec?.versions.traits?.includes("manhwa_style") && <div className="grid gap-1.5 sm:max-w-xs">
    <Label htmlFor={`${id}-layout`}>{t("contentEditor.readingLayout")}</Label>
    <Select value={traits.includes("manhwa_style") ? "continuous" : "paged"} onValueChange={(value) => edit({ traits: value === "continuous" ? [...new Set([...traits, "manhwa_style"])] : traits.filter((trait) => trait !== "manhwa_style") })}>
      <SelectTrigger id={`${id}-layout`} size="sm"><SelectValue>{(value: string) => t(value === "continuous" ? "contentEditor.continuousVertical" : "contentEditor.pagedLayout")}</SelectValue></SelectTrigger>
      <SelectContent>
        <SelectItem value="paged">{t("contentEditor.pagedLayout")}</SelectItem>
        <SelectItem value="continuous">{t("contentEditor.continuousVertical")}</SelectItem>
      </SelectContent>
    </Select>
  </div>;
  const destructive = <>
    <Button type="button" size="sm" variant="destructive" disabled={remove.pending} onClick={() => void remove.run(async () => {
      if (beforeDelete && !(await beforeDelete())) return false;
      await client.contents.removeVersion(content.id, v.id);
      return true;
    }).then((removed) => { if (removed) onDeleted?.(v); })}>{t("contentEditor.deleteVersion")}</Button>
    <ErrorLine error={remove.error} />
  </>;
  if (v.state === "deleted") {
    const restore = <div className="grid gap-2">
      <Button type="button" size="sm" disabled={save.pending} onClick={() => void save.run(() => client.contents.updateVersion(content.id, v.id, { state: "draft" })).then((restored) => { if (restored) onRestored?.(restored); })}>
        {t("contentEditor.restoreVersion")}
      </Button>
      <ErrorLine error={save.error} />
    </div>;
    return restoreTarget === undefined ? restore : restoreTarget && createPortal(restore, restoreTarget);
  }
  return (
    <div className="grid gap-4 pt-2">
      <form className="grid gap-3" onSubmit={(event)=>{event.preventDefault();if (coordinated) event.stopPropagation();else void submit();}}>
        <LanguageFields languages={model?.languages ?? []} value={langs} many={spec?.versions.languages === "many"} onChange={(langs)=>edit({langs})} />
        {showTraits && !!spec?.versions.traits?.length && (
          <fieldset className="flex flex-wrap gap-3">
            <legend className="sr-only">{t("contentEditor.traits")}</legend>
            {spec.versions.traits.map((x) => (
              <label key={x} className="flex items-center gap-1.5">
                <Switch size="sm" checked={traits.includes(x)} onCheckedChange={(on) => edit({traits:on ? [...traits, x] : traits.filter((y) => y !== x)})} aria-label={labelOf(p.labels, x)} />
                {labelOf(p.labels, x)}
              </label>
            ))}
          </fieldset>
        )}
        {scheduleTarget === undefined ? schedule : scheduleTarget && createPortal(schedule, scheduleTarget)}
        {!directAccess && <label className="flex items-center gap-2">
          <Switch size="sm" checked={own} onCheckedChange={(own)=>edit({own})} />
          {t("contentEditor.ownAccess")}
        </label>}
        {(directAccess || own) && <RuleFields value={rule} onChange={(rule)=>edit({rule})} model={model} kind={content.kind} owner={content.owner_group_id} labels={p.labels} client={client} />}
        <ErrorLine error={save.error} />
        <div className="flex flex-wrap items-center gap-3">
          {!coordinated && <>
            <Button type="submit" size="sm" disabled={save.pending || !langs.length}>
              {save.pending ? t("common.saving") : t("common.save")}
            </Button>
            <Button type="button" size="sm" variant="outline" disabled={save.pending || !valid} onClick={() => void submit(v.state === "draft" ? "published" : "draft")}>
              {t(v.state === "draft" ? "contentManager.publish" : "contentManager.unpublish")}
            </Button>
            <Saved show={saved} />
          </>}
          {actionsTarget === undefined ? destructive : actionsTarget && createPortal(destructive, actionsTarget)}
        </div>
      </form>
      {readerToolsTarget && readingLayout && createPortal(readingLayout, readerToolsTarget)}
      {allowsIndividualSales(model, content.kind) && <OfferFields
        offer={v.offer}
        pending={offer.pending}
        onSell={(terms) => offer.run(() => client.offers.sell({ content: content.id, version: v.id }, terms))}
        onWithdraw={() => void offer.run(() => client.offers.withdraw({ content: content.id, version: v.id }))}
      />}
      <ErrorLine error={offer.error} />
      {chapters && spec?.segments && <SegmentsEditor content={content} version={v} {...p} />}
      {media && <VersionCover content={content} version={v} {...p} />}
      {media && <VersionMedia content={content} version={v} model={model} {...p} />}
      {media && spec?.presentation === "reader" && <ReaderManifestEditor kind={content.kind} content={content.id} version={v.id} client={p.client} onError={p.onError} />}
    </div>
  );
}

/**
 * A version's own cover, where its kind has covers: the cover upload its
 * editors selected, else its first. Versions share no files.
 */
export function VersionCover({ content, version: v, ...p }: Common & { content: Content; version: Version }) {
  const { t } = useMessages();
  const client = useContentKitClient(p.client);
  const { presets } = usePresets({ client });
  const save = useAction("content.save", p.onError);
  const input = useRef<HTMLInputElement>(null);
  const [fresh, setFresh] = useState<string | null>(null);
  useEffect(() => () => void (fresh && URL.revokeObjectURL(fresh)), [fresh]);
  if (!presets.some((r) => r.kind === content.kind && r.name === "cover")) return null;
  const src = fresh ?? v.cover?.url;
  const pick = (file: File | null) =>
    void save.run(async () => {
      if (!file) {
        setFresh(null);
        return client.contents.setCover(content.id, v.id, null);
      }
      const ref = { kind: content.kind, id: content.id, version: v.id };
      const up = await client.media.uploadNamed(ref, file, { path: await client.media.presetPath(content.kind, "cover", file) });
      const saved = await client.contents.setCover(content.id, v.id, up.name);
      if (typeof URL.createObjectURL === "function") setFresh(URL.createObjectURL(file));
      return saved;
    });
  return (
    <div className="grid gap-1.5" data-ckui="version-cover">
      <span className="text-sm font-medium">{t("contentManager.cover")}</span>
      <div className="flex flex-wrap items-center gap-3">
        {src ? <img src={src} alt="" className="h-20 w-36 rounded-md border border-border object-cover" /> : <span className="text-muted-foreground">{t("contentManager.noCover")}</span>}
        <Button type="button" size="sm" variant="outline" disabled={save.pending} onClick={() => input.current?.click()}>
          {t(src ? "pollEditor.replaceImage" : "contentManager.addCover")}
        </Button>
        {v.cover?.image && (
          <Button type="button" size="sm" variant="ghost" disabled={save.pending} onClick={() => pick(null)}>
            {t("pollEditor.removeImage")}
          </Button>
        )}
        <input
          ref={input}
          type="file"
          accept="image/*"
          hidden
          aria-label={t("contentManager.cover")}
          onChange={(e) => {
            const f = e.target.files?.[0];
            e.target.value = "";
            if (f) pick(f);
          }}
        />
      </div>
      <ErrorLine error={save.error} />
    </div>
  );
}

/**
 * A version's media: a video kind's source, poster and subtitles (by its
 * upload rules), else its upload folders. Uploads to a version that is not
 * live are added as they finish; a live one's wait for "Add".
 */
function VersionMedia({ content, version, model, ...p }: Common & { content: Content; version: Version; model: ContentModel | null }) {
  const { t } = useMessages();
  const languageName = useLanguageName();
  const item = useMemo(() => ({ kind: content.kind, id: content.id, version: version.id }), [content.kind, content.id, version.id]);
  const r = useMediaRead(item, { editor: true, client: p.client });
  const languages = useMemo(() => (model?.languages ?? []).map((l) => ({ value: l, label: languageName(l) })), [model, languageName]);
  if (!r.read && r.loading) return <Skeleton className="h-24 w-full" />;
  if (r.read?.uploads && videoPaths(r.read.uploads).source) return <VideoMediaEditor item={item} languages={languages} client={p.client} onError={p.onError} />;
  return <>
    <MediaFolderEditor item={item} commit={version.live ? "manual" : "auto"} label={t("contentEditor.media")} client={p.client} onError={p.onError} />
    {model?.kinds[content.kind]?.presentation === "reader" && <PageNotices content={content.id} version={version.id} client={p.client} onError={p.onError} />}
  </>;
}

type Row = { title: string; start: string; end: string; release: string };
const segmentRows=(version:Version):Row[]=>version.segments.map((s)=>({title:s.title??"",start:String(s.start),end:String(s.end),release:toLocalInput(s.release_at)}));

type SegmentsEditorProps = Common & { content: Content; version: Version; heading?: boolean };

/** Reader chapters use stable start-page identities; other presentations retain their numeric ranges. */
export function SegmentsEditor(p: SegmentsEditorProps) {
  const { spec } = useKind(p.content.kind, p.client);
  if (!spec) return <Skeleton className="h-24 w-full" />;
  return spec.presentation === "reader" ? <ReaderChaptersEditor {...p} /> : <NumericSegmentsEditor {...p} />;
}

type ChapterRow = { title: string; startPage: string; release: string };

function ReaderChaptersEditor({ content, version, heading = true, ...p }: SegmentsEditorProps) {
  const { t } = useMessages();
  const coordinated = useContext(EditorSaveContext) !== null;
  const client = useContentKitClient(p.client);
  const read = useMediaRead({ kind: content.kind, id: content.id, version: version.id }, { editor: true, window: ALL_EDITOR_FILES, client });
  const pages = useMemo(() => editorMedia(read.read).pages, [read.read]);
  const rowsOf = useCallback((value: Version): ChapterRow[] => value.segments.map((segment) => ({
    title: segment.title ?? "",
    startPage: segment.start_page || pages[segment.start]?.id || "",
    release: toLocalInput(segment.release_at),
  })), [pages]);
  const fields = useDraft(useMemo(() => ({ rows: rowsOf(version) }), [rowsOf, version]));
  const rows = fields.value.rows;
  const starts = rows.map((row) => pages.findIndex((page) => page.id === row.startPage && !page.disabled));
  const unselected = rows.some((row) => !row.startPage);
  const missing = rows.some((row) => !!row.startPage && !pages.some((page) => page.id === row.startPage));
  const ambiguous = rows.some((row) => pages.some((page) => page.id === row.startPage && page.disabled));
  const ordered = starts.every((start, i) => i === 0 || start > starts[i - 1]!);
  const valid = !unselected && !missing && !ambiguous && ordered;
  const [saved, setSaved] = useState(false);
  const save = useAction("content.save", p.onError);
  const edit = (index: number, patch: Partial<ChapterRow>) => {
    setSaved(false);
    fields.edit((previous) => ({ rows: previous.rows.map((row, i) => i === index ? { ...row, ...patch } : row) }));
  };
  const submit = async () => {
    if (!valid) return false;
    setSaved(false);
    const settle = fields.checkpoint();
    const segments: SegmentInput[] = rows.map((row, i) => ({ position: i + 1, title: row.title.trim() || undefined, start_page: row.startPage, release_at: fromLocalInput(row.release) }));
    const out = await save.run(() => client.contents.setSegments(content.id, version.id, segments));
    if (!out) return false;
    const clean = settle({ rows: rowsOf(out) });
    setSaved(clean);
    return clean;
  };
  useEditorSaveTask(`segments:${version.id}`, { label: t("contentEditor.segments"), dirty: fields.dirty.length > 0, pending: save.pending, valid, save: submit, order: 20 });
  if (read.loading && !read.read) return <Skeleton className="h-24 w-full" />;
  return <div className="grid gap-3" data-ckui="segments-editor">
    {heading && <h4 className="text-sm font-medium">{t("contentEditor.segments")}</h4>}
    {rows.length === 0 && <p className="text-muted-foreground">{t("contentEditor.noSegments")}</p>}
    {rows.map((row, i) => {
      const start = starts[i]!, end = starts[i + 1] ?? pages.length;
      return <div key={i} className="flex flex-wrap items-center gap-2" data-ckui="segment-row">
        <span className="w-6 text-xs tabular-nums text-muted-foreground">{i + 1}</span>
        <Input className="h-8 w-40" aria-label={t("contentEditor.segmentTitle", { n: i + 1 })} placeholder={t("contentEditor.segmentTitlePlaceholder")} value={row.title} onChange={(e) => edit(i, { title: e.target.value })} />
        <PagePicker label={t("contentEditor.segmentStart", { n: i + 1 })} pages={pages} value={row.startPage} onChange={(startPage) => edit(i, { startPage })} />
        <output className="text-sm tabular-nums text-muted-foreground" data-ckui="chapter-range">{start >= 0 && end > start ? t("contentEditor.chapterRange", { from: start + 1, to: end }) : "—"}</output>
        <Input className="h-8 w-48" type="datetime-local" aria-label={t("contentEditor.segmentRelease", { n: i + 1 })} value={row.release} onChange={(e) => edit(i, { release: e.target.value })} />
        <Button type="button" size="xs" variant="ghost" onClick={() => { setSaved(false); fields.edit((previous) => ({ rows: previous.rows.filter((_, j) => j !== i) })); }}>{t("common.remove")}</Button>
      </div>;
    })}
    {unselected && <ErrorLine>{t("contentEditor.chapterStartRequired")}</ErrorLine>}
    {missing && <ErrorLine>{t("contentEditor.chapterStartMissing")}</ErrorLine>}
    {ambiguous && <ErrorLine>{t("contentEditor.chapterStartAmbiguous")}</ErrorLine>}
    {!unselected && !missing && !ambiguous && !ordered && <ErrorLine>{t("contentEditor.chapterStartsOrdered")}</ErrorLine>}
    <div className="flex flex-wrap items-center gap-2">
      <Button type="button" size="sm" variant="outline" onClick={() => {
        const next = starts.length ? starts[starts.length - 1]! + 1 : 0;
        setSaved(false);
        fields.edit((previous) => ({ rows: [...previous.rows, { title: "", startPage: pages[next]?.id ?? "", release: "" }] }));
      }}>{t("contentEditor.addSegment")}</Button>
      {!coordinated && <>
        <Button type="button" size="sm" disabled={save.pending || !valid} onClick={() => void submit()}>{t("contentEditor.saveSegments")}</Button>
        <Saved show={saved} />
      </>}
    </div>
    <ErrorLine error={read.error} />
    <ErrorLine error={save.error} />
  </div>;
}

/** Numeric ranges for presentations without reader pages. */
function NumericSegmentsEditor({ content, version, heading = true, ...p }: SegmentsEditorProps) {
  const { t } = useMessages();
  const coordinated = useContext(EditorSaveContext) !== null;
  const client = useContentKitClient(p.client);
  const fields=useDraft(useMemo(()=>({rows:segmentRows(version)}),[version]));
  const rows=fields.value.rows;
  const setRows=(next:SetStateAction<Row[]>)=>fields.edit((old)=>({rows:typeof next==="function"?next(old.rows):next}));
  const [saved, setSaved] = useState(false);
  const save = useAction("content.save", p.onError);
  // Published pages pick ranges by thumbnail; before a publication, numbers.
  const plan = useReaderPlan(content.id, version.id, { client });
  const { pages } = usePlanMedia(plan.plan, client);
  const set = (i: number, patch: Partial<Row>) => {
    setSaved(false);
    setRows((r) => r.map((x, j) => (j === i ? { ...x, ...patch } : x)));
  };
  const valid = rows.every((r) => /^\d+$/.test(r.start) && /^\d+$/.test(r.end) && Number(r.end) > Number(r.start));
  const submit=async()=>{
    if(!valid)return false;
    setSaved(false);const settle=fields.checkpoint();
    const segments:Segment[]=rows.map((r,i)=>({position:i+1,title:r.title.trim()||undefined,start:Number(r.start),end:Number(r.end),release_at:fromLocalInput(r.release),released:false}));
    const out=await save.run(()=>client.contents.setSegments(content.id,version.id,segments));
    if(!out)return false;const clean=settle({rows:segmentRows(out)});setSaved(clean);return clean;
  };
  useEditorSaveTask(`segments:${version.id}`,{label:t("contentEditor.segments"),dirty:fields.dirty.length>0,pending:save.pending,valid,save:submit,order:20});
  return (
    <div className="grid gap-2" data-ckui="segments-editor">
      {heading && <h4 className="text-sm font-medium">{t("contentEditor.segments")}</h4>}
      {rows.length === 0 && <p className="text-muted-foreground">{t("contentEditor.noSegments")}</p>}
      {rows.map((r, i) => (
        <div key={i} className="flex flex-wrap items-center gap-2" data-ckui="segment-row">
          <span className="w-6 text-xs text-muted-foreground tabular-nums">{i + 1}</span>
          <Input className="h-8 w-40" aria-label={t("contentEditor.segmentTitle", { n: i + 1 })} placeholder={t("contentEditor.segmentTitlePlaceholder")} value={r.title} onChange={(e) => set(i, { title: e.target.value })} />
          {pages.length ? (
            <>
              <PagePicker label={t("contentEditor.segmentStart", { n: i + 1 })} pages={pages} value={pages[Number(r.start)]?.id ?? ""}
                onChange={(name) => set(i, { start: String(pages.findIndex((x) => x.id === name)) })} />
              <span aria-hidden>–</span>
              {/* end is exclusive: the picker names the segment's last page. */}
              <PagePicker label={t("contentEditor.segmentEnd", { n: i + 1 })} pages={pages} value={pages[Number(r.end) - 1]?.id ?? ""}
                onChange={(name) => set(i, { end: String(pages.findIndex((x) => x.id === name) + 1) })} />
            </>
          ) : (
            <>
              <Input className="h-8 w-20" type="number" min={0} aria-label={t("contentEditor.segmentStart", { n: i + 1 })} value={r.start} onChange={(e) => set(i, { start: e.target.value })} />
              <span aria-hidden>–</span>
              <Input className="h-8 w-20" type="number" min={1} aria-label={t("contentEditor.segmentEnd", { n: i + 1 })} value={r.end} onChange={(e) => set(i, { end: e.target.value })} />
            </>
          )}
          <Input className="h-8 w-48" type="datetime-local" aria-label={t("contentEditor.segmentRelease", { n: i + 1 })} value={r.release} onChange={(e) => set(i, { release: e.target.value })} />
          <Button type="button" size="xs" variant="ghost" onClick={() => setRows((x) => x.filter((_, j) => j !== i))}>
            {t("common.remove")}
          </Button>
        </div>
      ))}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={() => {
            const last = rows.at(-1);
            const start = last ? Number(last.end) || 0 : 0;
            setRows((x) => [...x, { title: "", start: String(start), end: String(start + 1), release: "" }]);
          }}
        >
          {t("contentEditor.addSegment")}
        </Button>
        {!coordinated && <>
        <Button
          type="button"
          size="sm"
          disabled={save.pending || !valid}
          onClick={()=>void submit()}
        >
          {t("contentEditor.saveSegments")}
        </Button>
        <Saved show={saved} />
        </>}
      </div>
      <ErrorLine error={save.error} />
    </div>
  );
}

/** Every editor section of an existing content: details, access and price, versions, groups. */
export function ContentSections({ content, ...p }: Common & { content: Content; media?: boolean; className?: string; management?: boolean }) {
  const languageName = useLanguageName();
  const versions = useMemo(() => content.versions.map((v) => ({ id: v.id, label: versionLabel(v, languageName, p.labels) })), [content.versions, languageName, p.labels]);
  const { spec } = useKind(content.kind, p.client);
  return (
    <div className={cn("grid gap-6", p.className)} data-ckui="content-sections">
      <ContentDetailsEditor content={content} {...p} />
      {offersAccessChoice(spec) && <AccessEditor content={content} {...p} />}
      <VersionsEditor content={content} {...p} />
      {!!spec?.entities?.length && (
        <section className="grid gap-3 border-t border-border pt-4" data-ckui="content-groups">
          <MembershipEditor management={p.management} content={content.id} contentKind={content.kind} versions={spec.versions.hidden ? undefined : versions} labels={p.labels} client={p.client} onError={p.onError} />
        </section>
      )}
    </div>
  );
}
