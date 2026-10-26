import { Add01Icon, ArrowDown01Icon, Delete02Icon, GitMergeIcon, Link01Icon, MoreHorizontalIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { cn } from "cn";
import { useContext, useEffect, useId, useMemo, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import { fold, parseAttributes, slugify } from "../../client/entity-fields.js";
import type { ContentModel, Entity, EntityCard, EntityDetail, EntityKind, EntityUpdate, EntitiesText as Text, MergeReport, Name } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import { ContentKitContext, type ContentKitErrorHandler } from "../../react/context.js";
import { useEntity, useKinds, type UseEntity } from "../../react/entities.js";
import { useDraft } from "../../react/use-draft.js";
import { ContentKitUiRoot } from "../../scope.js";
import { Badge } from "#ckui/ui/badge";
import { Switch } from "#ckui/ui/switch";
import { Button } from "#ckui/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "#ckui/ui/dropdown-menu";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "#ckui/ui/dialog";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { Textarea } from "#ckui/ui/textarea";
import { contentError, ErrorLine, useAction } from "../social/parts.js";
import { useConfirm } from "../admin/shell.js";
import { AttributeFields, formOf } from "./fields.js";
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
  /** Leaves the new-entity form without creating, e.g. back to the list; none: no Cancel. */
  onCancel?: () => void;
  /** After a merge: the survivor's id is report.into. */
  onMerged?: (report: MergeReport) => void;
  onDeleted?: (entity: Entity) => void;
  /** Registers a confirmation check for the host's editor navigation. */
  onLeaveGuard?: (guard: (() => Promise<boolean>) | undefined) => void;
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
  return root(<EditForm key={g.entity.id} {...p} entity={g.entity} spec={spec} languages={languages} g={g} kinds={kinds} />);
}

function detailsOf(entity: EntityDetail, spec: EntityKind) {
  return { slug: entity.slug, internal: entity.internal ?? false, parent: (entity.parent ?? null) as EntityLike | null, scope: (entity.scope ?? null) as EntityLike | null, attrs: formOf(spec.attributes ?? {}, entity.attributes) };
}

type DetailsValue = ReturnType<typeof detailsOf>;

function addNames(names: Name[], additions: Name[]): Name[] {
  let next = names;
  for (const name of additions) {
    const same = (n: Name) => n.language === name.language && fold(n.name) === fold(name.name);
    if (name.kind === "alias" && next.some(same)) continue;
    next = next.filter((n) => !same(n)).map((n): Name => name.kind === "name" && n.language === name.language && n.kind === "name" ? { ...n, kind: "alias" } : n);
    next = [...next, name];
  }
  return next;
}

function EditForm({ entity, spec, languages, g, kinds, onLeaveGuard, ...p }: EntityEditorProps & { entity: EntityDetail; spec: EntityKind; languages: readonly string[]; g: UseEntity; kinds: ContentModel | null }) {
  const m = useMessages();
  const { t } = m;
  const offered = useMemo(() => [...new Set([...languages, ...entity.text.map((x) => x.language)])], [languages, entity.text]);
  const draft = useDraft(useMemo(() => ({
    details: detailsOf(entity, spec),
    names: entity.names,
    text: Object.fromEntries(offered.map((l) => {
      const x = entity.text.find((x) => x.language === l);
      return [l, { language: l, description: x?.description ?? "", sort_key: x?.sort_key ?? "" }];
    })),
    newName: "",
  }), [entity, spec, offered]));
  const { details, names, text, newName } = draft.value;
  const [language, setLanguage] = useState(languages[0]!);
  const [nameKind, setNameKind] = useState<Name["kind"]>("alias");
  const [invalid, setInvalid] = useState<string | null>(null);
  const [failedParts, setFailedParts] = useState("");
  const [saved, setSaved] = useState(false);
  const save = useAction("entity.save", p.onError);
  const dirty = draft.dirty.some((key) => key !== "newName") || !!newName.trim();
  const unsaved = dirty || save.pending;
  const pendingNames = () => addNames(names, (nameKind === "alias" ? newName.split(/[,\n]/) : [newName]).map((name) => name.trim()).filter(Boolean).map((name) => ({ language, kind: nameKind, name })));
  const { confirm, dialog } = useConfirm();
  useEffect(() => {
    if (!unsaved) return;
    const ask = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    addEventListener("beforeunload", ask);
    return () => removeEventListener("beforeunload", ask);
  }, [unsaved]);
  useEffect(() => {
    onLeaveGuard?.(async () => !unsaved || await confirm({ title: t("entitySave.leaveEntity"), description: t("entitySave.discardEdits"), confirm: t("entitySave.discard") }));
    return () => onLeaveGuard?.(undefined);
  }, [onLeaveGuard, unsaved, confirm, t]);
  const submit = async () => {
    if (save.pending || !dirty) return;
    setInvalid(null);
    setSaved(false);
    setFailedParts("");
    const patch: EntityUpdate = {};
    const parts: string[] = [];
    if (draft.dirty.includes("details")) {
      parts.push(t("entityEditor.details"));
      try {
        patch.attributes = parseAttributes(spec.attributes ?? {}, details.attrs);
      } catch (e) {
        setInvalid(`${t("entityEditor.details")}: ${(e as Error).message}`);
        return;
      }
      patch.slug = details.slug.trim();
      if (p.management) patch.internal = details.internal;
      if (spec.parent) patch.parent_id = details.parent?.id ?? "";
      if (spec.scoped_by && details.scope) patch.scope_id = details.scope.id;
    }
    if (draft.dirty.includes("names") || newName.trim()) {
      parts.push(t("entityEditor.names"));
      patch.names = pendingNames();
    }
    if (draft.dirty.includes("text")) {
      parts.push(t("entityEditor.text"));
      patch.text = Object.values(text).map((x) => ({ language: x.language, description: x.description.trim() || undefined, sort_key: x.sort_key.trim() || undefined })).filter((x) => x.description || x.sort_key);
    }
    const settle = draft.checkpoint();
    const done = await save.run(() => g.update(patch));
    if (!done) {
      setFailedParts(parts.join(", "));
      return;
    }
    setSaved(settle({
      details: { ...details, slug: done.slug, internal: done.internal, attrs: formOf(spec.attributes ?? {}, done.attributes) },
      names: patch.names ?? names,
      text: Object.fromEntries(offered.map((l) => {
        const x = patch.text ? patch.text.find((x) => x.language === l) : text[l];
        return [l, { language: l, description: x?.description ?? "", sort_key: x?.sort_key ?? "" }];
      })),
      newName: "",
    }));
  };
  return (
    <>
      {dialog}
      <Head management={p.management} entity={entity} g={g} labels={p.labels} onMerged={p.onMerged} onDeleted={p.onDeleted} onError={p.onError} client={p.client}>
        <span className="flex items-center gap-3" data-ckui="entity-save">
          <span role="status" className="text-muted-foreground empty:hidden">{dirty ? t("entitySave.unsaved") : saved ? t("entityEditor.saved") : ""}</span>
          <Button size="sm" variant={dirty ? "default" : "outline"} disabled={save.pending || !dirty || !details.slug.trim() || (!!spec.scoped_by && !details.scope)} onClick={() => void submit()}>{save.pending ? t("common.saving") : t("common.save")}</Button>
        </span>
      </Head>
      {invalid && <ErrorLine>{invalid}</ErrorLine>}
      {failedParts && <ErrorLine>{t("entitySave.saveFailed", { parts: failedParts })} {save.error && contentError(m, save.error)}</ErrorLine>}
      {/* What the entity is called and what's in it on the left; its settings and pictures beside them. */}
      <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,1fr)_20rem]">
        <div className="grid min-w-0 gap-6">
          <Names names={names} languages={languages} language={language} onLanguage={setLanguage} kind={nameKind} onKind={setNameKind} text={newName} onText={(newName) => draft.edit((f) => ({ ...f, newName }))} onChange={(names) => draft.edit((f) => ({ ...f, names }))} onAdd={() => draft.edit((f) => ({ ...f, names: pendingNames(), newName: "" }))} />
          <Texts form={text} languages={offered} onChange={(l, patch) => draft.edit((f) => ({ ...f, text: { ...f.text, [l]: { ...f.text[l]!, ...patch } } }))} />
          {p.children?.(entity)}
        </div>
        <aside className="grid min-w-0 gap-6">
          <Details management={p.management} entity={entity} spec={spec} form={details} onChange={(patch) => draft.edit((f) => ({ ...f, details: { ...f.details, ...patch } }))} labels={p.labels} client={p.client} />
          <Images management={p.management} entity={entity} spec={spec} onError={p.onError} client={p.client} />
          <Edges management={p.management} entity={entity} g={g} kinds={kinds} labels={p.labels} onError={p.onError} client={p.client} />
        </aside>
      </div>
    </>
  );
}

function Section({ title, children, ckui, action, note }: { title: ReactNode; children: ReactNode; ckui: string; action?: ReactNode; note?: ReactNode }) {
  return (
    <section className="grid gap-4 rounded-lg border border-border bg-muted/30 p-4" data-ckui={ckui}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="grid gap-0.5">
          <h3 className="text-sm font-medium">{title}</h3>
          {note && <p className="text-xs text-muted-foreground">{note}</p>}
        </div>
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
  children,
}: {
  management?: boolean;
  entity: EntityDetail;
  g: UseEntity;
  labels?: Labels;
  onMerged?: (r: MergeReport) => void;
  onDeleted?: (g: Entity) => void;
  onError?: ContentKitErrorHandler;
  client?: ContentKitClient;
  /** Actions before the menu, such as the editor's Save. */
  children?: ReactNode;
}) {
  const { t } = useMessages();
  const [merging, setMerging] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const remove = useAction("entity.save", onError);
  const name = entity.name || entity.slug;
  return (
    // Pinned below any host header that sets --ckui-sticky-top, so Save stays in reach down the page.
    <div
      className="sticky top-[var(--ckui-sticky-top,0px)] z-10 -mx-1 flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-border bg-background px-1 py-3 transition-[top] duration-300 motion-reduce:transition-none"
      data-ckui="entity-head"
    >
      <div className="flex min-w-0 flex-1 items-baseline gap-2">
        <h2 className="truncate text-xl font-semibold tracking-tight" title={name}>{name}</h2>
        <span className="shrink-0 text-sm text-muted-foreground">{labelOf(labels, entity.kind, "one")}</span>
      </div>
      <div className="flex items-center gap-2">
        {children}
        <DropdownMenu>
          <DropdownMenuTrigger render={<Button size="icon-sm" variant="ghost" disabled={remove.pending} aria-label={t("contentManager.moreActions", { title: name })} />}>
            <HugeiconsIcon icon={MoreHorizontalIcon} strokeWidth={2} />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="min-w-40">
            <DropdownMenuItem onClick={() => setMerging(true)}>
              <HugeiconsIcon icon={GitMergeIcon} strokeWidth={2} />
              {t("entityEditor.merge")}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => setConfirmDelete(true)}>
              <HugeiconsIcon icon={Delete02Icon} strokeWidth={2} />
              {t("entityEditor.delete")}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      <ErrorLine error={remove.error} className="basis-full" />
      <EntityMergeDialog
        open={merging}
        onOpenChange={setMerging}
        entity={entity}
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

const Required = () => <span aria-hidden className="text-destructive"> *</span>;

/** A parent or scope: the chosen entity with a change and a clear, or a picker. */
function EntityField({ label, kind, value, onChange, exclude, required, client, management }: { management?: boolean; label: string; kind: string; value: EntityLike | null; onChange: (g: EntityLike | null) => void; exclude?: string[]; required?: boolean; client?: ContentKitClient }) {
  const { t } = useMessages();
  return (
    <div className="grid gap-1.5" data-ckui="entity-field">
      <span className="text-sm font-medium">
        {label}
        {required && <Required />}
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

function CreateForm({ kind, spec, languages, labels, g, onCreated, onCancel, onError, client, management }: EntityEditorProps & { spec: EntityKind; languages: readonly string[]; g: UseEntity }) {
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
  const first0 = languages[0]!;
  const others = languages.slice(1);
  const first = names[first0] ?? "";
  const filledOthers = others.filter((l) => names[l]?.trim());
  const [othersOpen, setOthersOpen] = useState(true);
  const kindOne = labelOf(labels, kind, "one");
  const title = t("entityEditor.newTitle", { kind: kindOne });
  // A language in its own words ("中文"), beside its name in the UI's language.
  const nativeName = (l: string) => {
    try {
      return new Intl.DisplayNames([l], { type: "language" }).of(l) ?? l;
    } catch {
      return l;
    }
  };
  // The page address it will have: the host's route for the kind, the code it gets on creation shown as "…".
  const urls = useContext(ContentKitContext)?.urls;
  const address = useMemo(() => {
    if (!urls) return null;
    try {
      const path = urls.path({ content_kind: kind, code: "0000000000", slug: "s" });
      const at = path.lastIndexOf("0000000000");
      return at < 0 ? null : { prefix: `${path.slice(0, at)}…/` };
    } catch {
      return null;
    }
  }, [urls, kind]);
  // Picked attribute values, e.g. the Type, read beside the kind.
  const facts = Object.entries(attrs).flatMap(([name, v]) => (typeof v === "string" && v && declared[name]?.values ? [labelOf(labels, v, "one")] : []));
  // What Create waits for, named as the labels name it.
  const missing = [
    !first.trim() && t("entityEditor.nameIn", { language: languageName(languages[0]!) }),
    !slug.trim() && t("entityEditor.slug"),
    spec.scoped_by && !scope && labelOf(labels, spec.scoped_by, "one"),
  ].filter((x): x is string => !!x);
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
      className="grid items-start gap-6 lg:grid-cols-[minmax(0,1fr)_19rem]"
      data-ckui="entity-create"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <div className="grid gap-1 lg:col-span-2">
        <h2 className="text-2xl font-semibold tracking-tight">{title}</h2>
        <p className="text-sm text-muted-foreground">{t("entityEditor.createIntro")}</p>
      </div>
      <div className="grid min-w-0 gap-5">
        <Card title={t("entityEditor.identity")} hint={t("entityEditor.identityHint")} ckui="entity-create-identity">
          <div className="grid gap-1.5">
            <Label htmlFor={`${id}-${first0}`}>
              {t("entityEditor.nameIn", { language: languageName(first0) })}
              <Required />
            </Label>
            <Input id={`${id}-${first0}`} className="h-10" lang={first0} required autoFocus value={names[first0] ?? ""} onChange={(e) => setNames((n) => ({ ...n, [first0]: e.target.value }))} />
          </div>
          {spec.scoped_by && <EntityField management={management} label={labelOf(labels, spec.scoped_by, "one")} kind={spec.scoped_by} value={scope} onChange={setScope} required client={client} />}
          <div className="grid gap-1.5">
            <Label htmlFor={`${id}-slug`}>
              {t("entityEditor.slug")}
              <Required />
            </Label>
            {/* Reads as the page address: the host's route before the slug, the code it gets on creation as "…". */}
            <div className="flex h-10 min-w-0 items-stretch overflow-hidden rounded-md border border-input bg-transparent focus-within:border-ring focus-within:ring-3 focus-within:ring-ring/50 dark:bg-input/30">
              {address && (
                <span aria-hidden className="flex max-w-[45%] shrink-0 items-center truncate border-e border-input bg-muted/50 px-3 font-mono text-sm text-muted-foreground">
                  {address.prefix}
                </span>
              )}
              <input
                id={`${id}-slug`}
                className="min-w-0 flex-1 bg-transparent px-3 font-mono text-sm outline-none"
                required
                aria-describedby={`${id}-slug-hint`}
                value={slug}
                onChange={(e) => {
                  setSlugEdited(true);
                  setSlug(e.target.value);
                }}
              />
            </div>
            <p id={`${id}-slug-hint`} className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <HugeiconsIcon icon={Link01Icon} strokeWidth={2} className="size-3.5 shrink-0" />
              {t("entityEditor.slugHint")}
            </p>
          </div>
        </Card>
        {others.length > 0 && (
          <section className="rounded-lg border border-border bg-muted/30" data-ckui="entity-create-other-names">
            <button
              type="button"
              className="flex w-full items-start gap-3 p-5 text-start outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
              aria-expanded={othersOpen}
              aria-controls={`${id}-others`}
              onClick={() => setOthersOpen((o) => !o)}
            >
              <span className="grid min-w-0 flex-1 gap-0.5">
                <span className="text-sm font-medium">
                  {t("entityEditor.otherNames")}
                  <span className="font-normal text-muted-foreground"> · {t("entityEditor.optional")}</span>
                </span>
                <span className="text-xs text-muted-foreground">{t("entityEditor.otherNamesHint", { language: languageName(first0) })}</span>
              </span>
              <span className="shrink-0 rounded-full border border-border px-2 py-0.5 text-xs text-muted-foreground tabular-nums">
                {t("entityEditor.namesAdded", { count: filledOthers.length, total: others.length })}
              </span>
              <HugeiconsIcon icon={ArrowDown01Icon} strokeWidth={2} className={cn("mt-0.5 size-4 shrink-0 text-muted-foreground transition-transform", othersOpen && "rotate-180")} />
            </button>
            <div id={`${id}-others`} hidden={!othersOpen} className="grid gap-3 border-t border-border p-5">
              {others.map((l) => (
                <div key={l} className="grid gap-2 sm:grid-cols-[11rem_minmax(0,1fr)] sm:items-center sm:gap-4">
                  <Label htmlFor={`${id}-${l}`} className="flex items-center gap-3 font-normal">
                    <span aria-hidden className="grid h-6 min-w-8 place-items-center rounded border border-border bg-muted px-1 font-mono text-[11px] font-medium text-muted-foreground uppercase">{l}</span>
                    <span className="sr-only">{t("entityEditor.nameIn", { language: languageName(l) })}</span>
                    <span aria-hidden className="grid leading-tight">
                      <span className="text-sm font-medium">{languageName(l)}</span>
                      {nativeName(l) !== languageName(l) && <span className="text-xs text-muted-foreground">{nativeName(l)}</span>}
                    </span>
                  </Label>
                  <Input id={`${id}-${l}`} className="h-10" lang={l} placeholder={t("entityEditor.namePlaceholder", { language: languageName(l) })} value={names[l] ?? ""} onChange={(e) => setNames((n) => ({ ...n, [l]: e.target.value }))} />
                </div>
              ))}
            </div>
          </section>
        )}
        {(spec.parent || management || Object.keys(declared).length > 0) && (
          <Card title={t("entityEditor.visibility")} ckui="entity-create-details">
            {management && (
              <div className="flex items-start justify-between gap-4 rounded-md border border-border p-4">
                <div className="grid gap-1">
                  <Label htmlFor={`${id}-internal`}>{t("entityEditor.internal")}</Label>
                  <p className="text-xs text-muted-foreground">{t("entityEditor.internalHint")}</p>
                </div>
                <Switch id={`${id}-internal`} checked={internal} onCheckedChange={setInternal} />
              </div>
            )}
            {spec.parent && <EntityField management={management} label={t("entityEditor.parent")} kind={spec.parent} value={parent} onChange={setParent} client={client} />}
            <AttributeFields declared={declared} form={attrs} onChange={(n, v) => setAttrs((a) => ({ ...a, [n]: v }))} labels={labels} />
          </Card>
        )}
        {invalid && <ErrorLine>{invalid}</ErrorLine>}
        {create.error && <ErrorLine>{contentError(m, create.error)}</ErrorLine>}
        <div className="flex flex-wrap items-center justify-end gap-3">
          <p className="me-auto text-xs text-muted-foreground">
            <span className="text-destructive">*</span> {t("entityEditor.required")}
            {missing.length > 0 && (
              <>
                {" · "}
                <span id={`${id}-missing`} data-ckui="entity-create-missing">
                  {t("entityEditor.stillNeeded", { fields: missing.join(", ") })}
                </span>
              </>
            )}
          </p>
          {onCancel && (
            <Button type="button" variant="outline" disabled={create.pending} onClick={onCancel}>
              {t("common.cancel")}
            </Button>
          )}
          <Button type="submit" disabled={create.pending || missing.length > 0} aria-describedby={missing.length ? `${id}-missing` : undefined}>
            <HugeiconsIcon icon={Add01Icon} strokeWidth={2} data-icon="inline-start" />
            {create.pending ? t("common.saving") : t("entityEditor.createNamed", { kind: kindOne })}
          </Button>
        </div>
      </div>
      {/* How the new entity reads, as it is typed. */}
      <aside className="hidden lg:sticky lg:top-[calc(var(--ckui-sticky-top,0px)+1rem)] lg:block" aria-label={t("entityEditor.preview")} data-ckui="entity-create-preview">
        <div className="overflow-hidden rounded-lg border border-border bg-muted/30">
          <div className="flex items-center justify-between gap-2 border-b border-border px-5 py-3">
            <p className="text-xs font-medium tracking-wide text-muted-foreground uppercase">{t("entityEditor.preview")}</p>
            <span className={cn("rounded-full px-2 py-0.5 text-xs font-medium ring-1 ring-inset", internal ? "bg-muted text-muted-foreground ring-border" : "bg-success/15 text-foreground ring-success/45")}>
              {t(internal ? "entityEditor.internalShort" : "entityEditor.public")}
            </span>
          </div>
          <div className="grid gap-4 p-5">
            <div className="flex min-w-0 items-center gap-3">
              <span aria-hidden className="grid size-12 shrink-0 place-items-center rounded-full bg-primary/15 text-lg font-semibold text-primary">
                {(first.trim()[0] ?? "?").toUpperCase()}
              </span>
              <div className="grid min-w-0">
                <span className={cn("truncate font-semibold", !first.trim() && "text-muted-foreground")}>{first.trim() || title}</span>
                <span className="truncate text-sm text-muted-foreground">{[kindOne, ...facts].join(" · ")}</span>
              </div>
            </div>
            {slug.trim() && (
              <p className="flex min-w-0 items-center gap-2 rounded-md border border-border px-3 py-2 font-mono text-xs">
                <HugeiconsIcon icon={Link01Icon} strokeWidth={2} className="size-3.5 shrink-0 text-muted-foreground" />
                <span className="truncate">{(address?.prefix ?? "") + slug.trim()}</span>
              </p>
            )}
            {filledOthers.length > 0 && (
              <div className="grid gap-1.5">
                <p className="text-xs text-muted-foreground">{t("entityEditor.otherNames")}</p>
                <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1 text-sm">
                  {filledOthers.map((l) => (
                    <div key={l} className="contents">
                      <dt className="text-muted-foreground">{languageName(l)}</dt>
                      <dd className="truncate" lang={l}>{names[l]!.trim()}</dd>
                    </div>
                  ))}
                </dl>
              </div>
            )}
          </div>
        </div>
      </aside>
    </form>
  );
}

/** A card with a heading and an optional one-line hint. */
function Card({ title, hint, ckui, children }: { title: ReactNode; hint?: ReactNode; ckui: string; children: ReactNode }) {
  return (
    <section className="grid gap-4 rounded-lg border border-border bg-muted/30 p-5" data-ckui={ckui}>
      <div className="grid gap-0.5">
        <h3 className="text-sm font-medium">{title}</h3>
        {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
      </div>
      {children}
    </section>
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

function Details({ entity, spec, form, onChange: edit, labels, client, management }: { management?: boolean; entity: EntityDetail; spec: EntityKind; form: DetailsValue; onChange: (patch: Partial<DetailsValue>) => void; labels?: Labels; client?: ContentKitClient }) {
  const { t } = useMessages();
  const id = useId();
  const { slug, internal, parent, scope, attrs } = form;
  return (
    <Section title={t("entityEditor.details")} ckui="entity-details">
      <div className="grid gap-3">
        <div className="grid gap-1.5">
          <Label htmlFor={`${id}-slug`}>
            {t("entityEditor.slug")}
            <Required />
          </Label>
          <Input id={`${id}-slug`} className="h-8" required value={slug} onChange={(e) => edit({ slug: e.target.value })} />
        </div>
        {spec.scoped_by && <EntityField management={management} label={labelOf(labels, spec.scoped_by, "one")} kind={spec.scoped_by} value={scope} onChange={(scope) => edit({ scope })} required client={client} />}
        {spec.parent && <EntityField management={management} label={t("entityEditor.parent")} kind={spec.parent} value={parent} onChange={(parent) => edit({ parent })} exclude={[entity.id]} client={client} />}
        {management && <InternalField id={`${id}-internal`} checked={internal} onChange={(internal) => edit({ internal })} />}
        <AttributeFields declared={spec.attributes ?? {}} form={attrs} onChange={(n, v) => edit({ attrs: { ...attrs, [n]: v } })} labels={labels} />
      </div>
    </Section>
  );
}

function Names({ names, languages, language, onLanguage, kind, onKind, text, onText, onChange, onAdd }: { names: Name[]; languages: readonly string[]; language: string; onLanguage: (language: string) => void; kind: Name["kind"]; onKind: (kind: Name["kind"]) => void; text: string; onText: (text: string) => void; onChange: (names: Name[]) => void; onAdd: () => void }) {
  const { t } = useMessages();
  const id = useId();
  const languageName = useLanguageName();
  const sorted = [...names].sort((a, b) => a.language.localeCompare(b.language) || (a.kind === b.kind ? a.name.localeCompare(b.name) : a.kind === "name" ? -1 : 1));
  const offered = [...new Set([...languages, ...names.map((n) => n.language)])];
  return (
    <Section title={t("entityEditor.names")} ckui="entity-names-editor">
      {sorted.length ? (
        <ul className="grid divide-y divide-border rounded-md border border-border">
          {sorted.map((n) => (
            <li key={`${n.language}:${n.kind}:${n.name}`} className="flex flex-wrap items-center gap-2 p-2.5" data-ckui="entity-name" data-kind={n.kind} data-language={n.language}>
              <span className="w-20 shrink-0 text-xs text-muted-foreground">{languageName(n.language)}</span>
              <span className="min-w-0 flex-1 break-words" lang={n.language}>
                {n.name}
              </span>
              <span className="text-xs text-muted-foreground">{t(n.kind === "name" ? "entityEditor.canonical" : "entityEditor.alias")}</span>
              {n.kind === "alias" && (
                <Button size="xs" variant="ghost" onClick={() => onChange(addNames(names, [{ ...n, kind: "name" }]))}>
                  {t("entityEditor.makeCanonical")}
                </Button>
              )}
              <Button size="xs" variant="ghost" aria-label={t("entityEditor.removeName", { name: n.name })} onClick={() => onChange(names.filter((name) => name !== n))}>
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
          onAdd();
        }}
      >
        {offered.length > 1 && (
          <Select value={language} onValueChange={(v) => onLanguage(String(v))}>
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
        <Select value={kind} onValueChange={(v) => onKind(v as Name["kind"])}>
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
        <Input id={`${id}-name`} className="h-8 min-w-40 flex-1" lang={language} value={text} placeholder={t(kind === "alias" ? "entityEditor.newAliases" : "entityEditor.newName")} onChange={(e) => onText(e.target.value)} />
        <Button type="submit" size="sm" variant="outline" disabled={!text.trim()}>
          {t("entityEditor.addName")}
        </Button>
      </form>
    </Section>
  );
}

function Texts({ form, languages, onChange: set }: { form: Record<string, Text>; languages: readonly string[]; onChange: (language: string, patch: Partial<Text>) => void }) {
  const { t } = useMessages();
  const id = useId();
  const languageName = useLanguageName();
  return (
    <Section title={t("entityEditor.text")} ckui="entity-text">
      {/* One row per language: its description beside the key it sorts by. */}
      <div className="grid gap-3">
        <div aria-hidden className="hidden gap-3 text-xs font-medium text-muted-foreground md:grid md:grid-cols-[6rem_minmax(0,1fr)_11rem]">
          <span />
          <span>{t("entityEditor.description")}</span>
          <span>{t("entityEditor.sortKey")}</span>
        </div>
        {languages.map((l) => (
          <div key={l} role="group" aria-labelledby={`${id}-${l}-l`} className="grid gap-2 md:grid-cols-[6rem_minmax(0,1fr)_11rem] md:items-start md:gap-3" data-language={l}>
            <span id={`${id}-${l}-l`} className="pt-1.5 text-sm font-medium">{languageName(l)}</span>
            <Label htmlFor={`${id}-${l}-d`} className="sr-only">
              {t("entityEditor.descriptionIn", { language: languageName(l) })}
            </Label>
            <Textarea
              id={`${id}-${l}-d`}
              lang={l}
              rows={2}
              placeholder={t("entityEditor.description")}
              value={form[l]?.description ?? ""}
              onChange={(e) => set(l, { description: e.target.value })}
            />
            <Input
              id={`${id}-${l}-s`}
              className="h-8"
              lang={l}
              placeholder={t("entityEditor.sortKey")}
              aria-label={t("entityEditor.sortKeyIn", { language: languageName(l) })}
              value={form[l]?.sort_key ?? ""}
              onChange={(e) => set(l, { sort_key: e.target.value })}
            />
          </div>
        ))}
      </div>
    </Section>
  );
}

function Images({ entity, spec, onError, client, management }: { management?: boolean; entity: EntityDetail; spec: EntityKind; onError?: ContentKitErrorHandler; client?: ContentKitClient }) {
  const { t } = useMessages();
  const c = entity.cover;
  const mode = c?.image ? "image" : c?.content_id && !c.auto ? "content" : "auto";
  const avatar = !!spec.avatar;
  return (
    <Section title={t(avatar ? "entityEditor.images" : "entityEditor.cover")} ckui="entity-cover-editor" note={t("entityEditor.savesNow")}>
      <p className="text-muted-foreground" data-ckui="cover-mode">
        {t(mode === "image" ? "entityEditor.coverImage" : mode === "content" ? "entityEditor.coverContent" : "entityEditor.coverAuto")}
      </p>
      <EntityImageEditor management={management} entity={entity} slot="cover" client={client} onError={onError} />
      {avatar && <EntityImageEditor management={management} entity={entity} slot="avatar" client={client} onError={onError} />}
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
    <Section title={t("entityEditor.edges")} ckui="entity-edges">
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
