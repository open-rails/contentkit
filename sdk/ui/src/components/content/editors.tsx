import { cn } from "cn";
import { useEffect, useId, useMemo, useState, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { fromLocalInput, parseAttributes, toLocalInput } from "../../client/group-fields.js";
import type { Content, ContentModel, Listing, Rule, Segment, Version } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useContentKitClient, type ContentKitErrorHandler } from "../../react/context.js";
import { useKinds } from "../../react/groups.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Switch } from "#ckui/ui/switch";
import { MediaFolderEditor } from "../media-folder-editor.js";
import { SortableList } from "../sortable-list.js";
import { ErrorLine, useAction } from "../social/parts.js";
import { GroupPicker, labelOf, useGroupNames, useLanguageName, type Labels } from "../groups/parts.js";
import { AttributeFields, formOf } from "../groups/fields.js";
import { MembershipEditor } from "../groups/memberships.js";
import { OfferFields } from "./offer-fields.js";

interface Common {
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  labels?: Labels;
}

const NONE = "__none__";
const LISTINGS: readonly Listing[] = ["listed", "members", "unlisted"];
const COMMENTS = ["open", "closed", "hidden"] as const;
type Comments = (typeof COMMENTS)[number];
const LEVELS: readonly Rule["level"][] = ["public", "tier", "ppv", "group", "group_ppv"];
const grouped = (l: Rule["level"]) => l === "group" || l === "group_ppv";

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

/** Listing and the kind's declared attributes. */
export function ContentDetailsEditor({ content, ...p }: Common & { content: Content }) {
  const { t } = useMessages();
  const client = useContentKitClient(p.client);
  const { spec } = useKind(content.kind, p.client);
  const declared = useMemo(() => spec?.attributes ?? {}, [spec]);
  const [listing, setListing] = useState<Listing>(content.listing);
  const [comments, setComments] = useState(content.comments);
  const [attrs, setAttrs] = useState<Record<string, string | boolean>>({});
  useEffect(() => {
    setListing(content.listing);
    setComments(content.comments);
    setAttrs(formOf(declared, content.attributes));
  }, [content.revision, content.listing, content.comments, content.attributes, declared]);
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
            attributes = parseAttributes(declared, attrs);
          } catch (err) {
            return setInvalid((err as Error).message);
          }
          void save.run(() => client.contents.update(content.id, { listing, comments, attributes, revision: content.revision })).then((d) => d && setSaved(true));
        }}
      >
        <div className="grid gap-1.5 sm:max-w-xs">
          <Label>{t("contentEditor.listing")}</Label>
          <Select value={listing} onValueChange={(v) => setListing(v as Listing)}>
            <SelectTrigger size="sm" aria-label={t("contentEditor.listing")}>
              <SelectValue>{(v: string) => t(`contentEditor.listing_${v as Listing}`)}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {LISTINGS.map((l) => (
                <SelectItem key={l} value={l}>
                  {t(`contentEditor.listing_${l}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <div className="grid gap-1.5 sm:max-w-xs">
          <Label>{t("contentEditor.comments")}</Label>
          <Select value={comments} onValueChange={(v) => setComments(v as Comments)}>
            <SelectTrigger size="sm" aria-label={t("contentEditor.comments")}>
              <SelectValue>{(v: string) => t(`contentEditor.comments_${v as Comments}`)}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {COMMENTS.map((c) => (
                <SelectItem key={c} value={c}>
                  {t(`contentEditor.comments_${c}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <AttributeFields declared={declared} form={attrs} onChange={(n, v) => setAttrs((a) => ({ ...a, [n]: v }))} labels={p.labels} />
        {invalid && <ErrorLine>{invalid}</ErrorLine>}
        <ErrorLine error={save.error} />
        <div className="flex items-center gap-3">
          <Button type="submit" size="sm" disabled={save.pending}>
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
          <Saved show={saved} />
        </div>
      </form>
    </Section>
  );
}

/** A rule's form: level, tiers, group (the content's owner group by default), the preview its kind has and when it turns free. */
function RuleFields({ value, onChange, model, kind, owner, labels, client }: { value: Rule; onChange: (r: Rule) => void; model: ContentModel | null; kind: string; owner?: string; labels?: Labels; client: ContentKitClient }) {
  const { t } = useMessages();
  const id = useId();
  const tiers = model?.tiers ?? [];
  const spec = model?.kinds[kind];
  const groupKinds = Object.entries(model?.group_kinds ?? {})
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
            onChange({ level, group_id: grouped(level) ? value.group_id || owner : undefined, preview: value.preview, free_after: value.free_after });
          }}
        >
          <SelectTrigger size="sm" aria-label={t("contentEditor.access")}>
            <SelectValue>{(v: string) => t(`contentEditor.level_${v as Rule["level"]}`)}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {LEVELS.filter((l) => (l !== "tier" || tiers.length) && (!l.startsWith("group") || groupKinds.length)).map((l) => (
              <SelectItem key={l} value={l}>
                {t(`contentEditor.level_${l}`)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      {value.level === "tier" && (
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
const complete = (r: Rule) => !grouped(r.level) || !!r.group_id;

/** A rule as stored: empty previews and absent fields left out. */
function cleanRule(r: Rule): Rule {
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
export function AccessEditor({ content, ...p }: Common & { content: Content }) {
  const { t } = useMessages();
  const client = useContentKitClient(p.client);
  const { model } = useKind(content.kind, p.client);
  const [rule, setRule] = useState<Rule>(content.access);
  useEffect(() => setRule(content.access), [content.revision, content.access]);
  const [saved, setSaved] = useState(false);
  const save = useAction("content.save", p.onError);
  const offer = useAction("offer.save", p.onError);
  return (
    <Section title={t("contentEditor.accessTitle")} ckui="access-editor">
      <form
        className="grid gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          setSaved(false);
          void save.run(() => client.contents.update(content.id, { access: cleanRule(rule), revision: content.revision })).then((d) => d && setSaved(true));
        }}
      >
        <RuleFields value={rule} onChange={setRule} model={model} kind={content.kind} owner={content.owner_group_id} labels={p.labels} client={client} />
        <ErrorLine error={save.error} />
        <div className="flex items-center gap-3">
          <Button type="submit" size="sm" disabled={save.pending || !complete(rule)}>
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
          <Saved show={saved} />
        </div>
      </form>
      <OfferFields
        offer={content.offer}
        pending={offer.pending}
        onSell={(terms) => void offer.run(() => client.offers.sell({ content: content.id }, terms))}
        onWithdraw={() => void offer.run(() => client.offers.withdraw({ content: content.id }))}
      />
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
export function VersionsEditor({ content, media = true, ...p }: Common & { content: Content; media?: boolean }) {
  const { t } = useMessages();
  const client = useContentKitClient(p.client);
  const languageName = useLanguageName();
  const { model, spec } = useKind(content.kind, p.client);
  const order = useAction("content.save", p.onError);
  const add = useAction("content.save", p.onError);
  const [open, setOpen] = useState<string | null>(null);
  const versions = useMemo(() => [...content.versions].sort((a, b) => a.position - b.position), [content.versions]);
  if (!spec) return null;
  if (spec.versions.hidden) {
    const only = versions[0];
    if (!only || (!media && !spec.segments)) return null;
    return (
      <Section title={t("contentEditor.media")} ckui="version-media">
        {spec.segments && <SegmentsEditor content={content} version={only} {...p} />}
        {media && <MediaFolderEditor item={{ kind: content.kind, id: content.id, version: only.id }} label={t("contentEditor.media")} />}
      </Section>
    );
  }
  const languages = model?.languages ?? [];
  return (
    <Section title={t("contentEditor.versions")} ckui="versions-editor">
      <SortableList
        items={versions}
        id={(v) => v.id}
        name={(v) => versionLabel(v, languageName, p.labels)}
        label={t("contentEditor.versions")}
        disabled={order.pending || versions.length < 2}
        onMove={(from, to) => {
          const ids = versions.map((v) => v.id);
          const [moved] = ids.splice(from, 1);
          ids.splice(to, 0, moved!);
          void order.run(() => client.contents.orderVersions(content.id, ids));
        }}
      >
        {(v, handle) => (
          <div className="grid gap-2 rounded-md border border-border p-2" data-ckui="version-row" data-version={v.id}>
            <div className="flex flex-wrap items-center gap-2">
              {versions.length > 1 && handle}
              <span className="min-w-0 flex-1 font-medium break-words">{versionLabel(v, languageName, p.labels)}</span>
              <Badge variant={v.live ? "default" : "outline"}>{t(v.live ? "contentEditor.live" : v.state === "draft" ? "contentManager.draft" : "contentManager.scheduled")}</Badge>
              {v.access && <Badge variant="secondary">{t(`contentEditor.level_${v.access.level}`)}</Badge>}
              {v.pages ? <span className="text-xs text-muted-foreground">{t("contentEditor.pageCount", { count: v.pages })}</span> : null}
              <Button type="button" size="xs" variant="ghost" aria-expanded={open === v.id} onClick={() => setOpen(open === v.id ? null : v.id)}>
                {t(open === v.id ? "contentEditor.close" : "contentEditor.edit")}
              </Button>
            </div>
            {open === v.id && <VersionForm content={content} version={v} model={model} media={media} {...p} />}
          </div>
        )}
      </SortableList>
      <ErrorLine error={order.error} />
      <AddVersion languages={languages} many={spec?.versions.languages === "many"} pending={add.pending} onAdd={(input) => void add.run(() => client.contents.addVersion(content.id, input))} />
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

function AddVersion({ languages, many, pending, onAdd }: { languages: readonly string[]; many: boolean; pending: boolean; onAdd: (v: { languages: string[] }) => void }) {
  const { t } = useMessages();
  const [langs, setLangs] = useState<string[]>([]);
  return (
    <div className="flex flex-wrap items-center gap-2" data-ckui="add-version">
      <LanguageFields languages={languages} value={langs} many={many} onChange={setLangs} />
      <Button
        type="button"
        size="sm"
        variant="outline"
        disabled={pending || !langs.length}
        onClick={() => {
          onAdd({ languages: langs });
          setLangs([]);
        }}
      >
        {t("contentEditor.addVersion")}
      </Button>
    </div>
  );
}

function VersionForm({ content, version: v, model, media, ...p }: Common & { content: Content; version: Version; model: ContentModel | null; media: boolean }) {
  const { t } = useMessages();
  const id = useId();
  const client = useContentKitClient(p.client);
  const spec = model?.kinds[content.kind];
  const [langs, setLangs] = useState<string[]>(v.languages);
  const [traits, setTraits] = useState<string[]>(v.traits);
  const [label, setLabel] = useState(v.label ?? "");
  const [draft, setDraft] = useState(v.state === "draft");
  const [at, setAt] = useState(toLocalInput(v.publish_at));
  const [own, setOwn] = useState(!!v.access);
  const [rule, setRule] = useState<Rule>(v.access ?? content.access);
  const [saved, setSaved] = useState(false);
  const save = useAction("content.save", p.onError);
  const remove = useAction("content.save", p.onError);
  const offer = useAction("offer.save", p.onError);
  return (
    <div className="grid gap-4 pt-2">
      <form
        className="grid gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          setSaved(false);
          void save
            .run(() =>
              client.contents.updateVersion(content.id, v.id, {
                languages: langs,
                traits,
                label: label.trim(),
                state: draft ? "draft" : "published",
                publish_at: fromLocalInput(at),
                ...(own ? { access: cleanRule(rule) } : { clear_access: !!v.access }),
              }),
            )
            .then((d) => d && setSaved(true));
        }}
      >
        <LanguageFields languages={model?.languages ?? []} value={langs} many={spec?.versions.languages === "many"} onChange={setLangs} />
        {!!spec?.versions.traits?.length && (
          <fieldset className="flex flex-wrap gap-3">
            <legend className="sr-only">{t("contentEditor.traits")}</legend>
            {spec.versions.traits.map((x) => (
              <label key={x} className="flex items-center gap-1.5">
                <Switch size="sm" checked={traits.includes(x)} onCheckedChange={(on) => setTraits(on ? [...traits, x] : traits.filter((y) => y !== x))} aria-label={labelOf(p.labels, x)} />
                {labelOf(p.labels, x)}
              </label>
            ))}
          </fieldset>
        )}
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="grid gap-1.5">
            <Label htmlFor={`${id}-label`}>{t("contentEditor.versionLabel")}</Label>
            <Input id={`${id}-label`} className="h-8" value={label} onChange={(e) => setLabel(e.target.value)} />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor={`${id}-at`}>{t("contentEditor.versionPublishAt")}</Label>
            <Input id={`${id}-at`} className="h-8" type="datetime-local" value={at} onChange={(e) => setAt(e.target.value)} />
          </div>
        </div>
        <label className="flex items-center gap-2">
          <Switch size="sm" checked={draft} onCheckedChange={setDraft} />
          {t("contentEditor.versionDraft")}
        </label>
        <label className="flex items-center gap-2">
          <Switch size="sm" checked={own} onCheckedChange={setOwn} />
          {t("contentEditor.ownAccess")}
        </label>
        {own && <RuleFields value={rule} onChange={setRule} model={model} kind={content.kind} owner={content.owner_group_id} labels={p.labels} client={client} />}
        <ErrorLine error={save.error} />
        <div className="flex flex-wrap items-center gap-3">
          <Button type="submit" size="sm" disabled={save.pending || !langs.length || (own && !complete(rule))}>
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
          <Saved show={saved} />
          <Button type="button" size="sm" variant="destructive" className="ms-auto" disabled={remove.pending} onClick={() => void remove.run(() => client.contents.removeVersion(content.id, v.id))}>
            {t("contentEditor.deleteVersion")}
          </Button>
        </div>
        <ErrorLine error={remove.error} />
      </form>
      <OfferFields
        offer={v.offer}
        pending={offer.pending}
        onSell={(terms) => void offer.run(() => client.offers.sell({ content: content.id, version: v.id }, terms))}
        onWithdraw={() => void offer.run(() => client.offers.withdraw({ content: content.id, version: v.id }))}
      />
      <ErrorLine error={offer.error} />
      {spec?.segments && <SegmentsEditor content={content} version={v} {...p} />}
      {media && <MediaFolderEditor item={{ kind: content.kind, id: content.id, version: v.id }} label={t("contentEditor.media")} />}
    </div>
  );
}

type Row = { title: string; start: string; end: string; release: string };

/** A version's segments (chapters): page ranges with titles and release times. */
export function SegmentsEditor({ content, version, ...p }: Common & { content: Content; version: Version }) {
  const { t } = useMessages();
  const client = useContentKitClient(p.client);
  const seed = () => version.segments.map((s) => ({ title: s.title ?? "", start: String(s.start), end: String(s.end), release: toLocalInput(s.release_at) }));
  const [rows, setRows] = useState<Row[]>(seed);
  // Seeded again when the stored segments change.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => setRows(seed()), [JSON.stringify(version.segments)]);
  const [saved, setSaved] = useState(false);
  const save = useAction("content.save", p.onError);
  const set = (i: number, patch: Partial<Row>) => {
    setSaved(false);
    setRows((r) => r.map((x, j) => (j === i ? { ...x, ...patch } : x)));
  };
  const valid = rows.every((r) => /^\d+$/.test(r.start) && /^\d+$/.test(r.end) && Number(r.end) > Number(r.start));
  return (
    <div className="grid gap-2" data-ckui="segments-editor">
      <h4 className="text-sm font-medium">{t("contentEditor.segments")}</h4>
      {rows.length === 0 && <p className="text-muted-foreground">{t("contentEditor.noSegments")}</p>}
      {rows.map((r, i) => (
        <div key={i} className="flex flex-wrap items-center gap-2" data-ckui="segment-row">
          <span className="w-6 text-xs text-muted-foreground tabular-nums">{i + 1}</span>
          <Input className="h-8 w-40" aria-label={t("contentEditor.segmentTitle", { n: i + 1 })} placeholder={t("contentEditor.segmentTitlePlaceholder")} value={r.title} onChange={(e) => set(i, { title: e.target.value })} />
          <Input className="h-8 w-20" type="number" min={0} aria-label={t("contentEditor.segmentStart", { n: i + 1 })} value={r.start} onChange={(e) => set(i, { start: e.target.value })} />
          <span aria-hidden>–</span>
          <Input className="h-8 w-20" type="number" min={1} aria-label={t("contentEditor.segmentEnd", { n: i + 1 })} value={r.end} onChange={(e) => set(i, { end: e.target.value })} />
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
        <Button
          type="button"
          size="sm"
          disabled={save.pending || !valid}
          onClick={() => {
            setSaved(false);
            const segments: Segment[] = rows.map((r, i) => ({ position: i + 1, title: r.title.trim() || undefined, start: Number(r.start), end: Number(r.end), release_at: fromLocalInput(r.release), released: false }));
            void save.run(() => client.contents.setSegments(content.id, version.id, segments)).then((d) => d && setSaved(true));
          }}
        >
          {t("contentEditor.saveSegments")}
        </Button>
        <Saved show={saved} />
      </div>
      <ErrorLine error={save.error} />
    </div>
  );
}

/** Every editor section of an existing content: details, access and price, versions, groups. */
export function ContentSections({ content, ...p }: Common & { content: Content; media?: boolean; className?: string }) {
  const languageName = useLanguageName();
  const versions = useMemo(() => content.versions.map((v) => ({ id: v.id, label: versionLabel(v, languageName, p.labels) })), [content.versions, languageName, p.labels]);
  const { spec } = useKind(content.kind, p.client);
  return (
    <div className={cn("grid gap-6", p.className)} data-ckui="content-sections">
      <ContentDetailsEditor content={content} {...p} />
      <AccessEditor content={content} {...p} />
      <VersionsEditor content={content} {...p} />
      {!!spec?.groups?.length && (
        <section className="grid gap-3 border-t border-border pt-4" data-ckui="content-groups">
          <MembershipEditor content={content.id} contentKind={content.kind} versions={spec.versions.hidden ? undefined : versions} labels={p.labels} client={p.client} onError={p.onError} />
        </section>
      )}
    </div>
  );
}
