import { MediaReadinessNotice } from "../media-readiness-notice.js";
import { ImageAdd01Icon } from "@hugeicons/core-free-icons";
import { HugeiconsIcon } from "@hugeicons/react";
import { useCallback, useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import { parseAttributes } from "../../client/entity-fields.js";
import { toContentKitError } from "../../client/errors.js";
import type { CatalogText, Content, ContentModel, FileInfo, ForkBody, EntityCard, Membership, NewEntity, RefBody, Version, VersionText } from "../../client/generated/wire.js";
import { stem } from "../../client/media/client.js";
import { allowsEntityScope } from "../../client/entity-scopes.js";
import { ruleFor, uploadRules } from "../../client/media/rules.js";
import { isProcessing, pageRemoved } from "../../client/media/windows.js";
import { useLanguageName, useMessages } from "../../i18n/context.js";
import { useContent } from "../../react/contents.js";
import { useErrorReporter, useGuarded } from "../../react/context.js";
import { thumbnailOf, useMediaFolder } from "../../react/folder.js";
import { useReaderFileDraft } from "../../react/reader-file-draft.js";
import { DraftHistoryProvider, useDraftHistory } from "../../react/draft-history.js";
import { useContentEntities } from "../../react/entities.js";
import { useEditorCrop } from "../../react/crop.js";
import { usePageNotices } from "../../react/page-notices.js";
import { useContentScope } from "../../react/use-resource.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Textarea } from "#ckui/ui/textarea";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "#ckui/ui/dialog";
import { AttributeFields, formOf } from "../entities/fields.js";
import { EntityChips } from "../entities/entity-chips.js";
import { labelOf } from "../entities/parts.js";
import { ImageCropDialog } from "../image-crop-dialog.js";
import { MediaFolderEditor } from "../media-folder-editor.js";
import { useDraft } from "../../react/use-draft.js";
import { RuleFields, cleanRule, complete, VersionEditor, VersionStatus, SegmentsEditor, AddVersion, versionLabel, defaultAccessRule } from "../content/editors.js";
import { saveEditionAccess, selectedEdition } from "../../client/edition-access.js";
import { publicationOf } from "../../client/publication.js";
import { ErrorLine, contentError, formatDate, useAction } from "../social/parts.js";
import { localInput } from "../social/bans.js";
import { EditorSaveContext, useEditorSaveCoordinator } from "../content/editor-save.js";
import { ReaderManifestEditor } from "./reader-manifest.js";
import { isAudioRule } from "./page-picker.js";
import { PageNotices } from "./page-notices.js";
import { LoadFailed, NoAccess, Rows, useConfirm } from "./shell.js";
import { contentStatus, type ContentEditorProps } from "./content-manager.js";
import { fileTitle } from "./bulk-drafts.js";

const NO_REF: RefBody = { kind: "", id: "" };
const blank = (): CatalogText => ({ title: "", slug: "", description: "", body: "" });
const editableText = (item: Content | null, language: string): CatalogText =>
  item?.text?.[language] ??
  (item?.language === language ? { title: item.title, slug: item.slug, description: item.description, body: item.body } : blank());
const textOf = (text: CatalogText): CatalogText => ({
  ...text,
  title: text.title.trim(),
  description: text.description?.trim(),
});
class ValidationError extends Error {}

/** A copy's failure receipt (versions' fork_failed), naming the fork to try again. */
interface ForkFailure {
  from: string;
  playback?: boolean;
  segments?: boolean;
  reason: "destination" | "source" | "error";
  at: string;
}

/** A version's copy in its media metadata: the barrier while copying, the receipt once copied, or the last failure. */
function forkState(meta: Record<string, unknown> | undefined) {
  return { copyingFrom: (meta?.fork as { from: string } | undefined)?.from, copied: !!meta?.forked, failed: meta?.fork_failed as ForkFailure | undefined };
}

function CopyFailed({ failure, version, pending, onRetry }: { failure: ForkFailure; version: string; pending: boolean; onRetry: (failure: ForkFailure) => void }) {
  const { t } = useMessages();
  return (
    <div className="flex flex-wrap items-center gap-2">
      <ErrorLine>{t("readerEditor.copyFailedFrom", { version, reason: t(`readerEditor.copyFailure_${failure.reason}`) })}</ErrorLine>
      <Button size="sm" variant="outline" disabled={pending} onClick={() => onRetry(failure)}>{t("common.retry")}</Button>
    </div>
  );
}

// Without a visible title, label names the section for assistive technology.
function Panel({ title, label, children, className = "" }: { title?: ReactNode; label?: string; children: ReactNode; className?: string }) {
  return (
    <section aria-label={title ? undefined : label} className={`grid min-w-0 content-start gap-4 rounded-xl border border-border bg-muted/30 p-4 ${className}`}>
      {title && <h2 className="text-base font-semibold">{title}</h2>}
      {children}
    </section>
  );
}

/** A reader work, its files and associations are one editing session. */
export function ReaderContentEditor(p: ContentEditorProps & { model: ContentModel }) {
  return <DraftHistoryProvider><ReaderContentWorkspace {...p} /></DraftHistoryProvider>;
}

function ReaderContentWorkspace(p: ContentEditorProps & { model: ContentModel }) {
  const history = useDraftHistory()!;
  const { client, scope } = useContentScope(p.client);
  const m = useMessages();
  const { t } = m;
  const languageName = useLanguageName();
  const ids = useId();
  const guarded = useGuarded();
  const report = useErrorReporter(p.onError);
  const model = p.model,
    spec = model.kinds[p.kind]!;
  const originalAndVersion = spec.title_policy === "original_and_version";
  const offered = p.languages?.map((l) => (typeof l === "string" ? { value: l, label: l } : l));
  const languages = offered?.length ? offered : model.languages.map((l) => ({ value: l, label: l }));
  const languageKey = languages.map((l) => l.value).join("|");
  const [language, setLanguage] = useState(languages[0]?.value ?? "en");
  const [chosen, setChosen] = useState("");
  const ed = useContent(p.id, p.kind, language, { client });
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
  const [versionScheduleTarget, setVersionScheduleTarget] = useState<HTMLDivElement | null>(null);
  const [versionActionsTarget, setVersionActionsTarget] = useState<HTMLDivElement | null>(null);
  const [versionReaderTarget, setVersionReaderTarget] = useState<HTMLDivElement | null>(null);
  const [versionRestoreTarget, setVersionRestoreTarget] = useState<HTMLDivElement | null>(null);
  const [versionSchedule, setVersionSchedule] = useState("");
  const [stagedLanguage, setStagedLanguage] = useState<{ version: string; language: string } | null>(null);
  const [coverChoices, setCoverChoices] = useState(false);
  const [draftGeneration, setDraftGeneration] = useState(0);
  const publication = useDraft(useMemo(() => ({ at: item?.publish_at ? localInput(new Date(item.publish_at)) : "" }), [item?.publish_at]));
  const publishAt = publication.value.at;
  const [addingVersion, setAddingVersion] = useState(false);
  const [toolsExpanded, setToolsExpanded] = useState(false);
  // A page tile picked as the thumbnail's source, and the cover upload being cropped.
  const [page, setPage] = useState<FileInfo | null>(null);
  const [thumb, setThumb] = useState<{ path: string; aspect: string } | null>(null);
  const versions = useMemo(() => [...(item?.versions ?? [])].sort((a, b) => a.position - b.position), [item?.versions]);
  const selectedCode = p.versionCode ?? chosen;
  const version = selectedCode ? versions.find((v) => v.code === selectedCode) : versions.find((v) => v.state !== "deleted") ?? versions[0];
  const editionDeleted = version?.state === "deleted";
  const { id: contentID, versionCode, onVersionChange, onLeaveGuard } = p;
  const canonical = useRef("");
  useEffect(() => {
    if (!contentID || !item || !version || versionCode || !onVersionChange) return;
    const key = `${item.id}:${version.code}`;
    if (canonical.current === key) return;
    canonical.current = key;
    onVersionChange(version, { replace: true });
  }, [contentID, versionCode, onVersionChange, item, version]);
  const languageVersion = useRef("");
  useEffect(() => {
    if (!version || languageVersion.current === version.id) return;
    languageVersion.current = version.id;
    const first = version.languages.find((l) => languages.some((option) => option.value === l));
    if (first) setLanguage(first);
  }, [version, languages]);
  const membership = useContentEntities(item?.id, { language, client });
  const names = useMemo(() => new Map(membership.entities), [membership.entities]);
  const [picked, setPicked] = useState<ReadonlyMap<string, EntityCard>>(new Map());
  const cards = useMemo(() => new Map([...picked, ...names]), [names, picked]);
  const entityKinds = useMemo(() => spec.entities ?? [], [spec.entities]);
  const sharedKinds = useMemo(() => entityKinds.filter((kind) => allowsEntityScope(model.entity_kinds[kind], "content")), [entityKinds, model.entity_kinds]);
  const versionKinds = useMemo(() => entityKinds.filter((kind) => allowsEntityScope(model.entity_kinds[kind], "version")), [entityKinds, model.entity_kinds]);
  const texts = useDraft(useMemo(() => Object.fromEntries(languageKey.split("|").map((l) => [l, editableText(item, l)])), [item, languageKey]));
  const editionLanguage = stagedLanguage && stagedLanguage.version === version?.id ? stagedLanguage.language : version?.languages[0] ?? language;
  const versionID = version?.id;
  const onVersionLanguagesChange = useCallback((languages: readonly string[]) => {
    if (versionID) setStagedLanguage({ version: versionID, language: languages[0] ?? language });
  }, [versionID, language]);
  const versionTexts = useDraft(useMemo<Record<string, VersionText>>(() => version?.text ?? {}, [version?.text]));
  const displayTitle = versionTexts.value[editionLanguage]?.title ?? item?.text?.[editionLanguage]?.title ?? (item?.language === editionLanguage ? item.title : "");
  const details = useDraft(
    useMemo(
      () => ({
        originalTitle: item?.original_title ?? "",
        comments: item?.comments ?? (!spec.comment_modes?.length || spec.comment_modes.includes("open") ? "open" : spec.comment_modes[0]!),
        attributes: formOf(spec.attributes ?? {}, item?.attributes ?? {}),
        access: version?.access ?? defaultAccessRule(model, p.kind),
      }),
      [item?.original_title, item?.comments, item?.attributes, version?.access, spec.attributes, spec.comment_modes, model, p.kind],
    ),
  );
  const associations = useDraft(
    useMemo(
      () => ({
        memberships: membership.memberships.filter((a) => !a.version_id && sharedKinds.includes(membership.entities.get(a.group_id ?? "")?.kind ?? "")),
        create: [] as NewEntity[],
      }),
      [membership.memberships, membership.entities, sharedKinds],
    ),
  );
  const versionAssociations = useDraft(
    useMemo(() => ({
      memberships: membership.memberships.filter((a) => a.version_id === version?.id && versionKinds.includes(membership.entities.get(a.group_id ?? "")?.kind ?? "")),
      create: [] as NewEntity[],
    }), [membership.memberships, membership.entities, version?.id, versionKinds]),
  );
  // A scoped kind's new names wait for exactly one linked or new scope entity.
  const scopeMissing = (kind: string, memberships: readonly Membership[], create: readonly NewEntity[]) => {
    const by = model.entity_kinds[kind]?.scoped_by;
    if (!by) return undefined;
    const linked = new Set(memberships.filter((a) => cards.get(a.group_id ?? "")?.kind === by).map((a) => a.group_id));
    return linked.size + create.filter((c) => c.kind === by).length === 1 ? undefined : labelOf(p.labels, by);
  };
  const coordinator = useEditorSaveCoordinator();
  const latest = useRef({ texts, details, associations, versionAssociations, language, versionTexts, editionLanguage });
  latest.current = { texts, details, associations, versionAssociations, language, versionTexts, editionLanguage };
  // Starting entities (e.g. a series' "new gallery") join a new item's draft once, when they resolve.
  const started = useRef(false);
  useEffect(() => {
    if (started.current || p.id || !p.entities?.length) return;
    started.current = true;
    const usable = p.entities.filter((g) => entityKinds.includes(g.kind));
    if (!usable.length) return;
    setPicked((all) => new Map([...all, ...usable.map((g) => [g.id, g] as const)]));
    const seed = (kinds: readonly string[], draft: typeof associations) => draft.edit((d) => ({
      ...d,
      memberships: [...d.memberships, ...usable.filter((g) => kinds.includes(g.kind) && !d.memberships.some((a) => a.group_id === g.id)).map((g) => ({ group_id: g.id }))],
    }));
    seed(sharedKinds, latest.current.associations);
    seed(versionKinds.filter((kind) => !sharedKinds.includes(kind)), latest.current.versionAssociations);
  }, [p.id, p.entities, entityKinds, sharedKinds, versionKinds]);
  const text = texts.value[language] ?? blank();
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
    const l = originalAndVersion ? latest.current.editionLanguage : latest.current.language;
    const before = latest.current.texts.get()[l] ?? blank();
    const title = (originalAndVersion ? latest.current.versionTexts.get()[l]?.title : before.title)?.trim() || fileTitle(suggested ?? "") || t("readerEditor.untitled");
    if (originalAndVersion && !latest.current.versionTexts.get()[l]?.title?.trim()) latest.current.versionTexts.edit((all) => ({ ...all, [l]: { ...all[l], title } }));
    if (!before.title.trim()) latest.current.texts.edit((all) => ({ ...all, [l]: { ...(all[l] ?? blank()), title } }));
    const input = {
      kind: p.kind,
      text: { [l]: { ...before, title } },
      owner_group_id: spec.owner_entity ? undefined : p.ownerEntity,
      access: cleanRule(latest.current.details.get().access),
      ...(spec.comment_modes?.length ? { comments: latest.current.details.get().comments } : {}),
    };
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
  const addVersion = useAction("content.save", p.onError);
  const fork = useAction("content.save", p.onError);
  // A version added from a sibling, followed until its copy (a job) ends, also
  // once the dialog is closed. retried is the failure receipt a retry replaces.
  const [copy, setCopy] = useState<{ version: Version; body: ForkBody; accepted?: boolean; failed?: ForkFailure; retried?: string } | null>(null);
  const actions = useAction("content.save", p.onError);
  const sourceFolder = useMediaFolder(ref, { commit: version?.live || editionDeleted ? "manual" : "auto", client, onError: p.onError });
  const sourceFolderRef = useRef(sourceFolder);
  sourceFolderRef.current = sourceFolder;
  const fileDraft = useReaderFileDraft(sourceFolder, client);
  const folder = fileDraft.folder;
  const folderRef = useRef(folder);
  folderRef.current = folder;
  const files = folder.read.read?.files ?? [];
  const showVersion = (v: Version) => {
    ed.reload();
    if (p.onVersionChange) p.onVersionChange(v);
    else setChosen(v.code);
  };
  const openVersion = (v: Version) => {
    setCopy(null);
    setAddingVersion(false);
    showVersion(v);
  };
  // A copy ending in the dialog opens its version, or shows its failure there.
  // Ended after the dialog closed, the workspace shows the version's receipt.
  const endCopy = (v: Version, failed?: ForkFailure) => {
    if (!addingVersion) setCopy(null);
    else if (failed) setCopy((c) => (c?.version.id === v.id ? { ...c, failed } : c));
    else openVersion(v);
  };
  const endCopied = useRef(endCopy);
  endCopied.current = endCopy;
  const startCopy = (content: string, v: Version, body: ForkBody, retried?: string) => {
    setCopy({ version: v, body, retried });
    void fork.run(() => client.versions.fork(content, v.id, body)).then((ok) => ok && current() && setCopy((c) => (c?.version.id === v.id ? { ...c, accepted: true } : c)));
  };
  const copying = copy && !copy.failed && !fork.error ? copy : null;
  const copied = copying?.accepted ? copying.version : null;
  const retried = copying?.retried;
  const contentKey = item?.id;
  useEffect(() => {
    if (!copied || !contentKey) return;
    const ctrl = new AbortController();
    // The copy's receipt, or its failure's, in the new version's media metadata.
    const check = () => void client.media.read({ kind: p.kind, id: contentKey, version: copied.id }, { editor: true, limit: 1, signal: ctrl.signal })
      .then((r) => {
        const state = forkState(r.meta);
        if (ctrl.signal.aborted) return;
        if (state.failed && state.failed.at !== retried) endCopied.current(copied, state.failed);
        else if (state.copied && !state.failed) endCopied.current(copied);
      }, () => {});
    check();
    const timer = setInterval(check, 2000);
    return () => {
      ctrl.abort();
      clearInterval(timer);
    };
  }, [copied, retried, contentKey, p.kind, client]);
  // This version's copy: the one followed here, else its barrier (the page
  // was reloaded) or the last copy's failure.
  const received = forkState(folder.read.read?.meta);
  const copyingFrom = copying && copying.version.id === version?.id ? copying.body.from : received.copyingFrom;
  const copyFailure = copyingFrom ? undefined : received.failed;
  const refreshFiles = folder.read.refresh;
  const reloadFiles = folder.read.reload;
  const reloadContent = ed.reload;
  const wasCopying = useRef(false);
  useEffect(() => {
    if (!copyingFrom) {
      // Copied files, and chapters, which are the content's: read them again.
      if (wasCopying.current) {
        reloadFiles();
        reloadContent();
      }
      wasCopying.current = false;
      return;
    }
    wasCopying.current = true;
    const timer = setInterval(refreshFiles, 2500);
    return () => clearInterval(timer);
  }, [copyingFrom, refreshFiles, reloadFiles, reloadContent]);
  const notices = usePageNotices(item?.id, version?.id, { client });
  const reloadNotices = notices.reload;
  // A ZIP's pages appear after its commit. Media updates alone do not
  // refresh the notices when extraction changes the manifest.
  const mediaKey = ref && folder.read.read ? JSON.stringify([ref.id, ref.version,
    files.filter((file) => file.upload).map((file) => [file.path, file.size, file.staged, file.pending, file.failed]),
  ]) : null;
  useEffect(() => {
    if (mediaKey) reloadNotices();
  }, [mediaKey, reloadNotices]);
  const pages = folder.groups.filter((g) => g.rule?.page).flatMap((g) => g.files.filter((f) => !pageRemoved(f)));
  useEffect(() => {
    if (page && !pages.some((f) => f.path === page.path)) setPage(null);
  }, [page, pages]);
  const rules = folder.groups.flatMap((g) => (g.rule ? [g.rule] : []));
  const coverFile = version?.cover?.image ? files.find((f) => f.upload && !ruleFor(rules, f.path)?.page && stem(f.path).split("/").at(-1) === version.cover?.image) : undefined;
  // Until it has a cover of its own, a version shows its first page's small rendition, as published versions do.
  const firstPage = pages[0] && thumbnailOf(folder.read.read, pages[0]);
  const coverSrc = version?.cover?.url || coverFile?.editor_url || coverFile?.url || firstPage?.url;
  const coverCrop = useEditorCrop(ref ?? NO_REF, thumb?.path, { client, onError: p.onError, onSaved: () => setThumb(null) });
  const editThumbnail = (path: string) => setThumb({ path, aspect: ruleFor(rules, path)?.aspect ?? "" });
  const mediaFailed = !!folder.error || folder.queue.items.some((q) => q.status === "failed") || files.some((f) => f.failed);
  const processing = files.some(isProcessing);
  const selectedPages = pages.length;
  const filesPending = folder.busy || allocating || processing || folder.read.loading || cover.pending;
  const publishingBlocked =
    folder.busy || allocating || folder.read.loading || cover.pending ||
    mediaFailed ||
    !!folder.read.error ||
    selectedPages === 0 && !files.some((file) => file.upload && !file.unattached && file.type === "application/zip") ||
    coordinator.pending;
  const add = (input: Iterable<File>) => {
    const added = [...input];
    if (!added.length) return;
    folder.add(added);
    void ensure(added.find((f) => /\.zip$/i.test(f.name))?.name ?? added[0]?.name).catch(fail);
  };
  const dirty =
    fileDraft.dirty ||
    versionTexts.dirty.length > 0 ||
    publication.dirty.length > 0 ||
    texts.dirty.length > 0 ||
    details.dirty.length > 0 ||
    associations.dirty.length > 0 ||
    versionAssociations.dirty.length > 0 ||
    coordinator.dirty;
  // Queued files of a live edition count as edits until a save commits them.
  const unsaved = dirty || folder.pending.length > 0 || folder.queue.items.some((q) => q.status === "uploaded");
  useEffect(() => {
    if (!unsaved && !folder.busy) return;
    const ask = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    addEventListener("beforeunload", ask);
    return () => removeEventListener("beforeunload", ask);
  }, [unsaved, folder.busy]);
  const save = async (publish: boolean) => {
    if (saving || editionDeleted) return;
    setError(undefined);
    setNotice("");
    const state = latest.current;
    const active = state.texts.get()[state.language];
    const activeDisplayTitle = state.versionTexts.get()[state.editionLanguage]?.title ?? item?.text?.[state.editionLanguage]?.title ?? (item?.language === state.editionLanguage ? item.title : "");
    if (!(originalAndVersion ? activeDisplayTitle : active?.title)?.trim()) return setError(t("contentManager.titleRequired"));
    const problem = coordinator.problem();
    if (problem) return setError(t("readerEditor.fixSection", { section: problem }));
    if (!complete(state.details.get().access) || (state.details.get().access.level === "tier" && !state.details.get().access.tiers?.length))
      return setError(t("readerEditor.accessRequired"));
    let attributes: Record<string, unknown>;
    try {
      attributes = parseAttributes(spec.attributes ?? {}, state.details.get().attributes);
    } catch (e) {
      return fail(e instanceof RangeError ? new ValidationError(e.message) : e);
    }
    if (publish && publishingBlocked) return setError(t("readerEditor.filesNotReady"));
    setSaving(true);
    const settleTexts = state.texts.checkpoint(),
      settleVersionTexts = state.versionTexts.checkpoint(),
      settleDetails = state.details.checkpoint(),
      settleAssociations = state.associations.checkpoint(),
      settleVersionAssociations = state.versionAssociations.checkpoint(),
      settleFiles = fileDraft.checkpoint();
    const changedText = state.texts.dirty.slice();
    const changedDetails = state.details.dirty.slice();
    const snapshotTexts = { ...state.texts.get() };
    if (originalAndVersion) for (const l of changedText) {
      if (snapshotTexts[l] && !snapshotTexts[l].title.trim()) snapshotTexts[l] = { ...snapshotTexts[l], title: activeDisplayTitle };
    }
    const snapshotVersionTexts = { ...state.versionTexts.get() };
    let savedVersionTexts = snapshotVersionTexts;
    const snapshotDetails = state.details.get();
    const snapshotAssociations = state.associations.get().memberships.slice();
    const snapshotCreate = state.associations.get().create.slice();
    const snapshotVersionAssociations = state.versionAssociations.get().memberships.slice();
    const snapshotVersionCreate = state.versionAssociations.get().create.slice();
    const waiting = (c: NewEntity) => !!scopeMissing(c.kind, [...snapshotAssociations, ...snapshotVersionAssociations], [...snapshotCreate, ...snapshotVersionCreate]);
    let cleanAssociations = true;
    let draftSaved = false;
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
        if (changedDetails.some((field) => field !== "access")) {
          content = await client.contents.update(content.id, {
            ...(changedDetails.includes("originalTitle") ? { original_title: snapshotDetails.originalTitle } : {}),
            ...(changedDetails.includes("comments") ? { comments: snapshotDetails.comments } : {}),
            ...(changedDetails.includes("attributes") ? { attributes } : {}),
            revision: content.revision,
          });
        }
        if (changedDetails.includes("access")) {
          const target = version ? selectedEdition(content, version.id) : !item ? selectedEdition(content) : undefined;
          if (!target) throw new ValidationError(t("readerEditor.versionMissing"));
          const saved = await saveEditionAccess(client.contents, content, cleanRule(snapshotDetails.access), target.id);
          content = { ...content, versions: content.versions.map((edition) => edition.id === saved.id ? saved : edition) };
        }
        if (!current()) return;
        if (originalAndVersion && (state.versionTexts.dirty.length || !version?.text?.[state.editionLanguage]?.title)) {
          const targetVersion = version ? content.versions.find((v) => v.id === version.id) : content.versions[0];
          if (!targetVersion) throw new ValidationError(t("readerEditor.versionMissing"));
          const fresh = await client.contents.version(content.id, targetVersion.id, state.editionLanguage);
          const merged = { ...fresh.text };
          for (const l of state.versionTexts.dirty) merged[l] = snapshotVersionTexts[l]!;
          if (!merged[state.editionLanguage]?.title) merged[state.editionLanguage] = { ...merged[state.editionLanguage], title: activeDisplayTitle };
          const saved = await client.contents.updateVersion(content.id, targetVersion.id, { text: merged });
          savedVersionTexts = saved.text ?? merged;
        }
        if (!current()) return;
        const remember = (saved: { entities: EntityCard[] }) => setPicked((all) => new Map([...all, ...saved.entities.map((g) => [g.id, g] as const)]));
        if (state.associations.dirty.length) {
          const saved = await client.entities.setSharedOfContent(content.id, { kinds: [...sharedKinds], memberships: snapshotAssociations, create: snapshotCreate.filter((c) => !waiting(c)) }, language);
          remember(saved);
          cleanAssociations = settleAssociations({
            memberships: saved.memberships.filter((a) => !a.version_id && sharedKinds.includes(saved.entities.find((g) => g.id === a.group_id)?.kind ?? "")),
            create: snapshotCreate.filter(waiting),
          });
        }
        if (state.versionAssociations.dirty.length) {
          const targetVersion = version ? content.versions.find((v) => v.id === version.id) : content.versions[0];
          if (!targetVersion) throw new ValidationError(t("readerEditor.versionMissing"));
          const saved = await client.entities.setOfVersion(content.id, targetVersion.id, { kinds: [...versionKinds], memberships: snapshotVersionAssociations, create: snapshotVersionCreate.filter((c) => !waiting(c)) }, language);
          remember(saved);
          cleanAssociations = settleVersionAssociations({
            memberships: saved.memberships.filter((a) => a.version_id === targetVersion.id && versionKinds.includes(saved.entities.find((g) => g.id === a.group_id)?.kind ?? "")),
            create: snapshotVersionCreate.filter(waiting),
          }) && cleanAssociations;
        }
      });
      if (!current()) return;
      const blocked = await coordinator.save(current);
      if (blocked) throw new ValidationError(t("readerEditor.fixSection", { section: blocked }));
      if (!current()) return;
      // Edition sections can save their own policy. Checkpoint the latest
      // edition rule instead of restoring the earlier details snapshot.
      content = await client.contents.get(content.id, language);
      if (!current()) return;
      const cleanTexts = settleTexts(Object.fromEntries(languages.map((l) => [l.value, content.text?.[l.value] ?? snapshotTexts[l.value] ?? blank()])));
      const cleanVersionTexts = settleVersionTexts(savedVersionTexts);
      const cleanDetails = settleDetails({
        originalTitle: content.original_title ?? "",
        comments: content.comments,
        attributes: formOf(spec.attributes ?? {}, content.attributes),
        access: selectedEdition(content, version?.id)?.access ?? snapshotDetails.access,
      });
      if (!cleanTexts || !cleanVersionTexts || !cleanDetails || !cleanAssociations) throw new ValidationError(t("readerEditor.changedDuringSave"));
      // Files are the last draft mutation: a failed metadata section cannot
      // leave a rename/removal committed behind a still-undoable local draft.
      const currentFolder = folderRef.current;
      if (!currentFolder.busy && (currentFolder.pending.length || currentFolder.queue.items.some((q) => q.status === "uploaded"))) await currentFolder.commit();
      await fileDraft.commit();
      if (!settleFiles()) throw new ValidationError(t("readerEditor.changedDuringSave"));
      draftSaved = true;
      history.clear(publication.dirty.length ? publication.historyKey : undefined);
      if (publish) {
        const fresh = sourceFolderRef.current;
        if (!ref || fresh.busy || fresh.error || fresh.queue.items.some((q) => q.status === "failed"))
          throw new ValidationError(t("readerEditor.filesNotReady"));
        const edition = version ? await client.contents.version(content.id, version.id, language) : null;
        if (!edition) throw new ValidationError(t("readerEditor.versionMissing"));
        // The same action also retries an unfinished publication of an
        // already-live edition; it never withdraws its active reader snapshot.
        await client.contents.updateVersion(content.id, edition.id, { state: "published" });
        if (!current()) return;
        const settlePublication = publication.checkpoint();
        if (!content.live) {
          const at = versionSchedule || publishAt;
          const published = await guarded(() => client.contents.publish(content.id, at ? new Date(at).toISOString() : undefined));
          settlePublication({ at: published.publish_at ? localInput(new Date(published.publish_at)) : "" });
        } else ed.reload();
        history.clear();
      }
      setNotice(t(publish ? "contentManager.publishedNotice" : "pollEditor.saved"));
    } catch (e) {
      if (draftSaved && publish) {
        report(toContentKitError(e), "content.save");
        fail(new ValidationError(t("readerEditor.savedPublishFailed", { reason: contentError(m, toContentKitError(e)) })));
      } else fail(e);
    } finally {
      if (current()) setSaving(false);
    }
  };

  const customCover = async (file: File) => {
    const content = await ensure(file.name);
    const v = version ? content.versions.find((v) => v.id === version.id) : content.versions[0];
    if (!v || !current()) return;
    const target = { kind: p.kind, id: content.id, version: v.id };
    const up = await client.media.uploadNamed(target, file, { path: await client.media.presetPath(p.kind, "cover", file) });
    if (current()) await client.contents.setCover(content.id, v.id, up.name);
  };
  // The thumbnail is the version's cover: a copy of the chosen page, then cropped.
  // Cover uploads are server-named, so the copy takes a fresh i-{uuid} name.
  const setThumbnail = async (file: FileInfo) => {
    if (!ref || !item || !version) return;
    const ext = /\.[^./]+$/.exec(file.path)?.[0] ?? "";
    const path = await client.media.presetPath(p.kind, "cover", new File([], `i-${crypto.randomUUID()}${ext}`, { type: file.type }));
    await client.media.commit(ref, [{ op: "copy", from: { id: item.id, version: version.id, path: file.path }, to: path }]);
    if (!current()) return;
    await client.contents.setCover(item.id, version.id, stem(path).split("/").at(-1)!);
    if (current()) editThumbnail(path);
  };
  const { confirm, dialog } = useConfirm();
  useEffect(() => {
    onLeaveGuard?.(async () => !unsaved || await confirm({ title: t("readerEditor.leaveVersion"), description: t("readerEditor.discardVersionEdits"), confirm: t("readerEditor.discard") }));
    return () => onLeaveGuard?.(undefined);
  }, [onLeaveGuard, unsaved, confirm, t]);
  if (p.id && !item) return ed.error ? <LoadFailed error={ed.error} message={t("contentManager.loadFailed")} retry={ed.reload} /> : <Rows />;
  const status = item ? contentStatus(item) : "draft";
  const changeDetails = (patch: Partial<typeof details.value>) => details.edit((d) => ({ ...d, ...patch }));
  // The host's label for a language, unless it is only the code.
  const nameOf = (l: string) => { const label = languages.find((x) => x.value === l)?.label; return label && label !== l ? label : languageName(l); };
  const versionChoices = versions.map((v) => ({ id: v.id, label: versionLabel(v, nameOf, p.labels) + (v.state === "deleted" ? ` · ${t("contentManager.deleted")}` : "") }));
  const choiceLabel = (id: string) => versionChoices.find((v) => v.id === id)?.label ?? "";
  const discardLocalEdits = () => {
    folder.discard();
    fileDraft.discard();
    setDraftGeneration((value) => value + 1);
    texts.checkpoint()(Object.fromEntries(languages.map((l) => [l.value, editableText(item, l.value)])));
    versionTexts.checkpoint()({});
    details.checkpoint()({ originalTitle: item!.original_title ?? "", comments: item!.comments, attributes: formOf(spec.attributes ?? {}, item!.attributes ?? {}), access: version?.access ?? defaultAccessRule(model, p.kind) });
    publication.checkpoint()({ at: item!.publish_at ? localInput(new Date(item!.publish_at)) : "" });
    associations.checkpoint()({ memberships: membership.memberships.filter((a) => !a.version_id && sharedKinds.includes(membership.entities.get(a.group_id ?? "")?.kind ?? "")), create: [] });
    versionAssociations.checkpoint()({ memberships: [], create: [] });
    history.clear();
  };
  const chooseVersion = async (code: string) => {
    const next = versions.find((v) => v.code === code);
    if (!next || next.id === version?.id || saving || filesPending) return;
    if (unsaved && !(await confirm({ title: t("readerEditor.leaveVersion"), description: t("readerEditor.discardVersionEdits"), confirm: t("readerEditor.discard") }))) return;
    if (!current()) return;
    discardLocalEdits();
    if (p.onVersionChange) p.onVersionChange(next);
    else setChosen(next.code);
  };
  const renderEntities = (kinds: readonly string[], draft: typeof associations, scopedVersion?: string) => kinds.map((kind) => {
    const rows = draft.value.memberships.map((a, index) => ({ a, index })).filter(({ a }) => cards.get(a.group_id ?? "")?.kind === kind);
    return <EntityChips key={`${kind}:${scopedVersion ?? "shared"}`} kind={kind} label={scopedVersion ? t("readerEditor.versionKind", { kind: labelOf(p.labels, kind) }) : labelOf(p.labels, kind)} memberships={rows.map(({ a }) => a)} cards={cards}
      spec={model.entity_kinds[kind] ?? {}} labels={p.labels} allowVersionChange={false}
      versionLabel={(id) => versionChoices.find((v) => v.id === id)?.label ?? id} language={language} management={p.management} client={client}
      disabled={!!item && membership.loading} created={draft.value.create.filter((c) => c.kind === kind).map((c) => c.name)}
      needsScope={scopeMissing(kind, [...associations.value.memberships, ...versionAssociations.value.memberships], [...associations.value.create, ...versionAssociations.value.create])}
      onCreatedChange={(names) => draft.edit((d) => ({ ...d, create: [...d.create.filter((c) => c.kind !== kind), ...names.map((name) => ({ kind, name, language }))] }))}
      onChange={(i, patch) => draft.edit((d) => ({ ...d, memberships: d.memberships.map((x, j) => j === rows[i]!.index ? { ...x, ...patch, version_id: scopedVersion } : x) }))}
      onRemove={(i) => draft.edit((d) => ({ ...d, memberships: d.memberships.filter((_, j) => j !== rows[i]!.index) }))}
      onAdd={(card) => {
        setPicked((all) => new Map(all).set(card.id, card));
        draft.edit((d) => ({ ...d, memberships: [...d.memberships, { group_id: card.id, ...(scopedVersion ? { version_id: scopedVersion } : {}) }] }));
      }} />;
  });
  if (item && p.versionCode && !version) return ed.loading ? <Rows /> : <NoAccess>{t("readerEditor.versionMissing")}</NoAccess>;

  return (
    <EditorSaveContext.Provider value={coordinator.context}>
      <div className="grid gap-5 text-base" data-ckui="reader-content-editor" onKeyDown={(event) => {
        if (event.defaultPrevented || saving || !(event.ctrlKey || event.metaKey) || event.altKey || event.key.toLowerCase() !== "z") return;
        if ((event.target as Element).closest("input, textarea, [contenteditable='true']")) return;
        event.preventDefault();
        if (event.shiftKey) history.redo(); else history.undo();
      }}>
        <div className="flex flex-wrap items-center gap-3 rounded-xl border border-border bg-background p-4" data-ckui="editor-header">
          <button
            type="button"
            className="h-36 shrink-0 overflow-hidden rounded-md border border-border bg-muted disabled:cursor-default sm:h-40"
            style={{ aspectRatio: 0.7071 }}
            disabled={saving || cover.pending || editionDeleted}
            title={t("readerEditor.thumbnail")}
            aria-label={t("readerEditor.thumbnail")}
            onClick={() => setCoverChoices(true)}
            data-ckui="editor-thumbnail"
          >
            {coverSrc ? <img src={coverSrc} alt="" className="size-full object-cover" /> : (
              <span className="grid size-full place-content-center justify-items-center gap-1.5 p-2 text-center text-xs text-muted-foreground">
                <HugeiconsIcon icon={ImageAdd01Icon} strokeWidth={2} className="size-6" />
                {t("readerEditor.thumbnail")}
              </span>
            )}
          </button>
          <div className="min-w-32 flex-1">
            <h1 className="sr-only">{text.title || t("readerEditor.new")}</h1>
            <div className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
              <Badge variant={item?.live ? "default" : "outline"}>{t("readerEditor.galleryStatus", { status: t(`contentManager.${status}`) })}</Badge>
              {version && <span>{t("contentEditor.pageCount", { count: folder.read.read ? selectedPages : version.pages ?? 0 })}</span>}
              {allocating && <span role="status">{t("common.saving")}</span>}
            </div>
          </div>
          {item && (
            <div className="flex flex-wrap items-center gap-2" data-ckui="version-navigation">
              {version && <Select value={version.code} onValueChange={(code) => code && void chooseVersion(code)} disabled={saving || filesPending}>
                <SelectTrigger aria-label={t("contentEditor.versions")}><SelectValue>{versionChoices.find((v) => v.id === version.id)?.label}</SelectValue></SelectTrigger>
                <SelectContent>{versions.map((v) => <SelectItem key={v.id} value={v.code}>{versionChoices.find((c) => c.id === v.id)?.label}</SelectItem>)}</SelectContent>
              </Select>}
              <Button variant="outline" size="sm" disabled={saving || filesPending || unsaved || editionDeleted} onClick={() => setAddingVersion(true)}>{t("contentEditor.addVersion")}</Button>
            </div>
          )}
          {item && !editionDeleted && p.contentHref?.(item) && (
            <a href={p.contentHref(item)} className="text-sm underline" {...(item.live ? {} : { target: "_blank", rel: "noopener" })}>
              {t(item.live ? "contentManager.view" : "readerEditor.preview")}
            </a>
          )}
          <div ref={setVersionScheduleTarget} className="min-w-48" data-ckui="version-schedule-header" />
        </div>
        {/* Only the actions stay pinned while editing, below any host header that sets --ckui-sticky-top. */}
        <div className="sticky top-[var(--ckui-sticky-top,0px)] z-10 flex flex-wrap items-center justify-end gap-3 rounded-xl border border-border bg-background px-4 py-3 shadow-sm transition-[top] duration-300 motion-reduce:transition-none" data-ckui="editor-toolbar">
          <p className="me-auto min-w-0 truncate text-sm font-medium max-sm:hidden" aria-hidden>{(originalAndVersion ? displayTitle : text.title) || t("readerEditor.new")}</p>
          <div ref={setVersionRestoreTarget} data-ckui="version-restore-header" />
          {!editionDeleted && (item && !unsaved && !saving ? (
            <span role="status" className="text-sm text-muted-foreground" data-ckui="editor-saved">
              {t("readerEditor.allSaved")}
            </span>
          ) : (
            <Button disabled={saving || allocating || folder.updating || !unsaved} onClick={() => void save(false)}>
              {saving ? t("common.saving") : t(item?.live ? "readerEditor.saveChanges" : "readerEditor.saveDraft")}
            </Button>
          ))}
          <Button variant="outline" size="sm" disabled={saving || editionDeleted || !history.canUndo} onClick={history.undo}>{t("editor.undo")}</Button>
          <Button variant="outline" size="sm" disabled={saving || editionDeleted || !history.canRedo} onClick={history.redo}>{t("editor.redo")}</Button>
          {!editionDeleted && (!item?.live || version?.state === "draft") && (
            <Button variant="outline" disabled={saving || publishingBlocked} onClick={() => void save(true)}>
              {versionSchedule || publishAt ? t("readerEditor.schedule") : t("contentManager.publish")}
            </Button>
          )}
        </div>
        {error != null && <ErrorLine>{typeof error === "string" ? error : contentError(m, toContentKitError(error))}</ErrorLine>}
        {notice && (
          <p role="status" className="text-sm text-muted-foreground">
            {notice}
          </p>
        )}
        <fieldset disabled={saving || actions.pending || editionDeleted} className="grid min-w-0 items-start gap-5 xl:grid-cols-[minmax(0,1fr)_minmax(18rem,24rem)]">
          <div className="grid min-w-0 gap-5">
            <Panel title={t("readerEditor.details")}>
              <p className="text-sm text-muted-foreground">{t(originalAndVersion ? "readerEditor.titlesHint" : "readerEditor.sharedHint")}</p>
              {originalAndVersion ? <div className="grid gap-4">
                <div className="grid gap-1.5">
                  <Label htmlFor={`${ids}-original-title`}>{t("readerEditor.originalTitle")}</Label>
                  <Input id={`${ids}-original-title`} maxLength={512} value={details.value.originalTitle} onChange={(e) => changeDetails({ originalTitle: e.target.value })} />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor={`${ids}-title`}>{t("readerEditor.displayTitle", { language: languages.find((l) => l.value === editionLanguage)?.label ?? editionLanguage })}</Label>
                  <Input id={`${ids}-title`} required maxLength={512} value={displayTitle} onChange={(e) => versionTexts.edit((all) => ({ ...all, [editionLanguage]: { ...all[editionLanguage], title: e.target.value } }))} />
                </div>
              </div> : <>
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
              </>}
              <div className="grid gap-1.5">
                <Label htmlFor={`${ids}-description`}>{t("readerEditor.headerText")}</Label>
                <Textarea id={`${ids}-description`} rows={3} value={text.description ?? ""} onChange={(e) => editText({ description: e.target.value })} />
              </div>
              <AttributeFields
                declared={spec.attributes ?? {}}
                form={details.value.attributes}
                onChange={(name, value) => changeDetails({ attributes: { ...details.value.attributes, [name]: value } })}
                labels={p.labels}
              />
            </Panel>
            <Panel title={t("readerEditor.files")}>
              <p className="text-sm text-muted-foreground">{t("readerEditor.fileHelp")}</p>
              {page && (
                <div className="flex flex-wrap items-center gap-2 text-sm" data-ckui="selected-page">
                  <span className="text-muted-foreground">
                    {t("readerEditor.pageSelected", { name: folder.displayNames?.[page.path] ?? page.path.split("/").at(-1)! })} ({pages.findIndex((f) => f.path === page.path) + 1}/{pages.length})
                  </span>
                </div>
              )}
              {ref && <MediaReadinessNotice item={ref} client={client} />}
              <MediaFolderEditor
                folder={{ ...folder, add }}
                confirmRemove={false}
                label={false}
                layout="grid"
                disabled={saving || !!copyingFrom}
                inputLabel={t("readerEditor.files")}
                selectedPage={page?.path ?? null}
                onSelectPage={setPage}
              />
              {folder.read.error && <LoadFailed error={folder.read.error} message={t("contentManager.loadFailed")} retry={folder.read.reload} />}
              {(copyingFrom || mediaFailed || filesPending || (!folder.read.error && selectedPages === 0)) && (
                <p role="status" className="text-sm text-muted-foreground">
                  {copyingFrom ? t("readerEditor.copying", { version: choiceLabel(copyingFrom) }) : mediaFailed ? t("readerEditor.filesFailed") : filesPending ? t("readerEditor.processing") : t("readerEditor.addPages")}
                </p>
              )}
              {copyFailure && item && version && (
                <CopyFailed failure={copyFailure} version={choiceLabel(copyFailure.from)} pending={fork.pending}
                  onRetry={(f) => startCopy(item.id, version, { from: f.from, playback: f.playback, segments: f.segments }, f.at)} />
              )}
              {item && version && <PageNotices content={item.id} version={version.id} client={client} onError={p.onError} />}
            </Panel>
            {item && version && spec.segments && (
              <Panel title={t("contentEditor.segments")}>
                <SegmentsEditor key={version.id} content={item} version={version} client={client} onError={p.onError} heading={false} />
              </Panel>
            )}
            {item && version && uploadRules(folder.read.read).some(isAudioRule) && (
              <Panel title={t("readerManifest.title")}>
                <ReaderManifestEditor key={`${version.id}:${draftGeneration}`} kind={item.kind} content={item.id} version={version.id} client={client} onError={p.onError} heading={false} />
              </Panel>
            )}
            {item && version && spec.versions.traits?.includes("manhwa_style") && (
              <details className="rounded-xl border border-border bg-muted/30 p-4" onToggle={(e) => setToolsExpanded(e.currentTarget.open)}>
                <summary className="cursor-pointer font-medium">{t("readerEditor.readerTools")}</summary>
                <div hidden={!toolsExpanded} className="mt-4 grid gap-4">
                  <div ref={setVersionReaderTarget} data-ckui="version-reader-settings" />
                </div>
              </details>
            )}
          </div>
          <aside className="grid min-w-0 gap-5">
            <Panel title={t("readerEditor.sharedEntities")}>
              <p className="text-sm text-muted-foreground">{t("readerEditor.sharedHint")}</p>
              {membership.error && <ErrorLine error={membership.error} />}
              {renderEntities(sharedKinds, associations)}
            </Panel>
            {item && version && (
              <Panel title={t("readerEditor.versionDetails")}>
                <VersionEditor key={`${version.id}:${draftGeneration}`} content={item} version={version} client={client} labels={p.labels} onError={p.onError} media={false} chapters={false} showTraits={false} actionsTarget={versionActionsTarget} readerToolsTarget={versionReaderTarget} restoreTarget={versionRestoreTarget} onRestored={() => { history.clear(); ed.reload(); }}
                  scheduleTarget={versionScheduleTarget} onScheduleChange={setVersionSchedule} onLanguagesChange={onVersionLanguagesChange} scheduleDisabled={saving}
                  beforeDelete={async () => {
                    if (!current()) return false;
                    return await confirm({ title: t("contentEditor.deleteVersion"), description: t("readerEditor.deleteVersionHelp"), confirm: t("contentEditor.deleteVersion") }) && current();
                  }}
                  onDeleted={() => {
                    if (!current()) return;
                    discardLocalEdits();
                    const next = versions.find((v) => v.id !== version.id && v.state !== "deleted");
                    if (p.onVersionChange) p.onVersionChange(next ?? null, { replace: true });
                    else setChosen(next?.code ?? "");
                    ed.reload();
                  }} />
              </Panel>
            )}
            {versionKinds.length > 0 && (
              <Panel title={t("readerEditor.versionEntities")}>
                <p className="text-sm text-muted-foreground">{t("readerEditor.versionHint")}</p>
                {renderEntities(versionKinds, versionAssociations, version?.id)}
              </Panel>
            )}
            {!version && <Panel title={t("contentEditor.accessOnlyTitle")}>
              <RuleFields
                value={details.value.access}
                onChange={(access) => changeDetails({ access })}
                model={model}
                kind={p.kind}
                owner={item?.owner_group_id ?? p.ownerEntity}
                labels={p.labels}
                client={client}
              />
            </Panel>}
            {/* A new gallery has nothing to publish yet, unless comments can be set. */}
            {(item || spec.comment_modes?.length !== 1) && <Panel title={t("readerEditor.publication")}>
              {item && version && (
                <div className="grid gap-1 text-sm">
                  <div className="flex flex-wrap items-center gap-2 font-medium">
                    <span>{version.languages.map(nameOf).join(", ")}</span>
                    <VersionStatus version={version} />
                  </div>
                  {publicationOf(version)?.error && <ErrorLine>{publicationOf(version)!.error}</ErrorLine>}
                  {version.state === "published" && !item?.live && status !== "published" && (
                    <p className="text-pretty text-muted-foreground">
                      {status === "scheduled"
                        ? item.publish_at && Date.parse(item.publish_at) > Date.now() ? t("readerEditor.hiddenUntilDate", { date: formatDate(m, item.publish_at) }) : t("readerEditor.hiddenUntilLive")
                        : status === "publishing" ? t("contentEditor.publication_pending") : t(`readerEditor.hidden_${status}`)}
                    </p>
                  )}
                </div>
              )}
              {spec.comment_modes?.length !== 1 && (
                <>
                  <Label htmlFor={`${ids}-comments`}>{t("contentEditor.comments")}</Label>
                  <Select value={details.value.comments} onValueChange={(comments) => comments && changeDetails({ comments })}>
                    <SelectTrigger id={`${ids}-comments`}>
                      <SelectValue>{t(`contentEditor.comments_${details.value.comments as "open" | "closed" | "hidden"}`)}</SelectValue>
                    </SelectTrigger>
                    <SelectContent>
                      {(["open", "closed", "hidden"] as const).filter((mode) => !spec.comment_modes?.length || spec.comment_modes.includes(mode)).map((value) => (
                        <SelectItem key={value} value={value}>
                          {t(`contentEditor.comments_${value}`)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </>
              )}
              <div className="flex flex-wrap items-center justify-end gap-2">
                {version?.state === "published" && (
                  <Button variant="outline" disabled={actions.pending} onClick={() => void actions.run(async () => {
                    if (unsaved && !(await confirm({ title: t("contentManager.unpublish"), description: t("readerEditor.discardBeforeUnpublish"), confirm: t("contentManager.unpublish") }))) return false;
                    await client.contents.updateVersion(item!.id, version.id, { state: "draft" });
                    if (current()) { discardLocalEdits(); ed.reload(); }
                    return true;
                  })}>
                    {t("contentManager.unpublish")}
                  </Button>
                )}
                <div ref={setVersionActionsTarget} className="contents" data-ckui="version-destructive-actions" />
              </div>
              <ErrorLine error={actions.error} />
            </Panel>}
          </aside>
        </fieldset>
        <Dialog open={coverChoices} onOpenChange={(open) => !cover.pending && setCoverChoices(open)}>
          <DialogContent showCloseButton={!cover.pending}>
            <DialogHeader><DialogTitle>{t("readerEditor.thumbnail")}</DialogTitle></DialogHeader>
            <p className="text-sm text-muted-foreground">{t("readerEditor.thumbnailHelp")}</p>
            <label className="inline-flex cursor-pointer justify-center rounded-md border border-border px-3 py-2 text-sm font-medium hover:bg-muted">
              {t("contentManager.addCover")}
              <input type="file" accept="image/*" className="sr-only" aria-label={t("contentManager.cover")} disabled={cover.pending || saving} onChange={(e) => {
                const file = e.target.files?.[0]; e.target.value = "";
                if (file) void cover.run(async () => { await customCover(file); return true; }).then((ok) => ok && setCoverChoices(false));
              }} />
            </label>
            {page && <Button disabled={cover.pending || saving} onClick={() => void cover.run(async () => { await setThumbnail(page); return true; }).then((ok) => ok && setCoverChoices(false))}>{t("readerEditor.setThumbnail")}</Button>}
            {coverFile && <Button variant="outline" onClick={() => { setCoverChoices(false); editThumbnail(coverFile.path); }}>{t("folder.crop")}</Button>}
            <ErrorLine error={cover.error} />
          </DialogContent>
        </Dialog>
        {/* Closed during a copy, the dialog shows the added version, which follows the copy. */}
        <Dialog open={addingVersion} onOpenChange={(open) => {
          if (open || !copy) setAddingVersion(open);
          else if (copying) {
            setAddingVersion(false);
            showVersion(copy.version);
          } else openVersion(copy.version);
        }}>
          <DialogContent>
            <DialogHeader><DialogTitle>{t("contentEditor.addVersion")}</DialogTitle></DialogHeader>
            <AddVersion heading={false} languages={model.languages} many={spec.versions.languages === "many"} pending={addVersion.pending || !!copying}
              sources={versions.filter((v) => v.state !== "deleted").map((v) => ({ id: v.id, label: versionChoices.find((c) => c.id === v.id)?.label ?? v.code, audio: v.has_audio, chapters: !!spec.segments && v.segments.length > 0 }))}
              onAdd={(input, from) => {
                if (!item) return;
                const content = item.id;
                void addVersion.run(() => client.contents.addVersion(content, { ...input, access: version?.access ?? item.access })).then((v) => {
                  if (!v || !current()) return;
                  if (from) startCopy(content, v, from);
                  else openVersion(v);
                });
              }} />
            <ErrorLine error={addVersion.error} />
            {copy && (copy.failed ? (
              <CopyFailed failure={copy.failed} version={choiceLabel(copy.body.from)} pending={fork.pending} onRetry={(f) => item && startCopy(item.id, copy.version, copy.body, f.at)} />
            ) : fork.error ? (
              <div className="flex flex-wrap items-center gap-2">
                <ErrorLine>{t("readerEditor.copyFailed", { error: contentError(m, fork.error) })}</ErrorLine>
                <Button size="sm" variant="outline" disabled={fork.pending} onClick={() => item && startCopy(item.id, copy.version, copy.body, copy.retried)}>{t("common.retry")}</Button>
              </div>
            ) : (
              <p role="status" className="text-sm text-pretty text-muted-foreground">{t("readerEditor.copying", { version: choiceLabel(copy.body.from) })}</p>
            ))}
          </DialogContent>
        </Dialog>
        {dialog}
        <ImageCropDialog
          open={!!thumb}
          onOpenChange={(open) => !open && coverCrop.status !== "saving" && setThumb(null)}
          source={"source" in coverCrop ? (coverCrop.source ?? null) : null}
          aspect={thumb?.aspect ?? ""}
          rotatable={false}
          initialEdit={"edit" in coverCrop ? coverCrop.edit : null}
          title={t("readerEditor.thumbnail")}
          confirmLabel={t("readerEditor.setThumbnail")}
          busy={coverCrop.status === "saving"}
          error={coverCrop.status === "error" ? contentError(m, coverCrop.error) : undefined}
          onConfirm={(edit) => void coverCrop.save(edit)}
        />
      </div>
    </EditorSaveContext.Provider>
  );
}
