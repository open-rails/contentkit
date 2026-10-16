import { cn } from "cn";
import { useCallback, useEffect, useId, useRef, useState, type ReactNode } from "react";
import type { ContentKitUiAppearance } from "../../appearance.js";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import { parseAttributes, slugify } from "../../client/group-fields.js";
import type { ContentModel, Group, GroupCard, GroupDetail, GroupKind, GroupsText as Text, MergeReport, Name } from "../../client/generated/wire.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import type { ContentKitErrorHandler } from "../../react/context.js";
import { useGroup, useKinds, type UseGroup } from "../../react/groups.js";
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
import { GroupImageEditor } from "./banner.js";
import { GroupPicker, labelOf, useGroupNames, type GroupLike, type Labels } from "./parts.js";

export interface GroupEditorProps {
  /** The group edited; null creates one of `kind`. */
  id: string | null;
  /** Staff management, including internal entities and their visibility setting. */
  management?: boolean;
  /** The kind a new group is created as. */
  kind: string;
  /** Languages names and text are written in; default the content model's. */
  languages?: readonly string[];
  labels?: Labels;
  onCreated?: (group: Group) => void;
  /** After a merge: the survivor's id is report.into. */
  onMerged?: (report: MergeReport) => void;
  onDeleted?: (group: Group) => void;
  /** Below the editor's sections, with the loaded group: the members, host fields. */
  children?: (group: GroupDetail) => ReactNode;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
  className?: string;
  appearance?: ContentKitUiAppearance;
}


/**
 * Staff (root:groups:write): creates a group, or edits one: slug, parent,
 * scope and declared attributes; names and aliases per language; description
 * and sort key per language; cover (an upload, or automatic); edges to other
 * groups; merge into another group; delete and restore.
 */
export function GroupEditor(p: GroupEditorProps) {
  const { t } = useMessages();
  const { kinds, loading: kindsLoading } = useKinds({ client: p.client });
  const g = useGroup(p.id, { client: p.client, management: p.management });
  const languages = p.languages ?? kinds?.languages ?? ["en"];
  const kind = g.group?.kind ?? p.kind;
  const spec: GroupKind = kinds?.entity_kinds?.[kind] ?? {};
  const root = (children: ReactNode) => (
    <ContentKitUiRoot appearance={p.appearance} className={cn("grid gap-6 text-sm", p.className)} data-ckui="group-editor" data-kind={kind}>
      {children}
    </ContentKitUiRoot>
  );
  if (kindsLoading) return root(<Skeleton className="h-40 w-full" />);
  // A group created here is edited in place until the host routes to it.
  if (!p.id && !g.group && !g.loading) return root(<CreateForm {...p} kind={kind} spec={spec} languages={languages} g={g} />);
  if (g.deleted)
    return root(
      <Deleted
        group={g.deleted}
        onRestore={() => g.restore()}
        onError={p.onError}
        name={g.deleted.slug}
      />,
    );
  if (!g.group) {
    if (g.error)
      return root(
        <div role="alert" className="grid justify-items-start gap-2">
          <p className="text-destructive">{t("groupEditor.loadFailed")}</p>
          <Button size="sm" variant="outline" onClick={g.reload}>
            {t("common.retry")}
          </Button>
        </div>,
      );
    return root(<Skeleton className="h-40 w-full" />);
  }
  const group = g.group;
  return root(
    <>
      <Head management={p.management} group={group} g={g} labels={p.labels} onMerged={p.onMerged} onDeleted={p.onDeleted} onError={p.onError} client={p.client} />
      <Details management={p.management} key={group.id} group={group} spec={spec} g={g} labels={p.labels} onError={p.onError} client={p.client} />
      <Names group={group} g={g} languages={languages} onError={p.onError} />
      <Texts key={group.id} group={group} g={g} languages={languages} onError={p.onError} />
      <Images management={p.management} group={group} spec={spec} onError={p.onError} client={p.client} />
      {spec.sellable && <Membership group={group} onError={p.onError} client={p.client} />}
      <Edges management={p.management} group={group} g={g} kinds={kinds} labels={p.labels} onError={p.onError} client={p.client} />
      {p.children?.(group)}
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

function Deleted({ group, name, onRestore, onError }: { group: Group; name: string; onRestore: () => Promise<Group>; onError?: ContentKitErrorHandler }) {
  const { t } = useMessages();
  const restore = useAction("group.save", onError);
  return (
    <div className="grid justify-items-start gap-2" data-ckui="group-deleted" data-state={group.state}>
      <p>{t("groupEditor.deletedNotice", { name })}</p>
      <Button size="sm" variant="outline" disabled={restore.pending} onClick={() => void restore.run(onRestore)}>
        {t("groupEditor.restore")}
      </Button>
      <ErrorLine error={restore.error} />
    </div>
  );
}

function Head({
  group,
  g,
  labels,
  onMerged,
  onDeleted,
  onError,
  client,
  management,
}: {
  management?: boolean;
  group: GroupDetail;
  g: UseGroup;
  labels?: Labels;
  onMerged?: (r: MergeReport) => void;
  onDeleted?: (g: Group) => void;
  onError?: ContentKitErrorHandler;
  client?: ContentKitClient;
}) {
  const { t } = useMessages();
  const [merging, setMerging] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const remove = useAction("group.save", onError);
  return (
    <div className="flex flex-wrap items-center gap-2" data-ckui="group-head">
      <h2 className="text-base font-semibold break-words">{group.name || group.slug}</h2>
      <Badge variant="outline">{labelOf(labels, group.kind)}</Badge>
      <div className="ms-auto flex flex-wrap gap-2">
        <Button size="sm" variant="outline" onClick={() => setMerging(true)}>
          {t("groupEditor.merge")}
        </Button>
        <Button size="sm" variant="destructive" disabled={remove.pending} onClick={() => setConfirmDelete(true)}>
          {t("groupEditor.delete")}
        </Button>
      </div>
      <ErrorLine error={remove.error} className="basis-full" />
      <GroupMergeDialog
        open={merging}
        onOpenChange={setMerging}
        group={group}
        management={management}
        client={client}
        onMerge={async (into) => {
          const report = await g.merge(into.id);
          onMerged?.(report);
          return report;
        }}
      />
      <Dialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <DialogContent data-ckui="group-delete-dialog">
          <DialogHeader>
            <DialogTitle>{t("groupEditor.deleteTitle", { name: group.name || group.slug })}</DialogTitle>
            <DialogDescription>{t("groupEditor.deleteBody")}</DialogDescription>
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
              {t("groupEditor.delete")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

export interface GroupMergeDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The group merged away. */
  group: GroupDetail;
  /** Merges into the chosen group: memberships, names, edges and children move, its code redirects. */
  onMerge: (into: GroupCard) => Promise<MergeReport>;
  management?: boolean;
  language?: string;
  client?: ContentKitClient;
}

/** Picks a group of the same kind and merges this one into it, after typing the confirmation word. */
export function GroupMergeDialog({ open, onOpenChange, group, onMerge, language, client, management }: GroupMergeDialogProps) {
  const m = useMessages();
  const { t } = m;
  const id = useId();
  const [into, setInto] = useState<GroupCard | null>(null);
  const [word, setWord] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<ContentKitError | null>(null);
  useEffect(() => {
    if (!open) return;
    setInto(null);
    setWord("");
    setError(null);
  }, [open]);
  const typed = t("groupEditor.mergeWord");
  const ready = !!into && word.trim().toLowerCase() === typed.toLowerCase();
  const name = group.name || group.slug;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent data-ckui="group-merge-dialog">
        <DialogHeader>
          <DialogTitle>{t("groupEditor.mergeTitle", { name })}</DialogTitle>
          <DialogDescription>{t("groupEditor.mergeBody")}</DialogDescription>
        </DialogHeader>
        {into ? (
          <div className="flex flex-wrap items-center gap-2" data-ckui="merge-target">
            <span>{t("groupEditor.mergeInto", { name: into.name || into.slug })}</span>
            <Button size="xs" variant="ghost" onClick={() => setInto(null)}>
              {t("groupEditor.change")}
            </Button>
          </div>
        ) : (
          <GroupPicker management={management} kind={group.kind} language={language} exclude={[group.id]} label={t("groupEditor.mergeTarget")} onPick={setInto} client={client} autoFocus />
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
            {t("groupEditor.merge")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** A parent or scope: the chosen group with a change and a clear, or a picker. */
function GroupField({ label, kind, value, onChange, exclude, required, client, management }: { management?: boolean; label: string; kind: string; value: GroupLike | null; onChange: (g: GroupLike | null) => void; exclude?: string[]; required?: boolean; client?: ContentKitClient }) {
  const { t } = useMessages();
  return (
    <div className="grid gap-1.5" data-ckui="group-field">
      <span className="text-sm font-medium">
        {label}
        {required && <span aria-hidden> *</span>}
      </span>
      {value ? (
        <div className="flex flex-wrap items-center gap-2">
          <span className="rounded-md border border-border px-2 py-0.5" data-ckui="group-field-value">
            {value.name || value.slug}
          </span>
          <Button type="button" size="xs" variant="ghost" onClick={() => onChange(null)} aria-label={t("groupEditor.clearField", { field: label })}>
            {required ? t("groupEditor.change") : t("common.remove")}
          </Button>
        </div>
      ) : (
        <GroupPicker management={management} kind={kind} label={label} exclude={exclude} onPick={onChange} client={client} />
      )}
    </div>
  );
}

function CreateForm({ kind, spec, languages, labels, g, onCreated, onError, client, management }: GroupEditorProps & { spec: GroupKind; languages: readonly string[]; g: UseGroup }) {
  const m = useMessages();
  const { t } = m;
  const id = useId();
  const languageName = useLanguageName();
  const [names, setNames] = useState<Record<string, string>>({});
  const [slug, setSlug] = useState("");
  const [slugEdited, setSlugEdited] = useState(false);
  const [internal, setInternal] = useState(false);
  const [parent, setParent] = useState<GroupLike | null>(null);
  const [scope, setScope] = useState<GroupLike | null>(null);
  const declared = spec.attributes ?? {};
  const [attrs, setAttrs] = useState<Record<string, string | boolean>>(() => formOf(declared, {}));
  const [invalid, setInvalid] = useState<string | null>(null);
  const create = useAction("group.save", onError);
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
      data-ckui="group-create"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <h2 className="text-base font-semibold">{t("groupEditor.newTitle", { kind: labelOf(labels, kind) })}</h2>
      {languages.map((l, i) => (
        <div key={l} className="grid gap-1.5">
          <Label htmlFor={`${id}-${l}`}>{t("groupEditor.nameIn", { language: languageName(l) })}</Label>
          <Input id={`${id}-${l}`} className="h-8" lang={l} required={i === 0} value={names[l] ?? ""} onChange={(e) => setNames((n) => ({ ...n, [l]: e.target.value }))} />
        </div>
      ))}
      <div className="grid gap-1.5">
        <Label htmlFor={`${id}-slug`}>{t("groupEditor.slug")}</Label>
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
      {spec.scoped_by && <GroupField management={management} label={labelOf(labels, spec.scoped_by)} kind={spec.scoped_by} value={scope} onChange={setScope} required client={client} />}
      {spec.parent && <GroupField management={management} label={t("groupEditor.parent")} kind={spec.parent} value={parent} onChange={setParent} client={client} />}
      {management && <InternalField id={`${id}-internal`} checked={internal} onChange={setInternal} />}
      <AttributeFields declared={declared} form={attrs} onChange={(n, v) => setAttrs((a) => ({ ...a, [n]: v }))} labels={labels} />
      {invalid && <ErrorLine>{invalid}</ErrorLine>}
      {create.error && <ErrorLine>{contentError(m, create.error)}</ErrorLine>}
      <Button type="submit" className="justify-self-start" disabled={create.pending || !first.trim() || !slug.trim() || (!!spec.scoped_by && !scope)}>
        {create.pending ? t("common.saving") : t("groupEditor.create")}
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
        <Label htmlFor={id}>{t("groupEditor.internal")}</Label>
        <p className="text-xs text-muted-foreground">{t("groupEditor.internalHint")}</p>
      </div>
    </div>
  );
}

function Details({ group, spec, g, labels, onError, client, management }: { management?: boolean; group: GroupDetail; spec: GroupKind; g: UseGroup; labels?: Labels; onError?: ContentKitErrorHandler; client?: ContentKitClient }) {
  const m = useMessages();
  const { t } = m;
  const id = useId();
  const declared = spec.attributes ?? {};
  const [form, setForm, settle] = useSeeded(
    () => ({ slug: group.slug, internal: group.internal ?? false, parent: (group.parent ?? null) as GroupLike | null, scope: (group.scope ?? null) as GroupLike | null, attrs: formOf(declared, group.attributes) }),
    group.revision,
  );
  const { slug, internal, parent, scope, attrs } = form;
  const edit = (patch: Partial<typeof form>) => {
    setSaved(false);
    setForm((f) => ({ ...f, ...patch }));
  };
  const setSlug = (v: string) => edit({ slug: v });
  const setParent = (v: GroupLike | null) => edit({ parent: v });
  const setScope = (v: GroupLike | null) => edit({ scope: v });
  const setAttrs = (fn: (a: Record<string, string | boolean>) => Record<string, string | boolean>) => edit({ attrs: fn(attrs) });
  const [invalid, setInvalid] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const save = useAction("group.save", onError);
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
    <Section title={t("groupEditor.details")} ckui="group-details">
      <form
        className="grid gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <div className="grid gap-1.5">
          <Label htmlFor={`${id}-slug`}>{t("groupEditor.slug")}</Label>
          <Input id={`${id}-slug`} className="h-8" required value={slug} onChange={(e) => setSlug(e.target.value)} />
        </div>
        {spec.scoped_by && <GroupField management={management} label={labelOf(labels, spec.scoped_by)} kind={spec.scoped_by} value={scope} onChange={setScope} required client={client} />}
        {spec.parent && <GroupField management={management} label={t("groupEditor.parent")} kind={spec.parent} value={parent} onChange={setParent} exclude={[group.id]} client={client} />}
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
              {t("groupEditor.saved")}
            </span>
          )}
        </div>
      </form>
    </Section>
  );
}

function Names({ group, g, languages, onError }: { group: GroupDetail; g: UseGroup; languages: readonly string[]; onError?: ContentKitErrorHandler }) {
  const { t } = useMessages();
  const id = useId();
  const languageName = useLanguageName();
  const [language, setLanguage] = useState(languages[0]!);
  const [kind, setKind] = useState<Name["kind"]>("alias");
  const [text, setText] = useState("");
  const save = useAction("group.save", onError);
  const sorted = [...group.names].sort((a, b) => a.language.localeCompare(b.language) || (a.kind === b.kind ? a.name.localeCompare(b.name) : a.kind === "name" ? -1 : 1));
  const offered = [...new Set([...languages, ...group.names.map((n) => n.language)])];
  return (
    <Section title={t("groupEditor.names")} ckui="group-names-editor">
      {sorted.length ? (
        <ul className="grid divide-y divide-border rounded-lg border border-border">
          {sorted.map((n) => (
            <li key={`${n.language}:${n.kind}:${n.name}`} className="flex flex-wrap items-center gap-2 p-2.5" data-ckui="group-name" data-kind={n.kind} data-language={n.language}>
              <Badge variant="outline">{languageName(n.language)}</Badge>
              <span className="min-w-0 flex-1 break-words" lang={n.language}>
                {n.name}
              </span>
              <span className="text-xs text-muted-foreground">{t(n.kind === "name" ? "groupEditor.canonical" : "groupEditor.alias")}</span>
              {n.kind === "alias" && (
                <Button size="xs" variant="ghost" disabled={save.pending} onClick={() => void save.run(() => g.names("add", [{ ...n, kind: "name" }]))}>
                  {t("groupEditor.makeCanonical")}
                </Button>
              )}
              <Button size="xs" variant="ghost" disabled={save.pending} aria-label={t("groupEditor.removeName", { name: n.name })} onClick={() => void save.run(() => g.names("remove", [n]))}>
                {t("common.remove")}
              </Button>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-muted-foreground">{t("groupEditor.noNames")}</p>
      )}
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          const name = text.trim();
          if (name) void save.run(() => g.names("add", [{ language, kind, name }])).then((d) => d && setText(""));
        }}
      >
        {offered.length > 1 && (
          <Select value={language} onValueChange={(v) => setLanguage(String(v))}>
            <SelectTrigger size="sm" aria-label={t("groupEditor.language")}>
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
          <SelectTrigger size="sm" aria-label={t("groupEditor.nameKind")}>
            <SelectValue>{(v: string) => t(v === "name" ? "groupEditor.canonical" : "groupEditor.alias")}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="alias">{t("groupEditor.alias")}</SelectItem>
            <SelectItem value="name">{t("groupEditor.canonical")}</SelectItem>
          </SelectContent>
        </Select>
        <Label htmlFor={`${id}-name`} className="sr-only">
          {t("groupEditor.newName")}
        </Label>
        <Input id={`${id}-name`} className="h-8 min-w-40 flex-1" lang={language} value={text} placeholder={t("groupEditor.newName")} onChange={(e) => setText(e.target.value)} />
        <Button type="submit" size="sm" variant="outline" disabled={save.pending || !text.trim()}>
          {t("groupEditor.addName")}
        </Button>
      </form>
      <ErrorLine error={save.error} />
    </Section>
  );
}

function Texts({ group, g, languages, onError }: { group: GroupDetail; g: UseGroup; languages: readonly string[]; onError?: ContentKitErrorHandler }) {
  const { t } = useMessages();
  const id = useId();
  const languageName = useLanguageName();
  const offered = [...new Set([...languages, ...group.text.map((x) => x.language)])];
  const [form, setForm, settle] = useSeeded<Record<string, Text>>(
    () => Object.fromEntries(offered.map((l) => [l, group.text.find((x) => x.language === l) ?? { language: l }])),
    group.revision,
  );
  const [saved, setSaved] = useState(false);
  const save = useAction("group.save", onError);
  const set = (l: string, patch: Partial<Text>) => {
    setSaved(false);
    setForm((f) => ({ ...f, [l]: { ...f[l]!, ...patch } }));
  };
  return (
    <Section title={t("groupEditor.text")} ckui="group-text">
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
              {t("groupEditor.descriptionIn", { language: languageName(l) })}
            </Label>
            <Textarea
              id={`${id}-${l}-d`}
              lang={l}
              rows={3}
              placeholder={t("groupEditor.description")}
              value={form[l]?.description ?? ""}
              onChange={(e) => set(l, { description: e.target.value })}
            />
            <Label htmlFor={`${id}-${l}-s`} className="sr-only">
              {t("groupEditor.sortKeyIn", { language: languageName(l) })}
            </Label>
            <Input
              id={`${id}-${l}-s`}
              className="h-8"
              lang={l}
              placeholder={t("groupEditor.sortKey")}
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
              {t("groupEditor.saved")}
            </span>
          )}
        </div>
      </form>
    </Section>
  );
}

function Images({ group, spec, onError, client, management }: { management?: boolean; group: GroupDetail; spec: GroupKind; onError?: ContentKitErrorHandler; client?: ContentKitClient }) {
  const { t } = useMessages();
  const c = group.cover;
  const mode = c?.image ? "image" : c?.content_id && !c.auto ? "content" : "auto";
  return (
    <Section title={t(spec.owner === "user" ? "groupEditor.images" : "groupEditor.cover")} ckui="group-cover-editor">
      <p className="text-muted-foreground" data-ckui="cover-mode">
        {t(mode === "image" ? "groupEditor.coverImage" : mode === "content" ? "groupEditor.coverContent" : "groupEditor.coverAuto")}
      </p>
      <GroupImageEditor management={management} group={group} slot="cover" client={client} onError={onError} />
      {spec.owner === "user" && <GroupImageEditor management={management} group={group} slot="avatar" client={client} onError={onError} />}
    </Section>
  );
}

function Membership({ group, onError, client }: { group: GroupDetail; onError?: ContentKitErrorHandler; client?: ContentKitClient }) {
  const { t } = useMessages();
  const ck = useContentKitClient(client);
  const offer = useAction("offer.save", onError);
  return (
    <Section title={t("groupEditor.membershipPrice")} ckui="group-offer">
      <OfferFields
        key={group.offer?.revision}
        offer={group.offer}
        pending={offer.pending}
        onSell={(terms) => void offer.run(() => ck.offers.sellGroup(group.id, terms))}
        onWithdraw={() => void offer.run(() => ck.offers.withdrawGroup(group.id))}
      />
      <ErrorLine error={offer.error} />
    </Section>
  );
}

const RELATION = /^[a-z][a-z0-9_]*$/;

function Edges({ group, g, kinds, labels, onError, client, management }: { management?: boolean; group: GroupDetail; g: UseGroup; kinds: ContentModel | null; labels?: Labels; onError?: ContentKitErrorHandler; client?: ContentKitClient }) {
  const { t } = useMessages();
  const id = useId();
  const save = useAction("group.save", onError);
  const partners = useGroupNames(
    group.edges.map((e) => (e.from === group.id ? e.to : e.from)),
    { client, management },
  );
  const kindsList = Object.keys(kinds?.entity_kinds ?? {}).sort();
  const [relation, setRelation] = useState("");
  const [kind, setKind] = useState(kindsList[0] ?? group.kind);
  const out = group.edges.filter((e) => e.from === group.id);
  const incoming = group.edges.filter((e) => e.to === group.id);
  const nameOf = (gid: string) => {
    const x = partners.get(gid);
    return x ? x.name || x.slug : gid;
  };
  const valid = RELATION.test(relation.trim());
  return (
    <Section title={t("groupEditor.edges")} ckui="group-edges">
      {out.length + incoming.length ? (
        <ul className="grid gap-1.5">
          {out.map((e) => (
            <li key={`${e.relation}:${e.to}`} className="flex flex-wrap items-center gap-2" data-ckui="group-edge" data-relation={e.relation}>
              <Badge variant="secondary">{labelOf(labels, e.relation)}</Badge>
              <span>{nameOf(e.to)}</span>
              <Button size="xs" variant="ghost" disabled={save.pending} aria-label={t("groupEditor.removeEdge", { name: nameOf(e.to) })} onClick={() => void save.run(() => g.edges("remove", [{ relation: e.relation, to: e.to }]))}>
                {t("common.remove")}
              </Button>
            </li>
          ))}
          {incoming.map((e) => (
            <li key={`in:${e.relation}:${e.from}`} className="flex flex-wrap items-center gap-2 text-muted-foreground" data-ckui="group-edge" data-relation={`inverse:${e.relation}`}>
              <span className="text-foreground">{nameOf(e.from)}</span>
              <Badge variant="outline">{labelOf(labels, e.relation)}</Badge>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-muted-foreground">{t("groupEditor.noEdges")}</p>
      )}
      <div className="flex flex-wrap items-end gap-2">
        <div className="grid gap-1">
          <Label htmlFor={`${id}-rel`} className="text-xs text-muted-foreground">
            {t("groupEditor.relation")}
          </Label>
          <Input id={`${id}-rel`} className="h-8 w-40" value={relation} placeholder="member_of" aria-invalid={!!relation && !valid} onChange={(e) => setRelation(e.target.value)} />
        </div>
        {kindsList.length > 1 && (
          <Select value={kind} onValueChange={(v) => setKind(String(v))}>
            <SelectTrigger size="sm" aria-label={t("groupEditor.edgeKind")}>
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
          <GroupPicker
            management={management}
            key={kind}
            kind={kind}
            label={t("groupEditor.edgeTarget")}
            exclude={[group.id]}
            client={client}
            onPick={(to) => void save.run(() => g.edges("add", [{ relation: relation.trim(), to: to.id }])).then((d) => d && setRelation(""))}
          />
        )}
      </div>
      <ErrorLine error={save.error} />
    </Section>
  );
}
