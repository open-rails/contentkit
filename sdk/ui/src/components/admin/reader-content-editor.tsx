import { StarIcon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import { parseAttributes } from "../../client/group-fields.js";
import { imageRefs } from "../../client/contents.js";
import { toContentKitError } from "../../client/errors.js";
import type { CatalogText, Content, ContentModel, FileInfo, GroupCard, Listing, Rule } from "../../client/generated/wire.js";
import { stem } from "../../client/media/client.js";
import { isProcessing } from "../../client/media/windows.js";
import { useMessages } from "../../i18n/context.js";
import { useContent } from "../../react/contents.js";
import { useErrorReporter, useGuarded } from "../../react/context.js";
import { useMediaFolder } from "../../react/folder.js";
import { useContentGroups } from "../../react/groups.js";
import { usePageReview } from "../../react/page-review.js";
import { useContentScope } from "../../react/use-resource.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Textarea } from "#ckui/ui/textarea";
import { AttributeFields, formOf } from "../groups/fields.js";
import { EntityChips } from "../groups/entity-chips.js";
import { labelOf } from "../groups/parts.js";
import { MediaFolderEditor } from "../media-folder-editor.js";
import { useDraft } from "../../react/use-draft.js";
import { RuleFields, cleanRule, complete, VersionsEditor, versionLabel } from "../content/editors.js";
import { OfferFields } from "../content/offer-fields.js";
import { ErrorLine, contentError, useAction } from "../social/parts.js";
import { localInput } from "../social/bans.js";
import { EditorSaveContext, readerMediaReady, useEditorSaveCoordinator } from "../content/editor-save.js";
import { ReaderManifestEditor } from "./reader-manifest.js";
import { PageReview } from "./page-review.js";
import { LoadFailed, Rows, useConfirm } from "./shell.js";
import { contentStatus, type ContentEditorProps } from "./content-manager.js";

const blank = (): CatalogText => ({ title: "", slug: "", description: "", body: "" });
const editableText = (item: Content | null, language: string): CatalogText =>
  item?.text?.[language] ??
  (item?.language === language ? { title: item.title, slug: item.slug, description: item.description, body: item.body } : blank());
const fileTitle = (name: string) =>
  name
    .replace(/\.[^.]+$/, "")
    .replace(/[_-]+/g, " ")
    .trim()
    .slice(0, 300);
const textOf = (text: CatalogText): CatalogText => ({
  ...text,
  title: text.title.trim(),
  description: text.description?.trim(),
});
class ValidationError extends Error {}

// Without a visible title, label names the section for assistive technology.
function Panel({ title, label, children, className = "" }: { title?: ReactNode; label?: string; children: ReactNode; className?: string }) {
  return (
    <section aria-label={title ? undefined : label} className={`grid min-w-0 content-start gap-4 rounded-xl border border-border bg-card p-4 ${className}`}>
      {title && <h2 className="text-base font-semibold">{title}</h2>}
      {children}
    </section>
  );
}

/** A reader work, its files and associations are one editing session. */
export function ReaderContentEditor(p: ContentEditorProps & { model: ContentModel }) {
  const { client, scope } = useContentScope(p.client);
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const guarded = useGuarded();
  const report = useErrorReporter(p.onError);
  const model = p.model,
    spec = model.kinds[p.kind]!;
  const offered = p.languages?.map((l) => (typeof l === "string" ? { value: l, label: l } : l));
  const languages = offered?.length ? offered : model.languages.map((l) => ({ value: l, label: l }));
  const languageKey = languages.map((l) => l.value).join("|");
  const [language, setLanguage] = useState(languages[0]?.value ?? "en");
  const [chosen, setChosen] = useState("");
  const ed = useContent(p.id, p.kind, language, { client, version: chosen || undefined });
  const [created, setCreated] = useState<Content | null>(null);
  const item = ed.content ?? created;
  const owned = useRef({ item, scope, alive: true });
  owned.current.item = item;
  useEffect(() => {
    const owner = owned.current;
    owner.alive = true;
    return () => {
      owner.alive = false;
    };
  }, []);
  const current = () => owned.current.alive && owned.current.scope === scope;
  const creating = useRef<Promise<Content> | null>(null);
  const [allocating, setAllocating] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>();
  const [notice, setNotice] = useState("");
  const publication = useDraft(useMemo(() => ({ at: item?.publish_at ? localInput(new Date(item.publish_at)) : "" }), [item?.publish_at]));
  const publishAt = publication.value.at;
  const setPublishAt = (at: string) => publication.edit({ at });
  const [versionsOpened, setVersionsOpened] = useState(false);
  const [toolsOpened, setToolsOpened] = useState<string[]>([]);
  const [toolsExpanded, setToolsExpanded] = useState(false);
  const [reviewOpened, setReviewOpened] = useState(false);
  const versions = useMemo(() => [...(item?.versions ?? [])].sort((a, b) => a.position - b.position), [item?.versions]);
  const version = versions.find((v) => v.id === chosen) ?? versions[0];
  useEffect(() => {
    if (toolsExpanded && version) setToolsOpened((ids) => (ids.includes(version.id) ? ids : [...ids, version.id]));
  }, [toolsExpanded, version]);
  const membership = useContentGroups(item?.id, { language, client });
  const names = useMemo(() => new Map(membership.entities), [membership.entities]);
  const [picked, setPicked] = useState<ReadonlyMap<string, GroupCard>>(new Map());
  const cards = useMemo(() => new Map([...picked, ...names]), [names, picked]);
  const groupKinds = useMemo(() => spec.entities ?? [], [spec.entities]);
  const texts = useDraft(useMemo(() => Object.fromEntries(languageKey.split("|").map((l) => [l, editableText(item, l)])), [item, languageKey]));
  const details = useDraft(
    useMemo(
      () => ({
        listing: item?.listing ?? ("listed" as Listing),
        comments: item?.comments ?? "open",
        attributes: formOf(spec.attributes ?? {}, item?.attributes ?? {}),
        access: item?.access ?? ({ level: "public" } as Rule),
      }),
      [item?.listing, item?.comments, item?.attributes, item?.access, spec.attributes],
    ),
  );
  const associations = useDraft(
    useMemo(
      () => ({ memberships: membership.memberships.filter((a) => groupKinds.includes(membership.entities.get(a.group_id ?? "")?.kind ?? "")) }),
      [membership.memberships, membership.entities, groupKinds],
    ),
  );
  const coordinator = useEditorSaveCoordinator();
  const latest = useRef({ texts, details, associations, language });
  latest.current = { texts, details, associations, language };
  const text = texts.value[language] ?? blank();
  const shownBody = (text.body ?? "").replace(imageRefs(), (ref, name: string) => ed.imageSrc(name) ?? ref);
  const editText = (patch: Partial<CatalogText>) => texts.edit((all) => ({ ...all, [language]: { ...(all[language] ?? blank()), ...patch } }));
  const fail = (e: unknown) => {
    if (!current()) return;
    setError(e instanceof ValidationError ? e.message : e);
    if (!(e instanceof ValidationError)) report(toContentKitError(e), "content.save");
  };
  const ensure = async (suggested?: string): Promise<Content> => {
    if (owned.current.item) return owned.current.item;
    if (creating.current) return creating.current;
    setAllocating(true);
    const l = latest.current.language;
    const before = latest.current.texts.get()[l] ?? blank();
    const title = before.title.trim() || fileTitle(suggested ?? "") || t("readerEditor.untitled");
    if (!before.title.trim()) latest.current.texts.edit((all) => ({ ...all, [l]: { ...(all[l] ?? blank()), title } }));
    const input = { kind: p.kind, text: { [l]: { ...before, title } }, owner_group_id: p.ownerGroup };
    creating.current = ed
      .create(input)
      .then((content) => {
        owned.current.item = content;
        if (current()) {
          setCreated(content);
          p.onCreated?.(content);
        }
        return content;
      })
      .finally(() => {
        creating.current = null;
        if (current()) setAllocating(false);
      });
    return creating.current;
  };
  const ref = useMemo(() => (item && version ? { kind: p.kind, id: item.id, version: version.id } : null), [p.kind, item, version]);
  const cover = useAction("content.save", p.onError);
  const offer = useAction("offer.save", p.onError);
  const actions = useAction("content.save", p.onError);
  const folder = useMediaFolder(ref, { commit: version?.live ? "manual" : "auto", client, onError: p.onError });
  const folderRef = useRef(folder);
  folderRef.current = folder;
  const files = folder.read.read?.files ?? [];
  const review = usePageReview(item?.id, version?.id, { client });
  const reloadReview = review.reload;
  // A ZIP's pages appear after its commit. Media polling alone does not
  // invalidate the separate review count when extraction changes the manifest.
  const mediaReviewKey = ref && folder.read.read ? JSON.stringify([ref.id, ref.version,
    files.filter((file) => file.upload).map((file) => [file.path, file.size, file.staged, file.pending, file.failed]),
  ]) : null;
  useEffect(() => {
    if (mediaReviewKey) reloadReview();
  }, [mediaReviewKey, reloadReview]);
  const mediaFailed = !!folder.error || folder.queue.items.some((q) => q.status === "failed") || files.some((f) => f.failed);
  const processing = files.some(isProcessing);
  const selectedPages = review.review?.selected ?? 0;
  const filesPending = folder.busy || allocating || processing || folder.read.loading || review.loading || cover.pending;
  const publishingBlocked =
    filesPending ||
    mediaFailed ||
    !!folder.read.error ||
    !!review.error ||
    selectedPages === 0 ||
    coordinator.pending ||
    offer.pending;
  const add = (input: Iterable<File>) => {
    const added = [...input];
    if (!added.length) return;
    folder.add(added);
    void ensure(added.find((f) => /\.zip$/i.test(f.name))?.name ?? added[0]?.name).catch(fail);
  };
  const dirty =
    publication.dirty.length > 0 ||
    texts.dirty.length > 0 ||
    details.dirty.length > 0 ||
    associations.dirty.length > 0 ||
    coordinator.dirty;
  useEffect(() => {
    if (!dirty && !folder.busy) return;
    const ask = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    addEventListener("beforeunload", ask);
    return () => removeEventListener("beforeunload", ask);
  }, [dirty, folder.busy]);
  const save = async (publish: boolean) => {
    if (saving) return;
    setError(undefined);
    setNotice("");
    const state = latest.current;
    const active = state.texts.get()[state.language];
    if (!active?.title.trim()) return setError(t("contentManager.titleRequired"));
    const problem = coordinator.problem();
    if (problem) return setError(t("readerEditor.fixSection", { section: problem }));
    if (!complete(state.details.get().access) || (state.details.get().access.level === "tier" && !state.details.get().access.tiers?.length))
      return setError(t("readerEditor.accessRequired"));
    let attributes: Record<string, unknown>;
    try {
      attributes = parseAttributes(spec.attributes ?? {}, state.details.get().attributes);
    } catch (e) {
      return fail(e);
    }
    if (publish && publishingBlocked) return setError(t("readerEditor.filesNotReady"));
    setSaving(true);
    const settleTexts = state.texts.checkpoint(),
      settleDetails = state.details.checkpoint(),
      settleAssociations = state.associations.checkpoint();
    const changedText = state.texts.dirty.slice();
    const snapshotTexts = { ...state.texts.get() };
    const snapshotDetails = state.details.get();
    const snapshotAssociations = state.associations.get().memberships.slice();
    let cleanAssociations = true;
    try {
      let content = await ensure();
      if (!current()) return;
      if (membership.error) throw membership.error;
      await guarded(async () => {
        for (const key of changedText) {
          const l = String(key);
          const next = snapshotTexts[l];
          if (!next?.title.trim()) throw new ValidationError(t("contentManager.titleRequired"));
          content = await client.contents.putText(content.id, l, textOf(next));
          if (!current()) return;
        }
        // Text writes advance the content revision. Fetch the current record
        // before the revision-checked details write, never reuse a stale one.
        content = await client.contents.get(content.id, language);
        if (!current()) return;
        content = await client.contents.update(content.id, {
          listing: snapshotDetails.listing,
          comments: snapshotDetails.comments,
          attributes,
          access: cleanRule(snapshotDetails.access),
          revision: content.revision,
        });
        if (!current()) return;
        if (state.associations.dirty.length) {
          const targetVersion = content.versions.find((v) => v.id === version?.id) ?? content.versions[0];
          const memberships = snapshotAssociations.map((a) => {
            const kind = cards.get(a.group_id ?? "")?.kind;
            return kind && model.entity_kinds[kind]?.version_level && !a.version_id ? { ...a, version_id: targetVersion?.id } : a;
          });
          const saved = await client.entities.setOfContent(content.id, { kinds: [...groupKinds], memberships }, language);
          cleanAssociations = settleAssociations({
            memberships: saved.memberships.filter((a) => groupKinds.includes(saved.entities.find((g) => g.id === a.group_id)?.kind ?? "")),
          });
        }
      });
      if (!current()) return;
      // A live edition queues changes for one commit; a draft adds files as
      // uploads finish. Saving metadata never interrupts a running upload.
      const currentFolder = folderRef.current;
      if (!currentFolder.busy && (currentFolder.pending.length || currentFolder.queue.items.some((q) => q.status === "uploaded"))) await currentFolder.commit();
      const blocked = await coordinator.save(current);
      if (blocked) throw new ValidationError(t("readerEditor.fixSection", { section: blocked }));
      if (!current()) return;
      const cleanTexts = settleTexts(Object.fromEntries(languages.map((l) => [l.value, content.text?.[l.value] ?? snapshotTexts[l.value] ?? blank()])));
      const cleanDetails = settleDetails({
        listing: content.listing,
        comments: content.comments,
        attributes: formOf(spec.attributes ?? {}, content.attributes),
        access: content.access,
      });
      if (publish) {
        if (!cleanTexts || !cleanDetails || !cleanAssociations) throw new ValidationError(t("readerEditor.changedDuringSave"));
        const fresh = folderRef.current;
        if (!ref || fresh.busy || fresh.error || fresh.queue.items.some((q) => q.status === "failed") || !(await readerMediaReady(client, ref)))
          throw new ValidationError(t("readerEditor.filesNotReady"));
        const edition = version ? await client.contents.version(content.id, version.id, language) : null;
        if (edition?.state !== "published") throw new ValidationError(t("readerEditor.versionDraft"));
        if (!current()) return;
        const settlePublication = publication.checkpoint();
        const published = await guarded(() => client.contents.publish(content.id, publishAt ? new Date(publishAt).toISOString() : undefined));
        settlePublication({ at: published.publish_at ? localInput(new Date(published.publish_at)) : "" });
        setNotice(t("contentManager.publishedNotice"));
      } else setNotice(t("pollEditor.saved"));
    } catch (e) {
      fail(e);
    } finally {
      if (current()) setSaving(false);
    }
  };

  const customCover = async (file: File) => {
    const content = await ensure(file.name);
    const v = content.versions.find((v) => v.id === version?.id) ?? content.versions[0];
    if (!v || !current()) return;
    const target = { kind: p.kind, id: content.id, version: v.id };
    const up = await client.media.uploadNamed(target, file, { path: await client.media.presetPath(p.kind, "cover", file) });
    if (current()) await client.contents.setCover(content.id, v.id, up.name);
  };
  const selectCover = async (file: FileInfo) => {
    if (!ref || !item || !version) return;
    const path = await client.media.presetPath(p.kind, "cover", new File([], file.path.split("/").at(-1) ?? "cover", { type: file.type }));
    await client.media.commit(ref, [{ op: "copy", from: { id: item.id, version: version.id, path: file.path }, to: path }]);
    if (current()) await client.contents.setCover(item.id, version.id, stem(path).split("/").at(-1)!);
  };
  const { confirm, dialog } = useConfirm();
  const remove = async () => {
    if (
      !item ||
      !(await confirm({
        title: t("contentManager.deleteTitle", { title: item.title }),
        description: t("contentManager.deleteBody"),
        confirm: t("contentManager.delete"),
      }))
    )
      return;
    await actions.run(async () => {
      folder.discard();
      await client.contents.remove(item.id);
      if (current()) p.onDeleted?.(item);
    });
  };
  if (p.id && !item) return ed.error ? <LoadFailed error={ed.error} message={t("contentManager.loadFailed")} retry={ed.reload} /> : <Rows />;
  const status = item ? contentStatus(item) : "draft";
  const primaryVersion = version?.id;
  const changeDetails = (patch: Partial<typeof details.value>) => details.edit((d) => ({ ...d, ...patch }));
  const versionChoices = versions.map((v) => ({ id: v.id, label: versionLabel(v, (l) => languages.find((x) => x.value === l)?.label ?? l, p.labels) }));
  return (
    <EditorSaveContext.Provider value={coordinator.context}>
      <div className="grid gap-5 text-base" data-ckui="reader-content-editor">
        <div className="sticky top-0 z-10 flex flex-wrap items-center gap-3 rounded-xl border border-border bg-background/95 p-4 shadow-sm">
          <div className="min-w-0 flex-1">
            <h1 className="sr-only">{text.title || t("readerEditor.new")}</h1>
            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              <Badge variant={item?.live ? "default" : "outline"}>{t(`contentManager.${status}`)}</Badge>
              {version && <span>{t("contentEditor.pageCount", { count: review.review ? selectedPages : version.pages ?? 0 })}</span>}
              {allocating && <span role="status">{t("common.saving")}</span>}
            </div>
          </div>
          {item?.live && p.contentHref?.(item) && (
            <a href={p.contentHref(item)} className="text-sm underline">
              {t("contentManager.view")}
            </a>
          )}
          <Button disabled={saving || allocating} onClick={() => void save(false)}>
            {saving ? t("common.saving") : t(item?.live ? "common.save" : "readerEditor.saveDraft")}
          </Button>
          {!item?.live && (
            <Button variant="outline" disabled={saving || publishingBlocked} onClick={() => void save(true)}>
              {publishAt ? t("readerEditor.schedule") : t("contentManager.publish")}
            </Button>
          )}
        </div>
        {error != null && <ErrorLine>{typeof error === "string" ? error : contentError(m, toContentKitError(error))}</ErrorLine>}
        {notice && (
          <p role="status" className="text-sm text-muted-foreground">
            {notice}
          </p>
        )}
        <fieldset disabled={saving} className="grid min-w-0 items-start gap-5 xl:grid-cols-[minmax(0,1fr)_minmax(18rem,24rem)]">
          <div className="grid min-w-0 gap-5">
            <Panel label={t("readerEditor.details")}>
              <div className="grid gap-4 sm:grid-cols-[minmax(0,1fr)_10rem]">
                <div className="grid gap-1.5">
                  <Label htmlFor={`${ids}-title`}>{t("contentManager.itemTitle")}</Label>
                  <Input id={`${ids}-title`} required maxLength={300} value={text.title} onChange={(e) => editText({ title: e.target.value })} />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor={`${ids}-language`}>{t("contentManager.language")}</Label>
                  <Select value={language} onValueChange={(l) => l && setLanguage(l)}>
                    <SelectTrigger id={`${ids}-language`}>
                      <SelectValue>{languages.find((l) => l.value === language)?.label ?? language}</SelectValue>
                    </SelectTrigger>
                    <SelectContent>
                      {languages.map((l) => (
                        <SelectItem key={l.value} value={l.value}>
                          {l.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor={`${ids}-description`}>{t("contentManager.description")}</Label>
                <Textarea id={`${ids}-description`} rows={3} value={text.description ?? ""} onChange={(e) => editText({ description: e.target.value })} />
              </div>
              <AttributeFields
                declared={spec.attributes ?? {}}
                form={details.value.attributes}
                onChange={(name, value) => changeDetails({ attributes: { ...details.value.attributes, [name]: value } })}
                labels={p.labels}
              />
              <details className="grid gap-3">
                <summary className="cursor-pointer text-sm text-muted-foreground">{t("readerEditor.additionalText")}</summary>
                <div className="mt-3 grid gap-3">
                  <Label htmlFor={`${ids}-body`}>{t("contentManager.body")}</Label>
                  {p.renderEditor ? (
                    p.renderEditor({
                      value: shownBody,
                      onChange: (html) => editText({ body: ed.storedBody(html) }),
                      uploadImage: item ? (file) => ed.uploadImage(file) : undefined,
                      disabled: saving,
                      "aria-label": t("contentManager.body"),
                    })
                  ) : (
                    <Textarea id={`${ids}-body`} value={text.body ?? ""} onChange={(e) => editText({ body: e.target.value })} />
                  )}
                </div>
              </details>
            </Panel>
            <Panel title={t("readerEditor.files")}>
              {versions.length > 1 && (
                <Select value={primaryVersion ?? ""} onValueChange={(v) => v && setChosen(v)} disabled={folder.busy || folder.pending.length > 0}>
                  <SelectTrigger aria-label={t("contentEditor.versions")}>
                    <SelectValue>{versionChoices.find((v) => v.id === primaryVersion)?.label}</SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    {versionChoices.map((v) => (
                      <SelectItem key={v.id} value={v.id}>
                        {v.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
              <p className="text-sm text-muted-foreground">{t("readerEditor.fileHelp")}</p>
              <MediaFolderEditor
                folder={{ ...folder, add }}
                label={false}
                layout="grid"
                disabled={saving}
                inputLabel={t("readerEditor.files")}
                rowActions={(file) =>
                  file.type.startsWith("image/") && folder.groups.some((g) => g.rule?.page && g.files.some((f) => f.path === file.path)) ? (
                    <Button
                      size="icon-sm"
                      variant="ghost"
                      title={t("readerEditor.useCover")}
                      aria-label={t("readerEditor.useCoverFor", { name: file.path.split("/").at(-1) ?? file.path })}
                      disabled={cover.pending || saving}
                      onClick={() => void cover.run(() => selectCover(file))}
                    >
                      <HugeiconsIcon icon={StarIcon} />
                    </Button>
                  ) : null
                }
              />
              {folder.read.error && <LoadFailed error={folder.read.error} message={t("contentManager.loadFailed")} retry={folder.read.reload} />}
              {review.error && <LoadFailed error={review.error} message={t("pageReview.loadFailed")} retry={review.reload} />}
              {(mediaFailed || filesPending || (!review.error && !folder.read.error && selectedPages === 0)) && (
                <p role="status" className="text-sm text-muted-foreground">
                  {mediaFailed ? t("readerEditor.filesFailed") : filesPending ? t("readerEditor.processing") : t("readerEditor.addPages")}
                </p>
              )}
              {item && version && (
                <details onToggle={(e) => e.currentTarget.open && setReviewOpened(true)}>
                  <summary className="cursor-pointer font-medium">{t("pageReview.title")}</summary>
                  {reviewOpened && <PageReview key={version.id} content={item.id} version={version.id} client={client} onError={p.onError} />}
                </details>
              )}
            </Panel>
            {item && (
              <details className="rounded-xl border border-border bg-card p-4" onToggle={(e) => e.currentTarget.open && setVersionsOpened(true)}>
                <summary className="cursor-pointer font-medium">{t("readerEditor.versions")}</summary>
                {versionsOpened && (
                  <VersionsEditor content={item} media={false} activeVersion={primaryVersion} client={client} labels={p.labels} onError={p.onError} />
                )}
              </details>
            )}
            {item && version && (
              <details className="rounded-xl border border-border bg-card p-4" onToggle={(e) => setToolsExpanded(e.currentTarget.open)}>
                <summary className="cursor-pointer font-medium">{t("readerEditor.readerTools")}</summary>
                {toolsOpened
                  .filter((id) => versions.some((v) => v.id === id))
                  .map((id) => (
                    <div key={id} hidden={id !== version.id}>
                      <ReaderManifestEditor content={item.id} version={id} client={client} onError={p.onError} />
                    </div>
                  ))}
              </details>
            )}
          </div>
          <aside className="grid min-w-0 gap-5">
            <Panel title={t("readerEditor.entities")}>
              {membership.error && <ErrorLine error={membership.error} />}
              {groupKinds.map((kind) => {
                const rows = associations.value.memberships.map((a, index) => ({ a, index })).filter(({ a }) => cards.get(a.group_id ?? "")?.kind === kind);
                return (
                  <EntityChips
                    key={kind}
                    kind={kind}
                    label={labelOf(p.labels, kind)}
                    memberships={rows.map(({ a }) => a)}
                    cards={cards}
                    spec={model.entity_kinds[kind] ?? {}}
                    labels={p.labels}
                    versions={versionChoices}
                    versionLabel={(id) => versionChoices.find((v) => v.id === id)?.label ?? id}
                    language={language}
                    management={p.management}
                    client={client}
                    disabled={!!item && membership.loading}
                    onChange={(i, patch) => associations.edit((d) => ({ memberships: d.memberships.map((x, j) => (j === rows[i]!.index ? { ...x, ...patch } : x)) }))}
                    onRemove={(i) => associations.edit((d) => ({ memberships: d.memberships.filter((_, j) => j !== rows[i]!.index) }))}
                    onAdd={(card) => {
                      setPicked((all) => new Map(all).set(card.id, card));
                      associations.edit((d) => ({
                        memberships: [...d.memberships, { group_id: card.id, ...(model.entity_kinds[kind]?.version_level && version ? { version_id: version.id } : {}) }],
                      }));
                    }}
                  />
                );
              })}
            </Panel>
            <Panel title={t("contentManager.cover")}>
              {version?.cover?.url ? (
                <img src={version.cover.url} alt="" className="mx-auto max-h-64 rounded-lg object-contain" />
              ) : (
                <p className="text-sm text-muted-foreground">{t("readerEditor.firstPageCover")}</p>
              )}
              <label className="inline-flex cursor-pointer justify-center rounded-md border border-border px-3 py-2 text-sm font-medium hover:bg-muted">
                {t("contentManager.addCover")}
                <input
                  type="file"
                  accept="image/*"
                  className="sr-only"
                  aria-label={t("contentManager.cover")}
                  disabled={cover.pending || saving}
                  onChange={(e) => {
                    const file = e.target.files?.[0];
                    e.target.value = "";
                    if (file) void cover.run(() => customCover(file));
                  }}
                />
              </label>
              <ErrorLine error={cover.error} />
            </Panel>
            <Panel title={t("contentEditor.accessTitle")}>
              <RuleFields
                value={details.value.access}
                onChange={(access) => changeDetails({ access })}
                model={model}
                kind={p.kind}
                owner={item?.owner_group_id ?? p.ownerGroup}
                labels={p.labels}
                client={client}
              />
              {item && (details.value.access.level === "ppv" || details.value.access.level === "group_ppv" || item.offer) && (
                <OfferFields
                  offer={item.offer}
                  pending={offer.pending}
                  onSell={(terms) => offer.run(() => client.offers.sell({ content: item.id }, terms))}
                  onWithdraw={() => void offer.run(() => client.offers.withdraw({ content: item.id }))}
                />
              )}
              <ErrorLine error={offer.error} />
            </Panel>
            <Panel title={t("readerEditor.publication")}>
              {!item?.live && (
                <div className="grid gap-1.5">
                  <Label htmlFor={`${ids}-publish`}>{t("contentManager.publishAt")}</Label>
                  <Input id={`${ids}-publish`} type="datetime-local" value={publishAt} onChange={(e) => setPublishAt(e.target.value)} />
                </div>
              )}
              <Label htmlFor={`${ids}-listing`}>{t("contentEditor.listing")}</Label>
              <Select value={details.value.listing} onValueChange={(listing) => listing && changeDetails({ listing: listing as Listing })}>
                <SelectTrigger id={`${ids}-listing`}>
                  <SelectValue>{t(`contentEditor.listing_${details.value.listing}`)}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {(["listed", "members", "unlisted"] as const).map((value) => (
                    <SelectItem key={value} value={value}>
                      {t(`contentEditor.listing_${value}`)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Label htmlFor={`${ids}-comments`}>{t("contentEditor.comments")}</Label>
              <Select value={details.value.comments} onValueChange={(comments) => comments && changeDetails({ comments })}>
                <SelectTrigger id={`${ids}-comments`}>
                  <SelectValue>{t(`contentEditor.comments_${details.value.comments as "open" | "closed" | "hidden"}`)}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {(["open", "closed", "hidden"] as const).map((value) => (
                    <SelectItem key={value} value={value}>
                      {t(`contentEditor.comments_${value}`)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {item?.state === "published" && (
                <Button variant="outline" disabled={actions.pending} onClick={() => void actions.run(() => client.contents.unpublish(item.id))}>
                  {t("contentManager.unpublish")}
                </Button>
              )}
              {item && (
                <Button variant="destructive" disabled={actions.pending} onClick={() => void remove()}>
                  {t("contentManager.delete")}
                </Button>
              )}
              <ErrorLine error={actions.error} />
            </Panel>
          </aside>
        </fieldset>
        {dialog}
      </div>
    </EditorSaveContext.Provider>
  );
}
