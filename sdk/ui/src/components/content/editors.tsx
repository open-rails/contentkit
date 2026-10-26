import { cn } from "cn";
import { chapterRanges } from "../../client/chapters.js";
import { publicationOf, publicationPhase } from "../../client/publication.js";
import { contentAccess, saveContentAccess, selectedEdition } from "../../client/edition-access.js";
import { createPortal } from "react-dom";
import { useCallback, useContext, useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import { Add01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { ratio } from "../../client/aspect.js";
import type { ContentKitClient } from "../../client/client.js";
import { fromLocalInput, parseAttributes, toLocalInput } from "../../client/entity-fields.js";
import type { Content, ContentModel, Rule, SegmentInput, Version, VersionState } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import { useContentKitClient, type ContentKitErrorHandler } from "../../react/context.js";
import { useKinds } from "../../react/entities.js";
import { useChannel } from "../../react/channels.js";
import { ChannelPicker } from "../channels/directory.js";
import { useMediaRead } from "../../react/read.js";
import { usePresets } from "../../react/public.js";
import { useContentScope } from "../../react/use-resource.js";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Switch } from "#ckui/ui/switch";
import { ReaderManifestEditor } from "../admin/reader-manifest.js";
import { PageNotices } from "../admin/page-notices.js";
import { ALL_EDITOR_FILES, editorMedia, PagePicker } from "../admin/page-picker.js";
import { VideoMediaEditor, videoPaths } from "../admin/media-item-panel.js";
import { Skeleton } from "#ckui/ui/skeleton";
import { MediaFolderEditor } from "../media-folder-editor.js";
import { SortableList } from "../sortable-list.js";
import { ErrorLine, useAction } from "../social/parts.js";
import { labelOf, type Labels } from "../entities/parts.js";
import { AttributeFields, formOf } from "../entities/fields.js";
import { MembershipEditor } from "../entities/memberships.js";
import { useDraft } from "../../react/use-draft.js";
import { EditorSaveContext, useEditorSaveTask } from "./editor-save.js";

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

/** Whether staff choose a kind's access: it allows more than one level (none listed allows all). */
export function offersAccessChoice(spec: ContentModel["kinds"][string] | undefined): boolean {
  return !!spec && (!spec.access_levels?.length || spec.access_levels.length > 1);
}

export function Section({
  title,
  hint,
  aside,
  ckui,
  className,
  children,
}: {
  title: ReactNode;
  hint?: ReactNode;
  aside?: ReactNode;
  ckui: string;
  className?: string;
  children: ReactNode;
}) {
  return (
    <section className={cn("rounded-lg border border-border bg-muted/30 p-4 grid gap-4", className)} data-ckui={ckui}>
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="grid gap-1">
          <h3 className="text-sm font-medium">{title}</h3>
          {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
        </div>
        {aside}
      </div>
      {children}
    </section>
  );
}

export function SaveState({ dirty, saved }: { dirty?: boolean; saved?: boolean }) {
  const { t } = useMessages();
  if (dirty) {
    return (
      <span className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
        <span aria-hidden className="size-1.5 rounded-full bg-warning" />
        {t("contentManager.unsaved")}
      </span>
    );
  }
  if (saved) {
    return (
      <span role="status" className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
        <span aria-hidden className="size-1.5 rounded-full bg-success" />
        {t("contentEditor.saved")}
      </span>
    );
  }
  return null;
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
  const dirty = fields.dirty.length > 0 || attrs.dirty.length > 0;
  return (
    <Section title={t("contentEditor.details")} aside={<SaveState dirty={dirty} saved={saved} />} ckui="content-details">
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
        <div className="flex justify-end gap-2 border-t border-border pt-4">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            disabled={save.pending}
            onClick={() => {
              const settleFields = fields.checkpoint(), settleAttrs = attrs.checkpoint();
              void save.run(reload).then((d) => {
                if (!d) return;
                settleFields({ comments: d.comments });
                settleAttrs(formOf(declared, d.attributes));
                setInvalid(null);
                setSaved(false);
              });
            }}
          >
            {t("common.cancel")}
          </Button>
          <Button
            type="submit"
            size="sm"
            variant={dirty ? "default" : "outline"}
            disabled={save.pending}
          >
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
        </div>
      </form>
    </Section>
  );
}

/** A rule's form: level, tiers, channel (the parent item's channel by default), the preview its kind has and optional audience releases. */
export function RuleFields({ value, onChange, model, kind, owner, labels, client }: { value: Rule; onChange: (r: Rule) => void; model: ContentModel | null; kind: string; owner?: string; labels?: Labels; client: ContentKitClient }) {
  const { t } = useMessages();
  const id = useId();
  const tiers = model?.tiers ?? [];
  const spec = model?.kinds[kind];
  const declaredLevels = spec?.access_levels;
  const fixedTier = !!declaredLevels?.length && tiers.length === 1;
  const levels = declaredLevels?.length ? LEVELS.filter((level) => declaredLevels.includes(level)) : LEVELS;
  const hasChannels = !!model?.channels?.entitled;
  const releaseLevels = levels.filter((level) => level === "public" || level === "tier" && tiers.length > 0 || level === "group" && hasChannels);
  const named = useChannel(value.group_id, { client });
  const group = named.channel;
  const set = (patch: Partial<Rule>) => onChange({ ...value, ...patch });
  const pages = spec?.presentation === "reader";
  return (
    <div className="grid gap-3" data-ckui="rule-fields">
      <div className="grid gap-1.5 sm:max-w-xs">
        <Label>{t("contentEditor.access")}</Label>
        <Select
          value={value.level}
          onValueChange={(v) => {
            const level = v as Rule["level"];
            onChange({ level, tiers: level === "tier" ? (fixedTier ? [...tiers] : value.tiers) : undefined, group_id: grouped(level) ? value.group_id || owner : undefined, preview: value.preview, releases: level === "public" ? undefined : value.releases });
          }}
        >
          <SelectTrigger size="sm" aria-label={t("contentEditor.access")}>
            <SelectValue>{(v: string) => t(`contentEditor.level_${v as Rule["level"]}`)}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {levels.filter((l) => (l !== "tier" || tiers.length) && (!l.startsWith("group") || hasChannels)).map((l) => (
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
          {hasChannels && !owner && <ChannelPicker label={t("channels.channel")} client={client} onPick={(channel) => set({ group_id: channel.id })} />}

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
        </div>
      )}
      {value.level !== "public" && <fieldset className="grid gap-3">
        <legend>{t("contentEditor.releases")}</legend>
        {(value.releases ?? []).map((release, index) => {
          const change = (patch: Partial<typeof release>) => set({ releases: value.releases!.map((entry, i) => i === index ? { ...entry, ...patch } : entry) });
          return <div key={index} className="flex flex-wrap items-center gap-2">
            <Input required type="datetime-local" aria-label={t("contentEditor.releaseAt", { n: index + 1 })} value={toLocalInput(release.at)} onChange={(e) => change({ at: fromLocalInput(e.target.value) ?? "" })} />
            <Select value={release.level} onValueChange={(v) => change({ level: v as typeof release.level, tiers: v === "tier" ? tiers.slice(0, 1) : undefined, group_id: undefined })}>
              <SelectTrigger size="sm" aria-label={t("contentEditor.releaseAudience", { n: index + 1 })}><SelectValue>{(v: string) => t(`contentEditor.level_${v as Rule["level"]}`)}</SelectValue></SelectTrigger>
              <SelectContent>{releaseLevels.map((level) => <SelectItem key={level} value={level}>{t(`contentEditor.level_${level as Rule["level"]}`)}</SelectItem>)}</SelectContent>
            </Select>
            {release.level === "tier" && tiers.map((tier) => <label key={tier} className="flex items-center gap-1"><Switch aria-label={labelOf(labels, tier)} checked={release.tiers?.includes(tier) ?? false} onCheckedChange={(on) => change({ tiers: on ? [...(release.tiers ?? []), tier] : release.tiers?.filter((t) => t !== tier) })} />{labelOf(labels, tier)}</label>)}
            {release.level === "group" && <><span>{release.group_id}</span><ChannelPicker label={t("channels.channel")} client={client} onPick={(channel) => change({ group_id: channel.id })} /></>}
            <Button type="button" variant="ghost" size="sm" onClick={() => set({ releases: value.releases!.filter((_, i) => i !== index) })}>{t("contentEditor.removeRelease")}</Button>
          </div>;
        })}
        <Button type="button" variant="outline" size="sm" disabled={!releaseLevels.length || (value.releases?.length ?? 0) >= 32} onClick={() => { const level = releaseLevels[0] ?? "public"; set({ releases: [...(value.releases ?? []), { at: "", level, tiers: level === "tier" ? tiers.slice(0, 1) : undefined }] }); }}>{t("contentEditor.addRelease")}</Button>
      </fieldset>}
    </div>
  );
}

/** A rule the server takes: a channel level names its channel. */
export const complete = (r: Rule) => (!grouped(r.level) || !!r.group_id) && (r.releases ?? []).every((release) => !!release.at && (release.level !== "tier" || !!release.tiers?.length) && (release.level !== "group" || !!release.group_id));

/** A rule as stored: empty previews and absent fields left out. */
export function cleanRule(r: Rule): Rule {
  const preview = r.preview?.pages ? { pages: r.preview.pages } : undefined;
  return {
    level: r.level,
    tiers: r.level === "tier" ? r.tiers : undefined,
    group_id: r.level === "group" || r.level === "group_ppv" ? r.group_id : undefined,
    preview: r.level === "public" ? undefined : preview,
    releases: r.level === "public" ? undefined : r.releases,
  };
}

/** Who may open the content and what it sells for. */
export function AccessEditor(p: Common & { content: Content }) {
  const { client, scope } = useContentScope(p.client);
  const key = useEditorKey(`${p.content.id}:${selectedEdition(p.content)?.id ?? ""}`, client, scope);
  return <AccessForm key={key} {...p} client={client} />;
}

function AccessForm({ content, ...p }: Common & { content: Content }) {
  const { t } = useMessages();
  const client = useContentKitClient(p.client);
  const { model } = useKind(content.kind, p.client);
  const version = selectedEdition(content);
  const rule = useDraft(contentAccess(content) ?? (model ? defaultAccessRule(model, content.kind) : { level: "public" } as Rule));
  const reload = useEditorReload(content, p.client);
  const [saved, setSaved] = useState(false);
  const save = useAction("content.save", p.onError);
  const dirty = rule.dirty.length > 0;
  return (
    <Section title={t("contentEditor.accessOnlyTitle")} aside={<SaveState dirty={dirty} saved={saved} />} ckui="access-editor">
      <form
        className="grid gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          if (!content.channel_id && !version) return;
          setSaved(false);
          const settle = rule.checkpoint();
          void save.run(() => saveContentAccess(client.contents, content, cleanRule(rule.value), version?.id)).then((d) => d && setSaved(settle(contentAccess(d, version?.id) ?? rule.value)));
        }}
      >
        <RuleFields value={rule.value} onChange={(r) => { rule.edit(r); setSaved(false); }} model={model} kind={content.kind} owner={content.channel_id} labels={p.labels} client={client} />
        <ErrorLine error={save.error} />
        <div className="flex justify-end gap-2 border-t border-border pt-4">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            disabled={save.pending}
            onClick={() => {
              const settle = rule.checkpoint();
              void save.run(reload).then((d) => {
                if (!d) return;
                settle(contentAccess(d, version?.id) ?? rule.value);
                setSaved(false);
              });
            }}
          >
            {t("common.cancel")}
          </Button>
          <Button
            type="submit"
            size="sm"
            variant={dirty && complete(rule.value) ? "default" : "outline"}
            disabled={save.pending || !version || !complete(rule.value)}
          >
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
        </div>
      </form>
    </Section>
  );
}

/** A version's name in the editor: its label, else its languages and traits. */
export function versionLabel(v: Version, languageName: (l: string) => string, labels?: Labels): string {
  return v.label || [...(v.languages ?? []).map(languageName), ...(v.traits ?? []).map((x) => labelOf(labels, x))].join(" · ");
}

export function VersionStatus({ version }: { version: Version }) {
  const { t } = useMessages();
  if (version.state === "deleted") {
    return <span className="text-sm text-muted-foreground" data-ckui="version-status" data-state="deleted">{t("contentManager.deleted")}</span>;
  }
  const phase = publicationPhase(version);
  const color = version.live ? "bg-success" : version.state === "draft" ? "bg-muted-foreground/50" : "bg-sky-500";
  const label = version.live ? t("contentEditor.live") : version.state === "draft" ? t("contentManager.draft")
    : phase ? phase === "scheduled" ? t("contentManager.scheduled") : t(`contentEditor.publication_${phase}`)
      : version.publish_at && Date.parse(version.publish_at) > Date.now() ? t("contentManager.scheduled") : t("readerEditor.ready");
  return (
    <span className="inline-flex items-center gap-1.5 text-sm text-foreground" data-ckui="version-status" data-state={version.live ? "live" : version.state}>
      <span aria-hidden className={cn("size-2 shrink-0 rounded-full", color)} />
      {label}
      {version.live && phase && <span className="text-muted-foreground">· {phase === "scheduled" ? t("contentManager.scheduled") : t(`contentEditor.publication_${phase}`)}</span>}
    </span>
  );
}

/**
 * A content's versions: order, languages, traits, label, state and
 * schedule, an access override, segments (chapters) and
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
        {media && <VersionMedia content={content} version={only} model={model} bare {...p} />}
      </Section>
    );
  }
  const languages = model?.languages ?? [];
  const activeVersions = versions.filter((version) => version.state !== "deleted");
  const deletedVersions = versions.filter((version) => version.state === "deleted");
  const versionRow = (v: Version, handle?: ReactNode) => (
    <div className="grid gap-2 rounded-md border border-border px-3 py-2" data-ckui="version-row" data-version={v.id}>
      <div className="flex flex-wrap items-center gap-2">
        {activeVersions.length > 1 && handle}
        <span className="min-w-0 flex-1 break-words">
          <span className="font-medium">{versionLabel(v, languageName, p.labels)}</span>
          <span className="ms-2 font-mono text-xs text-muted-foreground">{v.code}</span>
        </span>
        <VersionStatus version={v} />
        {v.access && <span className="text-sm text-muted-foreground">{t(`contentEditor.level_${v.access.level}`)}</span>}
        {v.pages ? <span className="text-xs text-muted-foreground">{t("contentEditor.pageCount", { count: v.pages })}</span> : null}
        <Button type="button" size="sm" variant="outline" aria-expanded={open === v.id} onClick={() => open === v.id ? setOpen(null) : reveal(v.id)}>
          {t(open === v.id ? "contentEditor.close" : "contentEditor.edit")}
        </Button>
      </div>
      {opened.includes(v.id) && <div hidden={open !== v.id} className="-mx-3 mt-1 border-t border-border px-3 pt-2"><VersionForm content={content} version={v} model={model} media={media} {...p} /></div>}
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
        collapsible
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
  // The visible label lives here, so callers don't add their own.
  if (!many)
    return (
      <div className="grid gap-1.5">
        <span aria-hidden className="text-sm font-medium">{t("contentEditor.versionLanguage")}</span>
        <Select value={value[0] ?? NONE} onValueChange={(v) => onChange(v === NONE ? [] : [String(v)])}>
          <SelectTrigger size="sm" className="w-fit" aria-label={t("contentEditor.versionLanguage")}>
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
      </div>
    );
  return (
    <fieldset className="grid gap-1.5">
      <legend className="mb-1.5 text-sm font-medium">{t("contentEditor.versionLanguages")}</legend>
      <div className="flex flex-wrap gap-3">
        {languages.map((l) => (
          <label key={l} className="flex items-center gap-1.5">
            <Switch size="sm" checked={value.includes(l)} onCheckedChange={(on) => onChange(on ? [...value, l] : value.filter((x) => x !== l))} aria-label={languageName(l)} />
            {languageName(l)}
          </label>
        ))}
      </div>
    </fieldset>
  );
}

export function AddVersion({ languages, many, pending, onAdd, heading = true, collapsible = false }: {
  languages: readonly string[];
  many: boolean;
  pending: boolean;
  onAdd: (v: { languages: string[] }) => void;
  heading?: boolean;
  collapsible?: boolean;
}) {
  const { t } = useMessages();
  const [shown, setShown] = useState(!collapsible);
  const [langs, setLangs] = useState<string[]>([]);
  // In its own card section it gets a divider; inside a dialog (heading off) the dialog frames it.
  const frame = cn("grid gap-3", heading && "border-t border-border pt-4");
  if (!shown)
    return (
      <div className={frame} data-ckui="add-version">
        <Button type="button" size="sm" variant="outline" className="justify-self-start" onClick={() => setShown(true)}>
          <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
          {t("contentEditor.addVersion")}
        </Button>
      </div>
    );
  return (
    <div className={frame} data-ckui="add-version">
      {heading && (
        <div className="grid gap-0.5">
          <p className="text-sm font-medium">{t("contentEditor.addVersion")}</p>
          <p className="text-xs text-muted-foreground">{t("contentEditor.addVersionHint")}</p>
        </div>
      )}
      <LanguageFields languages={languages} value={langs} many={many} onChange={setLangs} />
      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={pending || !langs.length}
          onClick={() => {
            onAdd({ languages: langs });
            setLangs([]);
            if (collapsible) setShown(false);
          }}
        >
          {t("contentEditor.addVersion")}
        </Button>
        {collapsible && (
          <Button type="button" size="sm" variant="ghost" onClick={() => { setLangs([]); setShown(false); }}>
            {t("common.cancel")}
          </Button>
        )}
      </div>
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
  const seed = useCallback((v: Version) => ({langs:v.languages,traits:v.traits,at:toLocalInput(v.publish_at),rule:v.access??content.access}),[content.access]);
  const fields = useDraft(useMemo(()=>seed(v),[seed,v]));
  const {langs,traits,at,rule}=fields.value;
  useEffect(() => { onScheduleChange?.(at); }, [at, onScheduleChange]);
  useEffect(() => { onLanguagesChange?.(langs); }, [langs, onLanguagesChange]);
  const edit=(patch:Partial<typeof fields.value>)=>{fields.edit((old)=>({...old,...patch}));setSaved(false);};
  const [saved, setSaved] = useState(false);
  const save = useAction("content.save", p.onError);
  const remove = useAction("content.save", p.onError);
  const valid=langs.length>0&&complete(rule);
  const submit=async(state?: VersionState)=>{
    if(!valid)return false;
    setSaved(false);
    const settle=fields.checkpoint();
    const accessChanged = !content.channel_id && fields.dirty.includes("rule");
    const out=await save.run(()=>client.contents.updateVersion(content.id,v.id,{languages:langs,traits,publish_at:fromLocalInput(at),...(state?{state}:{}),...(accessChanged?{access:cleanRule(rule)}:{})}));
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
    <Button type="button" size="sm" variant="ghost" className="ms-auto text-destructive hover:text-destructive" disabled={remove.pending} onClick={() => void remove.run(async () => {
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
  // A player version reads as one task, top to bottom: its video, then chapters, then its
  // settings and the Save/Publish that applies to them. Reader works keep settings alone.
  const segments = chapters && !!spec?.segments;
  return (
    <div className="grid gap-4 pt-2">

      {media && <VersionMedia content={content} version={v} model={model} {...p} />}
      {media && (
        <div className="border-t border-border pt-4">
          <VersionCover content={content} version={v} {...p} />
        </div>
      )}
      {media && spec?.presentation === "reader" && (
        <div className="border-t border-border pt-4">
          <ReaderManifestEditor kind={content.kind} content={content.id} version={v.id} client={p.client} onError={p.onError} />
        </div>
      )}
      {segments && (
        <div className={cn(media && "border-t border-border pt-4")}>
          <SegmentsEditor content={content} version={v} {...p} />
        </div>
      )}
      <div className={cn("grid gap-3", (media || segments) && "border-t border-border pt-4")} data-ckui="version-settings">
        {media && (
          <div className="grid gap-0.5">
            <h4 className="text-sm font-medium">{t("contentEditor.versionSettings")}</h4>
            <p className="text-xs text-muted-foreground">{t("contentEditor.versionSettingsHint")}</p>
          </div>
        )}
        <form className="grid gap-3" onSubmit={(event)=>{event.preventDefault();if (coordinated) event.stopPropagation();else void submit();}}>
          <LanguageFields languages={model?.languages ?? []} value={langs} many={spec?.versions.languages === "many"} onChange={(langs)=>edit({langs})} />
          {showTraits && !!spec?.versions.traits?.length && (
            <fieldset className="grid gap-1.5">
              <legend className="mb-1.5 text-sm font-medium">{t("contentEditor.traits")}</legend>
              <div className="flex flex-wrap gap-3">
                {spec.versions.traits.map((x) => (
                  <label key={x} className="flex items-center gap-1.5">
                    <Switch size="sm" checked={traits.includes(x)} onCheckedChange={(on) => edit({traits:on ? [...traits, x] : traits.filter((y) => y !== x)})} aria-label={labelOf(p.labels, x)} />
                    {labelOf(p.labels, x)}
                  </label>
                ))}
              </div>
            </fieldset>
          )}
          {scheduleTarget === undefined ? schedule : scheduleTarget && createPortal(schedule, scheduleTarget)}
          {!content.channel_id && <RuleFields value={rule} onChange={(rule)=>edit({rule})} model={model} kind={content.kind} labels={p.labels} client={client} />}
          {publicationOf(v)?.error && <ErrorLine>{publicationOf(v)!.error}</ErrorLine>}
          <ErrorLine error={save.error} />
          <div className="flex flex-wrap items-center gap-3">
            {publicationPhase(v) === "failed" && <Button type="button" size="sm" variant="outline" disabled={save.pending || !valid} onClick={() => void submit("published")}>{t("common.retry")}</Button>}
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
      </div>
      {readerToolsTarget && readingLayout && createPortal(readingLayout, readerToolsTarget)}

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
  const preset = presets.find((r) => r.kind === content.kind && r.name === "cover");
  if (!preset) return null;
  const src = fresh ?? v.cover?.url;
  // The kind's cover shape: wide for players, portrait for reader works.
  const shape = ratio(preset.aspect) ?? 16 / 9;
  const box = cn("shrink-0 rounded-md border border-border bg-muted", shape >= 1 ? "w-40" : "w-24");
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
        {src ? (
          <img src={src} alt="" className={cn(box, "object-cover")} style={{ aspectRatio: String(shape) }} />
        ) : (
          <span className={cn(box, "grid place-items-center text-xs text-muted-foreground")} style={{ aspectRatio: String(shape) }}>
            {t("contentManager.noCover")}
          </span>
        )}
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
// bare: inside a card that already says "Media", so the folder adds no heading of its own.
function VersionMedia({ content, version, model, bare, ...p }: Common & { content: Content; version: Version; model: ContentModel | null; bare?: boolean }) {
  const { t } = useMessages();
  const languageName = useLanguageName();
  const item = useMemo(() => ({ kind: content.kind, id: content.id, version: version.id }), [content.kind, content.id, version.id]);
  const r = useMediaRead(item, { editor: true, client: p.client });
  const languages = useMemo(() => (model?.languages ?? []).map((l) => ({ value: l, label: languageName(l) })), [model, languageName]);
  if (!r.read && r.loading) return <Skeleton className="h-24 w-full" />;
  if (r.read?.uploads && videoPaths(r.read.uploads).source) return <VideoMediaEditor item={item} languages={languages} client={p.client} onError={p.onError} />;
  return <>
    <MediaFolderEditor item={item} commit={version.live ? "manual" : "auto"} label={bare ? false : t("contentEditor.media")} client={p.client} onError={p.onError} />
    {model?.kinds[content.kind]?.presentation === "reader" && <PageNotices content={content.id} version={version.id} client={p.client} onError={p.onError} />}
  </>;
}

type SegmentsEditorProps = Common & { content: Content; version: Version; heading?: boolean };

/** Reader chapters use virtual filename start markers in current manifest order. */
export function SegmentsEditor(p: SegmentsEditorProps) {
  const { spec } = useKind(p.content.kind, p.client);
  if (!spec) return <Skeleton className="h-24 w-full" />;
  return <ReaderChaptersEditor {...p} />;
}

type ChapterRow = { id?: string; title: string; startPage: string; release: string };

function ReaderChaptersEditor({ content, version, heading = true, ...p }: SegmentsEditorProps) {
  const { t } = useMessages();
  const coordinated = useContext(EditorSaveContext) !== null;
  const client = useContentKitClient(p.client);
  const read = useMediaRead({ kind: content.kind, id: content.id, version: version.id }, { editor: true, window: ALL_EDITOR_FILES, client });
  const pages = useMemo(() => editorMedia(read.read, "path").pages, [read.read]);
  const rowsOf = useCallback((value: Version): ChapterRow[] => value.segments.map((segment) => ({
    id: "id" in segment && typeof segment.id === "string" ? segment.id : undefined,
    title: segment.title ?? "",
    startPage: segment.start_page || pages[segment.start]?.id || "",
    release: toLocalInput(segment.release_at),
  })), [pages]);
  const fields = useDraft(useMemo(() => ({ rows: rowsOf(version) }), [rowsOf, version]));
  const rows = fields.value.rows;
  const { ranges, order, problem } = chapterRanges(pages.map((page) => page.id), rows.map((row) => row.startPage));
  const valid = problem === null;
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
    const segments: SegmentInput[] = order.map((index, position) => {
      const row = rows[index]!;
      return { id: row.id, position, title: row.title.trim() || undefined, start_page: row.startPage, release_at: fromLocalInput(row.release) };
    });
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
    <p className="text-sm text-muted-foreground">{t("contentEditor.chapterMarkersHint")}</p>
    {rows.length === 0 && <p className="text-muted-foreground">{t("contentEditor.noSegments")}</p>}
    {order.map((i, position) => {
      const row = rows[i]!;
      const { start, end } = ranges[i]!;
      return <div key={row.id ?? i} className="flex flex-wrap items-center gap-2" data-ckui="segment-row">
        <span className="w-6 text-xs tabular-nums text-muted-foreground">{position + 1}</span>
        <Input className="h-8 w-40" aria-label={t("contentEditor.segmentTitle", { n: position + 1 })} placeholder={t("contentEditor.segmentTitlePlaceholder")} value={row.title} onChange={(e) => edit(i, { title: e.target.value })} />
        <PagePicker label={t("contentEditor.segmentStart", { n: position + 1 })} pages={pages} value={row.startPage} onChange={(startPage) => edit(i, { startPage })} />
        <output className="text-sm tabular-nums text-muted-foreground" data-ckui="chapter-range">{start >= 0 && end > start ? t("contentEditor.chapterRange", { from: start + 1, to: end }) : "—"}</output>
        <Input className="h-8 w-48" type="datetime-local" aria-label={t("contentEditor.segmentRelease", { n: position + 1 })} value={row.release} onChange={(e) => edit(i, { release: e.target.value })} />
        <Button type="button" size="xs" variant="ghost" onClick={() => { setSaved(false); fields.edit((previous) => ({ rows: previous.rows.filter((_, j) => j !== i) })); }}>{t("common.remove")}</Button>
      </div>;
    })}
    {problem === "unselected" && <ErrorLine>{t("contentEditor.chapterStartRequired")}</ErrorLine>}
    {problem === "missing" && <ErrorLine>{t("contentEditor.chapterStartMissing")}</ErrorLine>}
    {problem === "duplicate" && <ErrorLine>{t("contentEditor.chapterStartAmbiguous")}</ErrorLine>}
    <div className="flex flex-wrap items-center gap-2">
      <Button type="button" size="sm" variant="outline" onClick={() => {
        const last = order.at(-1);
        const next = last === undefined ? 0 : ranges[last]!.start + 1;
        setSaved(false);
        fields.edit((previous) => ({ rows: [...previous.rows, { title: "", startPage: pages[next]?.id ?? "", release: "" }] }));
      }}>{t("contentEditor.addSegment")}</Button>
      {!coordinated && <>
        <Button type="button" size="sm" variant="outline" disabled={save.pending || !valid} onClick={() => void submit()}>{t("contentEditor.saveSegments")}</Button>
        <Saved show={saved} />
      </>}
    </div>
    <ErrorLine error={read.error} />
    <ErrorLine error={save.error} />
  </div>;
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
