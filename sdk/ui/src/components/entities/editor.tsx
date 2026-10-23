import { cn } from "cn";
import { useCallback, useEffect, useId, useRef, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import { parseAttributes, slugify } from "../../client/entity-fields.js";
import type { ContentModel, Entity, EntityCard, EntityDetail, EntityKind, EntitiesText as Text, MergeReport, Name } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import type { ContentKitErrorHandler } from "../../react/context.js";
import { useEntity, useKinds, type UseEntity } from "../../react/entities.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Switch } from "#ckui/ui/switch";
import { Button } from "#ckui/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "#ckui/ui/dialog";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { Textarea } from "#ckui/ui/textarea";
import { contentError, ErrorLine, useAction } from "../social/parts.js";
import { OfferFields } from "../content/offer-fields.js";
import { AttributeFields, formOf } from "./fields.js";
import { useContentKitClient } from "../../react/context.js";
import { EntityImageEditor } from "./banner.js";
import { EntityPicker, labelOf, useEntityNames, type EntityLike, type Labels } from "./parts.js";

export interface EntityEditorProps {
  /** The entity edited; null creates one of `kind`. */
  id: string | null;
  /** Staff management, including internal entities and their visibility setting. */
  management?: boolean;
  /** The kind a new entity is created as. */
  kind: string;
  /** Languages names and text are written in; default the content model's. */
  languages?: readonly string[];
  labels?: Labels;
  onCreated?: (entity: Entity) => void;
  /** After a merge: the survivor's id is report.into. */
  onMerged?: (report: MergeReport) => void;
  onDeleted?: (entity: Entity) => void;
  /** Below the editor's sections, with the loaded entity: the members, host fields. */
  children?: (entity: EntityDetail) => ReactNode;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}


/**
 * Staff (app:entities:write): creates a entity, or edits one: slug, parent,
 * scope and declared attributes; names and aliases per language; description
 * and sort key per language; cover (an upload, or automatic); edges to other
 * entities; merge into another entity; delete and restore.
 */
export function EntityEditor(p: EntityEditorProps) {
  const { t } = useMessages();
  const { kinds, loading: kindsLoading } = useKinds({ client: p.client });
  const g = useEntity(p.id, { client: p.client, management: p.management });
  const languages = p.languages ?? kinds?.languages ?? ["en"];
  const kind = g.entity?.kind ?? p.kind;
  const spec: EntityKind = kinds?.entity_kinds?.[kind] ?? {};
  const root = (children: ReactNode) => (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-6 text-sm", p.className)} data-ckui="entity-editor" data-kind={kind}>
      {children}
    </ContentKitUiRoot>
  );
  if (kindsLoading) return root(<Skeleton className="h-40 w-full" />);
  // A entity created here is edited in place until the host routes to it.
  if (!p.id && !g.entity && !g.loading) return root(<CreateForm {...p} kind={kind} spec={spec} languages={languages} g={g} />);
  if (g.deleted)
    return root(
      <Deleted
        entity={g.deleted}
        onRestore={() => g.restore()}
        onError={p.onError}
        name={g.deleted.slug}
      />,
    );
  if (!g.entity) {
    if (g.error)
      return root(
        <div role="alert" className="grid justify-items-start gap-2">
          <p className="text-destructive">{t("entityEditor.loadFailed")}</p>
          <Button size="sm" variant="outline" onClick={g.reload}>
            {t("common.retry")}
          </Button>
        </div>,
      );
    return root(<Skeleton className="h-40 w-full" />);
  }
  const entity = g.entity;
  return root(
    <>
      <Head management={p.management} entity={entity} g={g} labels={p.labels} onMerged={p.onMerged} onDeleted={p.onDeleted} onError={p.onError} client={p.client} />
      <Details management={p.management} key={`details:${entity.id}`} entity={entity} spec={spec} g={g} labels={p.labels} onError={p.onError} client={p.client} />
      <Names entity={entity} g={g} languages={languages} onError={p.onError} />
      <Texts key={`text:${entity.id}`} entity={entity} g={g} languages={languages} onError={p.onError} />
      <Images management={p.management} entity={entity} spec={spec} onError={p.onError} client={p.client} />
      {spec.sellable && <Membership entity={entity} onError={p.onError} client={p.client} />}
      <Edges management={p.management} entity={entity} g={g} kinds={kinds} labels={p.labels} onError={p.onError} client={p.client} />
      {p.children?.(entity)}
    </>,
  );
}

/** Form state seeded from the record, seeded again when it changes unless edited since; settle() after a save. */
function useSeeded<T>(seed: () => T, revision: unknown): [T, (next: (prev: T) => T) => void, () => void] {
  const [value, setValue] = useState(seed);
  const edited = useRef(false);
  const latest = useRef(seed);
  latest.current = seed;
  useEffect(() => {
    if (!edited.current) setValue(latest.current());
  }, [revision]);
  const set = useCallback((next: (prev: T) => T) => {
    edited.current = true;
    setValue(next);
  }, []);
  const settle = useCallback(() => {
    edited.current = false;
  }, []);
  return [value, set, settle];
}

function Section({ title, children, ckui, action }: { title: ReactNode; children: ReactNode; ckui: string; action?: ReactNode }) {
  return (
    <section className="grid gap-3" data-ckui={ckui}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="font-medium">{title}</h3>
        {action}
      </div>
      {children}
    </section>
  );
}

function Deleted({ entity, name, onRestore, onError }: { entity: Entity; name: string; onRestore: () => Promise<Entity>; onError?: ContentKitErrorHandler }) {
  const { t } = useMessages();
  const restore = useAction("entity.save", onError);
  return (
    <div className="grid justify-items-start gap-2" data-ckui="entity-deleted" data-state={entity.state}>
      <p>{t("entityEditor.deletedNotice", { name })}</p>
      <Button size="sm" variant="outline" disabled={restore.pending} onClick={() => void restore.run(onRestore)}>
        {t("entityEditor.restore")}
      </Button>
      <ErrorLine error={restore.error} />
    </div>
  );
}

function Head({
  entity,
  g,
  labels,
  onMerged,
  onDeleted,
  onError,
  client,
  management,
}: {
  management?: boolean;
  entity: EntityDetail;
  g: UseEntity;
  labels?: Labels;
  onMerged?: (r: MergeReport) => void;
  onDeleted?: (g: Entity) => void;
  onError?: ContentKitErrorHandler;
  client?: ContentKitClient;
}) {
  const { t } = useMessages();
  const [merging, setMerging] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const remove = useAction("entity.save", onError);
  return (
    <div className="flex flex-wrap items-center gap-2" data-ckui="entity-head">
      <h2 className="text-base font-semibold break-words">{entity.name || entity.slug}</h2>
      <Badge variant="outline">{labelOf(labels, entity.kind)}</Badge>
      <div className="ms-auto flex flex-wrap gap-2">
        <Button size="sm" variant="outline" onClick={() => setMerging(true)}>
          {t("entityEditor.merge")}
        </Button>
        <Button size="sm" variant="destructive" disabled={remove.pending} onClick={() => setConfirmDelete(true)}>
          {t("entityEditor.delete")}
        </Button>
      </div>
      <ErrorLine error={remove.error} className="basis-full" />
      <EntityMergeDialog
        open={merging}
        onOpenChange={setMerging}
        entityy={entity}
        management={management}
        client={client}
        onMerge={async (into) => {
          const report = await g.merge(into.id);
          onMerged?.(report);
          return report;
        }}
      />
      <Dialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <DialogContent data-ckui="entity-delete-dialog">
          <DialogHeader>
            <DialogTitle>{t("entityEditor.deleteTitle", { name: entity.name || entity.slug })}</DialogTitle>
            <DialogDescription>{t("entityEditor.deleteBody")}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirmDelete(false)}>
              {t("common.cancel")}
            </Button>
            <Button
              variant="destructive"
              disabled={remove.pending}
              onClick={() => {
                setConfirmDelete(false);
                void remove.run(g.remove).then((d) => d && onDeleted?.(d));
              }}
            >
              {t("entityEditor.delete")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

export interface EntityMergeDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The entity merged away. */
  entity: EntityDetail;
  /** Merges into the chosen entity: memberships, names, edges and children move, its code redirects. */
  onMerge: (into: EntityCard) => Promise<MergeReport>;
  management?: boolean;
  language?: string;
  client?: ContentKitClient;
}

/** Picks a entity of the same kind and merges this one into it, after typing the confirmation word. */
export function EntityMergeDialog({ open, onOpenChange, entity, onMerge, language, client, management }: EntityMergeDialogProps) {
  const m = useMessages();
  const { t } = m;
  const id = useId();
  const [into, setInto] = useState<EntityCard | null>(null);
  const [word, setWord] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<ContentKitError | null>(null);
  useEffect(() => {
    if (!open) return;
    setInto(null);
    setWord("");
    setError(null);
  }, [open]);
  const typed = t("entityEditor.mergeWord");
  const ready = !!into && word.trim().toLowerCase() === typed.toLowerCase();
  const name = entity.name || entity.slug;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent data-ckui="entity-merge-dialog">
        <DialogHeader>
          <DialogTitle>{t("entityEditor.mergeTitle", { name })}</DialogTitle>
          <DialogDescription>{t("entityEditor.mergeBody")}</DialogDescription>
        </DialogHeader>
        {into ? (
          <div className="flex flex-wrap items-center gap-2" data-ckui="merge-target">
            <span>{t("entityEditor.mergeInto", { name: into.name || into.slug })}</span>
            <Button size="xs" variant="ghost" onClick={() => setInto(null)}>
              {t("entityEditor.change")}
            </Button>
          </div>
        ) : (
          <EntityPicker management={management} kind={entity.kind} language={language} exclude={[entity.id]} label={t("entityEditor.mergeTarget")} onPick={setInto} client={client} autoFocus />
        )}
        <div className="grid gap-1.5">
          <Label htmlFor={`${id}-typed`}>{t("admin.typeToConfirm", { word: typed })}</Label>
          <Input id={`${id}-typed`} value={word} autoComplete="off" onChange={(e) => setWord(e.target.value)} />
        </div>
        {error && <ErrorLine>{contentError(m, error)}</ErrorLine>}
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button
            variant="destructive"
            disabled={!ready || pending}
            onClick={async () => {
              if (!into) return;
              setPending(true);
              setError(null);
              try {
                await onMerge(into);
                onOpenChange(false);
              } catch (e) {
                setError(toContentKitError(e));
              } finally {
                setPending(false);
              }
            }}
          >
            {t("entityEditor.merge")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** A parent or scope: the chosen entity with a change and a clear, or a picker. */
function EntityField({ label, kind, value, onChange, exclude, required, client, management }: { management?: boolean; label: string; kind: string; value: EntityLike | null; onChange: (g: EntityLike | null) => void; exclude?: string[]; required?: boolean; client?: ContentKitClient }) {
  const { t } = useMessages();
  return (
    <div className="grid gap-1.5" data-ckui="entity-field">
      <span className="text-sm font-medium">
        {label}
        {required && <span aria-hidden> *</span>}
      </span>
      {value ? (
        <div className="flex flex-wrap items-center gap-2">
          <span className="rounded-md border border-border px-2 py-0.5" data-ckui="entity-field-value">
            {value.name || value.slug}
          </span>
          <Button type="button" size="xs" variant="ghost" onClick={() => onChange(null)} aria-label={t("entityEditor.clearField", { field: label })}>
            {required ? t("entityEditor.change") : t("common.remove")}
          </Button>
        </div>
      ) : (
        <EntityPicker management={management} kind={kind} label={label} exclude={exclude} onPick={onChange} client={client} />
      )}
    </div>
  );
}

function CreateForm({ kind, spec, languages, labels, g, onCreated, onError, client, management }: EntityEditorProps & { spec: EntityKind; languages: readonly string[]; g: UseEntity }) {
  const m = useMessages();
  const { t } = m;
  const id = useId();
  const languageName = useLanguageName();
  const [names, setNames] = useState<Record<string, string>>({});
  const [slug, setSlug] = useState("");
  const [slugEdited, setSlugEdited] = useState(false);
  const [internal, setInternal] = useState(false);
  const [parent, setParent] = useState<EntityLike | null>(null);
  const [scope, setScope] = useState<EntityLike | null>(null);
  const declared = spec.attributes ?? {};
  const [attrs, setAttrs] = useState<Record<string, string | boolean>>(() => formOf(declared, {}));
  const [invalid, setInvalid] = useState<string | null>(null);
  const create = useAction("entity.save", onError);
  const first = names[languages[0]!] ?? "";
  useEffect(() => {
    if (!slugEdited) setSlug(slugify(first));
  }, [first, slugEdited]);
  const submit = async () => {
    setInvalid(null);
    let attributes: Record<string, unknown>;
    try {
      attributes = parseAttributes(declared, attrs);
    } catch (e) {
      setInvalid((e as Error).message);
      return;
    }
    const given: Name[] = languages.filter((l) => names[l]?.trim()).map((l) => ({ language: l, kind: "name", name: names[l]!.trim() }));
    const made = await create.run(() =>
      g.create({ kind, slug: slug.trim(), ...(management ? { internal } : {}), names: given, parent_id: parent?.id, scope_id: scope?.id, attributes: Object.keys(attributes).length ? attributes : undefined }),
    );
    if (made) onCreated?.(made);
  };
  return (
    <form
      className="grid gap-4"
      data-ckui="entity-create"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <h2 className="text-base font-semibold">{t("entityEditor.newTitle", { kind: labelOf(labels, kind) })}</h2>
      {languages.map((l, i) => (
        <div key={l} className="grid gap-1.5">
          <Label htmlFor={`${id}-${l}`}>{t("entityEditor.nameIn", { language: languageName(l) })}</Label>
          <Input id={`${id}-${l}`} className="h-8" lang={l} required={i === 0} value={names[l] ?? ""} onChange={(e) => setNames((n) => ({ ...n, [l]: e.target.value }))} />
        </div>
      ))}
      <div className="grid gap-1.5">
        <Label htmlFor={`${id}-slug`}>{t("entityEditor.slug")}</Label>
        <Input
          id={`${id}-slug`}
          className="h-8"
          required
          value={slug}
          onChange={(e) => {
            setSlugEdited(true);
            setSlug(e.target.value);
          }}
        />
      </div>
      {spec.scoped_by && <EntityField management={management} label={labelOf(labels, spec.scoped_by)} kind={spec.scoped_by} value={scope} onChange={setScope} required client={client} />}
      {spec.parent && <EntityField management={management} label={t("entityEditor.parent")} kind={spec.parent} value={parent} onChange={setParent} client={client} />}
      {management && <InternalField id={`${id}-internal`} checked={internal} onChange={setInternal} />}
      <AttributeFields declared={declared} form={attrs} onChange={(n, v) => setAttrs((a) => ({ ...a, [n]: v }))} labels={labels} />
      {invalid && <ErrorLine>{invalid}</ErrorLine>}
      {create.error && <ErrorLine>{contentError(m, create.error)}</ErrorLine>}
      <Button type="submit" className="justify-self-start" disabled={create.pending || !first.trim() || !slug.trim() || (!!spec.scoped_by && !scope)}>
        {create.pending ? t("common.saving") : t("entityEditor.create")}
      </Button>
    </form>
  );
}

function InternalField({ id, checked, onChange }: { id: string; checked: boolean; onChange: (value: boolean) => void }) {
  const { t } = useMessages();
  return (
    <div className="flex items-start gap-3">
      <Switch id={id} checked={checked} onCheckedChange={onChange} />
      <div className="grid gap-1">
        <Label htmlFor={id}>{t("entityEditor.internal")}</Label>
        <p className="text-xs text-muted-foreground">{t("entityEditor.internalHint")}</p>
      </div>
    </div>
  );
}

function Details({ entity, spec, g, labels, onError, client, management }: { management?: boolean; entity: EntityDetail; spec: EntityKind; g: UseEntity; labels?: Labels; onError?: ContentKitErrorHandler; client?: ContentKitClient }) {
  const m = useMessages();
  const { t } = m;
  const id = useId();
  const declared = spec.attributes ?? {};
  const [form, setForm, settle] = useSeeded(
    () => ({ slug: entity.slug, internal: entity.internal ?? false, parent: (entity.parent ?? null) as EntityLike | null, scope: (entity.scope ?? null) as EntityLike | null, attrs: formOf(declared, entity.attributes) }),
    entity.revision,
  );
  const { slug, internal, parent, scope, attrs } = form;
  const edit = (patch: Partial<typeof form>) => {
    setSaved(false);
    setForm((f) => ({ ...f, ...patch }));
  };
  const setSlug = (v: string) => edit({ slug: v });
  const setParent = (v: EntityLike | null) => edit({ parent: v });
  const setScope = (v: EntityLike | null) => edit({ scope: v });
  const setAttrs = (fn: (a: Record<string, string | boolean>) => Record<string, string | boolean>) => edit({ attrs: fn(attrs) });
  const [invalid, setInvalid] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const save = useAction("entity.save", onError);
  const submit = async () => {
    setInvalid(null);
    setSaved(false);
    let attributes: Record<string, unknown>;
    try {
      attributes = parseAttributes(declared, attrs);
    } catch (e) {
      setInvalid((e as Error).message);
      return;
    }
    const done = await save.run(() =>
      g.update({
        slug: slug.trim(),
        ...(management ? { internal } : {}),
        attributes,
        ...(spec.parent ? { parent_id: parent?.id ?? "" } : {}),
        ...(spec.scoped_by && scope ? { scope_id: scope.id } : {}),
      }),
    );
    if (done) {
      settle();
      setSaved(true);
    }
  };
  return (
    <Section title={t("entityEditor.details")} ckui="group-details">
      <form
        className="grid gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <div className="grid gap-1.5">
          <Label htmlFor={`${id}-slug`}>{t("entityEditor.slug")}</Label>
          <Input id={`${id}-slug`} className="h-8" required value={slug} onChange={(e) => setSlug(e.target.value)} />
        </div>
        {spec.scoped_by && <EntityField management={management} label={labelOf(labels, spec.scoped_by)} kind={spec.scoped_by} value={scope} onChange={setScope} required client={client} />}
        {spec.parent && <EntityField management={management} label={t("entityEditor.parent")} kind={spec.parent} value={parent} onChange={setParent} exclude={[entity.id]} client={client} />}
        {management && <InternalField id={`${id}-internal`} checked={internal} onChange={(value) => edit({ internal: value })} />}
        <AttributeFields declared={declared} form={attrs} onChange={(n, v) => setAttrs((a) => ({ ...a, [n]: v }))} labels={labels} />
        {invalid && <ErrorLine>{invalid}</ErrorLine>}
        <ErrorLine error={save.error} />
        <div className="flex items-center gap-3">
          <Button type="submit" size="sm" disabled={save.pending || !slug.trim() || (!!spec.scoped_by && !scope)}>
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
          {saved && (
            <span role="status" className="text-muted-foreground">
              {t("entityEditor.saved")}
            </span>
          )}
        </div>
      </form>
    </Section>
  );
}

function Names({ entity, g, languages, onError }: { entity: EntityDetail; g: UseEntity; languages: readonly string[]; onError?: ContentKitErrorHandler }) {
  const { t } = useMessages();
  const id = useId();
  const languageName = useLanguageName();
  const [language, setLanguage] = useState(languages[0]!);
  const [kind, setKind] = useState<Name["kind"]>("alias");
  const [text, setText] = useState("");
  const save = useAction("entity.save", onError);
  const sorted = [...entity.names].sort((a, b) => a.language.localeCompare(b.language) || (a.kind === b.kind ? a.name.localeCompare(b.name) : a.kind === "name" ? -1 : 1));
  const offered = [...new Set([...languages, ...entity.names.map((n) => n.language)])];
  return (
    <Section title={t("entityEditor.names")} ckui="group-names-editor">
      {sorted.length ? (
        <ul className="grid divide-y divide-border rounded-lg border border-border">
          {sorted.map((n) => (
            <li key={`${n.language}:${n.kind}:${n.name}`} className="flex flex-wrap items-center gap-2 p-2.5" data-ckui="entity-name" data-kind={n.kind} data-language={n.language}>
              <Badge variant="outline">{languageName(n.language)}</Badge>
              <span className="min-w-0 flex-1 break-words" lang={n.language}>
                {n.name}
              </span>
              <span className="text-xs text-muted-foreground">{t(n.kind === "name" ? "entityEditor.canonical" : "entityEditor.alias")}</span>
              {n.kind === "alias" && (
                <Button size="xs" variant="ghost" disabled={save.pending} onClick={() => void save.run(() => g.names("add", [{ ...n, kind: "name" }]))}>
                  {t("entityEditor.makeCanonical")}
                </Button>
              )}
              <Button size="xs" variant="ghost" disabled={save.pending} aria-label={t("entityEditor.removeName", { name: n.name })} onClick={() => void save.run(() => g.names("remove", [n]))}>
                {t("common.remove")}
              </Button>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-muted-foreground">{t("entityEditor.noNames")}</p>
      )}
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          // Aliases take a comma-separated list, as entity chips do; a canonical name stays whole.
          const have = new Set(entity.names.filter((n) => n.language === language).map((n) => n.name.toLowerCase()));
          const tokens = kind === "alias" ? [...new Set(text.split(/[,\n]/).map((x) => x.trim()).filter((x) => x && !have.has(x.toLowerCase())))] : [text.trim()].filter(Boolean);
          if (tokens.length) void save.run(() => g.names("add", tokens.map((name) => ({ language, kind, name })))).then((d) => d && setText(""));
        }}
      >
        {offered.length > 1 && (
          <Select value={language} onValueChange={(v) => setLanguage(String(v))}>
            <SelectTrigger size="sm" aria-label={t("entityEditor.language")}>
              <SelectValue>{(v: string) => languageName(v)}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {offered.map((l) => (
                <SelectItem key={l} value={l}>
                  {languageName(l)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
        <Select value={kind} onValueChange={(v) => setKind(v as Name["kind"])}>
          <SelectTrigger size="sm" aria-label={t("entityEditor.nameKind")}>
            <SelectValue>{(v: string) => t(v === "name" ? "entityEditor.canonical" : "entityEditor.alias")}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="alias">{t("entityEditor.alias")}</SelectItem>
            <SelectItem value="name">{t("entityEditor.canonical")}</SelectItem>
          </SelectContent>
        </Select>
        <Label htmlFor={`${id}-name`} className="sr-only">
          {t("entityEditor.newName")}
        </Label>
        <Input id={`${id}-name`} className="h-8 min-w-40 flex-1" lang={language} value={text} placeholder={t(kind === "alias" ? "entityEditor.newAliases" : "entityEditor.newName")} onChange={(e) => setText(e.target.value)} />
        <Button type="submit" size="sm" variant="outline" disabled={save.pending || !text.trim()}>
          {t("entityEditor.addName")}
        </Button>
      </form>
      <ErrorLine error={save.error} />
    </Section>
  );
}

function Texts({ entity, g, languages, onError }: { entity: EntityDetail; g: UseEntity; languages: readonly string[]; onError?: ContentKitErrorHandler }) {
  const { t } = useMessages();
  const id = useId();
  const languageName = useLanguageName();
  const offered = [...new Set([...languages, ...entity.text.map((x) => x.language)])];
  const [form, setForm, settle] = useSeeded<Record<string, Text>>(
    () => Object.fromEntries(offered.map((l) => [l, entity.text.find((x) => x.language === l) ?? { language: l }])),
    entity.revision,
  );
  const [saved, setSaved] = useState(false);
  const save = useAction("entity.save", onError);
  const set = (l: string, patch: Partial<Text>) => {
    setSaved(false);
    setForm((f) => ({ ...f, [l]: { ...f[l]!, ...patch } }));
  };
  return (
    <Section title={t("entityEditor.text")} ckui="group-text">
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          const text = Object.values(form)
            .map((x) => ({ language: x.language, description: x.description?.trim() || undefined, sort_key: x.sort_key?.trim() || undefined }))
            .filter((x) => x.description || x.sort_key);
          void save.run(() => g.update({ text })).then((d) => {
            if (!d) return;
            settle();
            setSaved(true);
          });
        }}
      >
        {offered.map((l) => (
          <fieldset key={l} className="grid gap-2" data-language={l}>
            <legend className="mb-1 text-xs font-medium text-muted-foreground">{languageName(l)}</legend>
            <Label htmlFor={`${id}-${l}-d`} className="sr-only">
              {t("entityEditor.descriptionIn", { language: languageName(l) })}
            </Label>
            <Textarea
              id={`${id}-${l}-d`}
              lang={l}
              rows={3}
              placeholder={t("entityEditor.description")}
              value={form[l]?.description ?? ""}
              onChange={(e) => set(l, { description: e.target.value })}
            />
            <Label htmlFor={`${id}-${l}-s`} className="sr-only">
              {t("entityEditor.sortKeyIn", { language: languageName(l) })}
            </Label>
            <Input
              id={`${id}-${l}-s`}
              className="h-8"
              lang={l}
              placeholder={t("entityEditor.sortKey")}
              value={form[l]?.sort_key ?? ""}
              onChange={(e) => set(l, { sort_key: e.target.value })}
            />
          </fieldset>
        ))}
        <ErrorLine error={save.error} />
        <div className="flex items-center gap-3">
          <Button type="submit" size="sm" disabled={save.pending}>
            {save.pending ? t("common.saving") : t("common.save")}
          </Button>
          {saved && (
            <span role="status" className="text-muted-foreground">
              {t("entityEditor.saved")}
            </span>
          )}
        </div>
      </form>
    </Section>
  );
}

function Images({ entity, spec, onError, client, management }: { management?: boolean; entity: EntityDetail; spec: EntityKind; onError?: ContentKitErrorHandler; client?: ContentKitClient }) {
  const { t } = useMessages();
  const c = entity.cover;
  const mode = c?.image ? "image" : c?.content_id && !c.auto ? "content" : "auto";
  const avatar = spec.owner === "user" || !!spec.avatar;
  return (
    <Section title={t(avatar ? "entityEditor.images" : "entityEditor.cover")} ckui="group-cover-editor">
      <p className="text-muted-foreground" data-ckui="cover-mode">
        {t(mode === "image" ? "entityEditor.coverImage" : mode === "content" ? "entityEditor.coverContent" : "entityEditor.coverAuto")}
      </p>
      <EntityImageEditor management={management} entityy={entity} slot="cover" client={client} onError={onError} />
      {avatar && <EntityImageEditor management={management} entityy={entity} slot="avatar" client={client} onError={onError} />}
    </Section>
  );
}

function Membership({ entity, onError, client }: { entity: EntityDetail; onError?: ContentKitErrorHandler; client?: ContentKitClient }) {
  const { t } = useMessages();
  const ck = useContentKitClient(client);
  const offer = useAction("offer.save", onError);
  return (
    <Section title={t("entityEditor.membershipPrice")} ckui="group-offer">
      <OfferFields
        key={entity.offer?.revision}
        offer={entity.offer}
        pending={offer.pending}
        onSell={(terms) => void offer.run(() => ck.offers.sellEntity(entity.id, terms))}
        onWithdraw={() => void offer.run(() => ck.offers.withdrawEntity(entity.id))}
      />
      <ErrorLine error={offer.error} />
    </Section>
  );
}

const RELATION = /^[a-z][a-z0-9_]*$/;

function Edges({ entity, g, kinds, labels, onError, client, management }: { management?: boolean; entity: EntityDetail; g: UseEntity; kinds: ContentModel | null; labels?: Labels; onError?: ContentKitErrorHandler; client?: ContentKitClient }) {
  const { t } = useMessages();
  const id = useId();
  const save = useAction("entity.save", onError);
  const partners = useEntityNames(
    entity.edges.map((e) => (e.from === entity.id ? e.to : e.from)),
    { client, management },
  );
  const kindsList = Object.keys(kinds?.entity_kinds ?? {}).sort();
  const [relation, setRelation] = useState("");
  const [kind, setKind] = useState(kindsList[0] ?? entity.kind);
  const out = entity.edges.filter((e) => e.from === entity.id);
  const incoming = entity.edges.filter((e) => e.to === entity.id);
  const nameOf = (gid: string) => {
    const x = partners.get(gid);
    return x ? x.name || x.slug : gid;
  };
  const valid = RELATION.test(relation.trim());
  return (
    <Section title={t("entityEditor.edges")} ckui="group-edges">
      {out.length + incoming.length ? (
        <ul className="grid gap-1.5">
          {out.map((e) => (
            <li key={`${e.relation}:${e.to}`} className="flex flex-wrap items-center gap-2" data-ckui="entity-edge" data-relation={e.relation}>
              <Badge variant="secondary">{labelOf(labels, e.relation)}</Badge>
              <span>{nameOf(e.to)}</span>
              <Button size="xs" variant="ghost" disabled={save.pending} aria-label={t("entityEditor.removeEdge", { name: nameOf(e.to) })} onClick={() => void save.run(() => g.edges("remove", [{ relation: e.relation, to: e.to }]))}>
                {t("common.remove")}
              </Button>
            </li>
          ))}
          {incoming.map((e) => (
            <li key={`in:${e.relation}:${e.from}`} className="flex flex-wrap items-center gap-2 text-muted-foreground" data-ckui="entity-edge" data-relation={`inverse:${e.relation}`}>
              <span className="text-foreground">{nameOf(e.from)}</span>
              <Badge variant="outline">{labelOf(labels, e.relation)}</Badge>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-muted-foreground">{t("entityEditor.noEdges")}</p>
      )}
      <div className="flex flex-wrap items-end gap-2">
        <div className="grid gap-1">
          <Label htmlFor={`${id}-rel`} className="text-xs text-muted-foreground">
            {t("entityEditor.relation")}
          </Label>
          <Input id={`${id}-rel`} className="h-8 w-40" value={relation} placeholder="member_of" aria-invalid={!!relation && !valid} onChange={(e) => setRelation(e.target.value)} />
        </div>
        {kindsList.length > 1 && (
          <Select value={kind} onValueChange={(v) => setKind(String(v))}>
            <SelectTrigger size="sm" aria-label={t("entityEditor.edgeKind")}>
              <SelectValue>{(v: string) => labelOf(labels, v)}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {kindsList.map((k) => (
                <SelectItem key={k} value={k}>
                  {labelOf(labels, k)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
        {valid && (
          <EntityPicker
            management={management}
            key={kind}
            kind={kind}
            label={t("entityEditor.edgeTarget")}
            exclude={[entity.id]}
            client={client}
            onPick={(to) => void save.run(() => g.edges("add", [{ relation: relation.trim(), to: to.id }])).then((d) => d && setRelation(""))}
          />
        )}
      </div>
      <ErrorLine error={save.error} />
    </Section>
  );
}
