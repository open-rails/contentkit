import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import { imageRefs } from "../../client/contents.js";
import type { Content } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useAdminLocation, useFlowClient, useStaffAccess, type AdminLocation, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { useContentKitClient, useErrorReporter, type ContentKitErrorHandler } from "../../react/context.js";
import { useContent } from "../../react/contents.js";
import { useKinds } from "../../react/groups.js";
import { keyOf, useContentScope, usePaged } from "../../react/use-resource.js";
import { Alert, AlertDescription } from "#ckui/ui/alert";
import { Badge } from "#ckui/ui/badge";
import { Button, buttonVariants } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { Textarea } from "#ckui/ui/textarea";
import { localInput } from "../social/bans.js";
import { contentError, ErrorLine, formatDate, RelativeTime, useAction } from "../social/parts.js";
import { ReaderContentEditor } from "./reader-content-editor.js";
import { ContentSections } from "../content/editors.js";
import type { Labels } from "../groups/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Pager, Rows, pageOf, useConfirm, type AdminFlowProps, type AdminRouteProps } from "./shell.js";

type Choice = string | { value: string; label: string };

/** What ContentManager hands its body editor. */
export interface BodyEditorProps {
  /** The body as HTML, its images at URLs that show them now. */
  value: string;
  onChange: (html: string) => void;
  /** Uploads an image to the item and resolves with a URL that shows it; absent until the item exists. */
  uploadImage?: (file: File) => Promise<string>;
  disabled?: boolean;
  placeholder?: string;
  "aria-label"?: string;
}

export interface ContentManagerProps extends AdminFlowProps, AdminRouteProps {
  /** The content kind managed. */
  kind: string;
  /** Languages to write in: codes, or codes with labels; the first is a new item's. */
  languages?: readonly Choice[];
  /**
   * The body editor: `richTextEditor` from `@openrails/contentkit-ui/editor`,
   * or the host's own. Without it, the body is a plain HTML field.
   */
  renderEditor?: (props: BodyEditorProps) => ReactNode;
  /** A link to a live item's page. */
  contentHref?: (content: Content) => string | undefined;
  /** Display names of kinds, traits, tiers, attributes and values by identifier. */
  labels?: Labels;
  pageSize?: number;
}

/** Whether the caller manages content: root:content:write holders, creators and owner-group editors. */
export function useContentManagerAccess(o: FlowClientOptions = {}): StaffAccess<"contents"> {
  return useStaffAccess(["contents"] as const, o);
}

export type ContentStatus = "draft" | "scheduled" | "published" | "held" | "rejected";

/** An item's status as the manager names it. */
export function contentStatus(c: Content): ContentStatus {
  if (c.moderation === "rejected") return "rejected";
  if (c.moderation === "held") return "held";
  if (c.state === "draft") return "draft";
  if (!c.live) return "scheduled";
  return "published";
}

const choices = (list?: readonly Choice[]) => (list ?? []).map((c) => (typeof c === "string" ? { value: c, label: c } : c));
const badge: Record<ContentStatus, "default" | "secondary" | "outline" | "destructive"> = { published: "default", scheduled: "secondary", draft: "outline", held: "secondary", rejected: "destructive" };
const states = ["draft", "scheduled", "published"] as const;

/**
 * Content items of one kind for their creators and staff: every one the
 * caller manages, by state and text, deleted ones on their own to restore;
 * create and edit title, description, schedule, cover and body with
 * inline images per language, publish or keep a draft, and delete; then its
 * listing and attributes, access rule and price, versions (order, languages,
 * traits, schedule, access override, price, segments, media) and groups. The
 * list is at basePath, a new item at basePath/new and each item at
 * basePath/{id}, filters in the query.
 */
export function ContentManager({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: ContentManagerProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="content-manager" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Contents {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<ContentManagerProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };

function Contents({ basePath, navigation, ...p }: Inner) {
  const { t } = useMessages();
  const loc = useAdminLocation(basePath, navigation);
  const access = useContentManagerAccess({ client: p.client });
  // Editor URLs name items by content code; the editor itself works on ids.
  const known = useRef(new Map<string, string>()).current;
  const open = (c: Content, replace = false) => {
    known.set(c.code, c.id);
    loc.go(c.code, undefined, replace ? { replace: true } : undefined);
  };
  const target = useCodeTarget(p.client, p.kind, loc.view && loc.view !== "new" ? loc.view : null, known);
  if (access.loading) return <Rows />;
  if (!access.allowed.contents) return <NoAccess>{t("contentManager.noAccess")}</NoAccess>;
  if (!loc.view) return <List {...p} loc={loc} open={open} />;
  if (loc.view !== "new" && !target.id) {
    if (target.error) return <LoadFailed error={target.error} message={t("contentManager.loadFailed")} retry={target.retry} />;
    return target.missing ? <NoAccess>{t("contentManager.notFound")}</NoAccess> : <Rows />;
  }
  return (
    <>
      <Button variant="ghost" size="sm" className="justify-self-start" onClick={() => loc.go("")}>
        {t("contentManager.back")}
      </Button>
      <ContentEditor
        management
        {...p}
        id={loc.view === "new" ? null : target.id}
        onCreated={(c) => open(c, true)}
        onDeleted={() => loc.go("", undefined, { replace: true })}
      />
    </>
  );
}

/** Resolves an editor URL's content code to its id within the caller's manage rights; known codes resolve at once. */
function useCodeTarget(client: ContentKitClient, kind: string, code: string | null, known: Map<string, string>) {
  const [found, setFound] = useState<{ code: string; id: string | null; error?: ContentKitError } | null>(null);
  const [attempt, setAttempt] = useState(0);
  const cached = code ? known.get(code) : undefined;
  useEffect(() => {
    if (!code || cached) return;
    const ctl = new AbortController();
    client.contents.manage({ kind, code, limit: 1 }, ctl.signal).then(
      (items) => {
        const id = items[0]?.id ?? null;
        if (id) known.set(code, id);
        setFound({ code, id });
      },
      (e) => {
        if (!ctl.signal.aborted) setFound({ code, id: null, error: toContentKitError(e) });
      },
    );
    return () => ctl.abort();
  }, [client, kind, code, cached, known, attempt]);
  const retry = () => setAttempt((n) => n + 1);
  if (!code) return { id: null, retry };
  if (cached) return { id: cached, retry };
  if (found?.code !== code) return { id: null, retry };
  return { id: found.id, error: found.error, missing: !found.id && !found.error, retry };
}

function List({ loc, open, client, kind, languages: offered, contentHref, pageSize = 20, onError }: Inner & { loc: AdminLocation; open: (c: Content) => void }) {
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const language = choices(offered)[0]?.value;
  const query = loc.query.get("q") ?? "";
  const status = states.find((s) => s === loc.query.get("status")) ?? "";
  const deleted = loc.query.get("deleted") === "1";
  const page = pageOf(loc.query);
  const filters = { q: query, status, deleted: deleted ? "1" : undefined };
  const filter = (next: Partial<typeof filters>) => loc.go("", { ...filters, ...next });
  const [text, setText] = useState(query);
  useEffect(() => setText(query), [query]);
  const { store, scope } = useContentScope(client);
  const state = deleted ? "deleted" : status || undefined;
  const list = usePaged(store, keyOf("contents-page", scope, kind, query, state, pageSize), { type: "contents", scope: deleted ? "deleted" : undefined }, page, pageSize, (q, signal) =>
    client.contents.manage({ ...q, kind, q: query || undefined, state, language }, signal),
  );
  const del = useAction("content.save", onError);
  const { confirm, dialog } = useConfirm();
  const remove = async (c: Content) => {
    if (await confirm({ title: t("contentManager.deleteTitle", { title: c.title }), description: t("contentManager.deleteBody"), confirm: t("contentManager.delete") })) {
      await del.run(() => client.contents.remove(c.id));
    }
  };
  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold">{t("contentManager.title")}</h2>
        <Button size="sm" onClick={() => loc.go("new")}>
          {t("contentManager.new")}
        </Button>
      </div>
      <div className="flex gap-2" role="group" aria-label={t("contentManager.title")}>
        <Button size="sm" variant={deleted ? "outline" : "default"} aria-pressed={!deleted} onClick={() => loc.go("", { q: query })}>
          {t("contentManager.items")}
        </Button>
        <Button size="sm" variant={deleted ? "default" : "outline"} aria-pressed={deleted} onClick={() => loc.go("", { q: query, deleted: "1" })}>
          {t("contentManager.deleted")}
        </Button>
      </div>
      <div className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_auto]">
        <form
          className="flex gap-2"
          role="search"
          onSubmit={(e) => {
            e.preventDefault();
            filter({ q: text.trim() });
          }}
        >
          <Label htmlFor={`${ids}-q`} className="sr-only">
            {t("contentManager.search")}
          </Label>
          <Input id={`${ids}-q`} className="h-8" type="search" value={text} placeholder={t("contentManager.search")} onChange={(e) => setText(e.target.value)} />
          <Button type="submit" size="sm" variant="outline">
            {t("moderation.filter")}
          </Button>
        </form>
        {!deleted && (
          <Select value={status} onValueChange={(v) => filter({ status: String(v ?? "") as typeof status })}>
            <SelectTrigger size="sm" aria-label={t("contentManager.status")}>
              <SelectValue>{(v: string) => t(v ? `contentManager.${v as (typeof states)[number]}` : "contentManager.allStatuses")}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="">{t("contentManager.allStatuses")}</SelectItem>
              {states.map((s) => (
                <SelectItem key={s} value={s}>
                  {t(`contentManager.${s}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
      </div>
      <ErrorLine error={del.error} />
      {list.loading && !list.items.length ? (
        <Rows />
      ) : list.error && !list.items.length ? (
        <LoadFailed error={list.error} message={t("contentManager.loadFailed")} retry={list.reload} />
      ) : !list.items.length ? (
        <p className="text-muted-foreground">{t(deleted ? "contentManager.noDeleted" : "contentManager.empty")}</p>
      ) : (
        <ul className="grid divide-y divide-border rounded-lg border border-border" aria-busy={list.loading}>
          {list.items.map((c) => {
            const s = contentStatus(c);
            const href = s === "published" ? contentHref?.(c) : undefined;
            return (
              <li key={c.id} className="flex flex-wrap items-start gap-3 p-3" data-ckui="content-row" data-status={deleted ? "deleted" : s}>
                <div className="grid min-w-0 flex-1 gap-1">
                  {deleted ? (
                    <span className="font-medium break-words">{c.title}</span>
                  ) : (
                    <button type="button" className="text-left font-medium break-words hover:underline" onClick={() => open(c)}>
                      {c.title}
                    </button>
                  )}
                  <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
                    {!deleted && <Badge variant={badge[s]}>{t(`contentManager.${s}`)}</Badge>}
                    <span>{c.language}</span>
                    <RelativeTime at={deleted && c.deleted_at ? c.deleted_at : c.updated_at} />
                    {s === "scheduled" && c.publish_at && <span>{t("pollManager.goesLive", { date: formatDate(m, c.publish_at) })}</span>}
                  </div>
                </div>
                <div className="flex flex-wrap gap-2">
                  {deleted ? (
                    <Button size="sm" variant="outline" disabled={del.pending} onClick={() => void del.run(() => client.contents.restore(c.id))}>
                      {t("moderation.restore")}
                    </Button>
                  ) : (
                    <>
                      {href && (
                        <a className={buttonVariants({ size: "sm", variant: "ghost" })} href={href}>
                          {t("contentManager.view")}
                        </a>
                      )}
                      <Button size="sm" variant="outline" onClick={() => open(c)}>
                        {t("comments.edit")}
                      </Button>
                      <Button size="sm" variant="destructive" disabled={del.pending} onClick={() => void remove(c)}>
                        {t("comments.delete")}
                      </Button>
                    </>
                  )}
                </div>
              </li>
            );
          })}
        </ul>
      )}
      <Pager page={page} hasNext={list.hasNext} disabled={list.loading} onPage={(n) => loc.go("", { ...filters, page: n > 1 ? n : undefined })} />
      {dialog}
    </div>
  );
}

interface Form {
  title: string;
  description: string;
  publishAt: string;
  body: string;
}

const empty: Form = { title: "", description: "", publishAt: "", body: "" };

export interface ContentEditorProps {
  /** Staff entity lookup, including internal classifications. */
  management?: boolean;
  /** The item edited; null creates one of kind. */
  id: string | null;
  kind: string;
  /** Languages to write in: codes, or codes with labels; the first is a new item's. */
  languages?: readonly Choice[];
  /** The body editor; without it, a plain HTML field. */
  renderEditor?: (props: BodyEditorProps) => ReactNode;
  /** A link to a live item's page. */
  contentHref?: (content: Content) => string | undefined;
  labels?: Labels;
  /** A new item's owner group: a channel its editors write for. */
  ownerGroup?: string;
  /** After create: show the new item (the editor goes on editing it). */
  onCreated?: (content: Content) => void;
  onDeleted?: (content: Content) => void;
  client?: ContentKitClient;
  onError?: ContentKitErrorHandler;
}

/**
 * One content item for its creators and staff: title, description,
 * schedule, cover and body per language, publish, unpublish and delete;
 * then listing and attributes, access rule and price, versions and groups.
 */
export function ContentEditor(p:ContentEditorProps){
  const {client,scope}=useContentScope(p.client);
  const allocated=useRef<{id:string;session:number}|null>(null);
  const [session,setSession]=useState({id:p.id,kind:p.kind,client,scope,key:0});
  let boundary=session;
  if(session.id!==p.id||session.kind!==p.kind||session.client!==client||session.scope!==scope){
    const continues=session.id==null&&p.id===allocated.current?.id&&allocated.current?.session===session.key&&session.kind===p.kind&&session.client===client&&session.scope===scope;
    boundary={id:p.id,kind:p.kind,client,scope,key:continues?session.key:session.key+1};
    setSession(boundary);
  }
  const {kinds,error,reload}=useKinds({client});
  const {t}=useMessages();
  if(!kinds)return error?<LoadFailed error={error} message={t("contentManager.loadFailed")} retry={reload}/>:<Rows/>;
  const props={...p,client,onCreated:(content:Content)=>{allocated.current={id:content.id,session:boundary.key};p.onCreated?.(content);}};
  if(kinds.kinds[p.kind]?.presentation==="reader")return <ReaderContentEditor key={boundary.key} {...props} model={kinds}/>;
  return <StandardContentEditor key={boundary.key} {...props}/>;
}

function StandardContentEditor({ id, client: own, kind, languages: offered, renderEditor, contentHref, labels, ownerGroup, onCreated, onDeleted, onError, management }: ContentEditorProps) {
  const client = useContentKitClient(own);
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const languages = choices(offered);
  const [language, setLanguage] = useState(languages[0]?.value ?? "en");
  const ed = useContent(id, kind, language, { client });
  // A hidden version's cover is edited here; a visible version's in its own form.
  const hidden = !!useKinds({ client }).kinds?.kinds[kind]?.versions.hidden;
  const report = useErrorReporter(onError);
  // The body is kept as stored (image references); the editor shows it with URLs.
  const [form, setForm] = useState<Form | null>(id ? null : empty);
  const [error, setError] = useState<ContentKitError | string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const { confirm, dialog } = useConfirm();
  const item = ed.content;
  useEffect(() => {
    if (!item) return;
    const stored = item.text?.[language];
    setForm((f) => f ?? { title: stored?.title ?? "", description: stored?.description ?? "", body: stored?.body ?? "",
      publishAt: item.publish_at && !item.live ? localInput(new Date(item.publish_at)) : "" });
  }, [item, language]);
  const { imageSrc } = ed;
  const shown = useMemo(
    () => (form?.body ?? "").replace(imageRefs(), (ref, name: string) => {
      const src = imageSrc(name);
      return src && src !== name ? src.replaceAll("&", "&amp;") : ref;
    }),
    [form?.body, imageSrc],
  );
  if (id && !form) {
    if (ed.error) return <LoadFailed error={ed.error} message={t("contentManager.loadFailed")} retry={ed.reload} />;
    return (
      <div className="grid gap-3" aria-busy>
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }
  const f = form!;
  const set = (patch: Partial<Form>) => setForm((cur) => ({ ...cur!, ...patch }));
  const fail = (e: unknown) => {
    const err = toContentKitError(e);
    setError(err);
    report(err, "content.save");
  };
  const text = () => ({ title: f.title.trim(), description: f.description.trim(), body: f.body });
  const save = async () => {
    setError(null);
    setNotice(null);
    if (!f.title.trim()) return setError(t("contentManager.titleRequired"));
    try {
      if (!item) {
        const created = await ed.create({ kind, text: { [language]: text() }, owner_group_id: ownerGroup });
        onCreated?.(created);
        return;
      }
      await ed.putText(text());
      setNotice(t("pollEditor.saved"));
    } catch (e) {
      fail(e);
    }
  };
  const publish = async (live: boolean) => {
    setError(null);
    setNotice(null);
    try {
      if (live) await ed.publish(f.publishAt ? new Date(f.publishAt).toISOString() : undefined);
      else await ed.unpublish();
      setNotice(t(live ? "contentManager.publishedNotice" : "contentManager.unpublishedNotice"));
    } catch (e) {
      fail(e);
    }
  };
  const remove = async () => {
    if (!item || !(await confirm({ title: t("contentManager.deleteTitle", { title: item.title }), description: t("contentManager.deleteBody"), confirm: t("contentManager.delete") }))) return;
    try {
      await ed.remove();
      onDeleted?.(item);
    } catch (e) {
      fail(e);
    }
  };
  const s = item ? contentStatus(item) : "draft";
  const href = item && s === "published" ? contentHref?.(item) : undefined;
  const editor: BodyEditorProps = {
    value: shown,
    onChange: (html) => set({ body: ed.storedBody(html) }),
    uploadImage: item ? (file) => ed.uploadImage(file) : undefined,
    disabled: ed.saving,
    placeholder: t("contentManager.bodyPlaceholder"),
    "aria-label": t("contentManager.body"),
  };
  return (
    <>
    <form
      className="grid gap-4"
      data-ckui="content-form"
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="text-base font-semibold">{t(item ? "contentManager.editTitle" : "contentManager.newTitle")}</h2>
        {item && <Badge variant={badge[s]}>{t(`contentManager.${s}`)}</Badge>}
        {href && (
          <a className="text-xs text-muted-foreground hover:underline" href={href}>
            {t("contentManager.view")}
          </a>
        )}
      </div>
      {item?.moderation && (
        <Alert variant={item.moderation === "rejected" ? "destructive" : "default"}>
          <AlertDescription>
            {item.moderation_reason
              ? t(`contentManager.${item.moderation === "rejected" ? "rejectedBecause" : "heldBecause"}`, { reason: item.moderation_reason })
              : t(`contentManager.${item.moderation === "rejected" ? "rejectedNote" : "heldNote"}`)}
          </AlertDescription>
        </Alert>
      )}
      {languages.length > 1 && (
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-lang`}>{t("contentManager.language")}</Label>
          <Select
            value={language}
            onValueChange={(v) => {
              setLanguage(String(v ?? ""));
              setForm(item ? null : empty);
            }}
          >
            <SelectTrigger id={`${ids}-lang`} className="w-full">
              <SelectValue>{(v: string) => languages.find((l) => l.value === v)?.label ?? v}</SelectValue>
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
      )}
      <div className="grid gap-1.5">
        <Label htmlFor={`${ids}-title`}>{t("contentManager.itemTitle")}</Label>
        <Input id={`${ids}-title`} value={f.title} required maxLength={300} onChange={(e) => set({ title: e.target.value })} />
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-publish`}>{t("contentManager.publishAt")}</Label>
          <Input id={`${ids}-publish`} type="datetime-local" value={f.publishAt} onChange={(e) => set({ publishAt: e.target.value })} />
        </div>
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor={`${ids}-description`}>{t("contentManager.description")}</Label>
        <Textarea id={`${ids}-description`} rows={2} value={f.description} onChange={(e) => set({ description: e.target.value })} />
      </div>
      {(hidden || !item) && <Cover item={item} src={ed.coverSrc} saving={ed.saving} onFile={(file) => void ed.setCover(file).catch(fail)} />}
      <div className="grid gap-1.5">
        <span className="text-sm font-medium">{t("contentManager.body")}</span>
        {renderEditor ? renderEditor(editor) : <Textarea aria-label={editor["aria-label"]} rows={12} value={f.body} placeholder={editor.placeholder} onChange={(e) => set({ body: e.target.value })} />}
        {!item && <p className="text-xs text-muted-foreground">{t("contentManager.imagesAfterCreate")}</p>}
      </div>
      {error && <ErrorLine>{typeof error === "string" ? error : contentError(m, error)}</ErrorLine>}
      {notice && !error && (
        <p role="status" className="text-sm text-muted-foreground">
          {notice}
        </p>
      )}
      <div className="flex flex-wrap gap-2 border-t border-border pt-4">
        <Button type="submit" disabled={ed.saving}>
          {ed.saving ? t("common.saving") : t(item ? "common.save" : "contentManager.createDraft")}
        </Button>
        {item && !item.live && (
          <Button type="button" variant="outline" disabled={ed.saving} onClick={() => void publish(true)}>
            {t("contentManager.publish")}
          </Button>
        )}
        {item?.state === "published" && (
          <Button type="button" variant="outline" disabled={ed.saving} onClick={() => void publish(false)}>
            {t("contentManager.unpublish")}
          </Button>
        )}
        {item && (
          <Button type="button" variant="destructive" className="ms-auto" disabled={ed.saving} onClick={() => void remove()}>
            {t("contentManager.delete")}
          </Button>
        )}
      </div>
      {dialog}
    </form>
    {item && <ContentSections management={management} content={item} client={client} onError={onError} labels={labels} />}
    </>
  );
}

function Cover({ item, src, saving, onFile }: { item: Content | null; src?: string; saving: boolean; onFile: (file: File | null) => void }) {
  const { t } = useMessages();
  const file = useRef<HTMLInputElement>(null);
  return (
    <div className="grid gap-1.5" data-ckui="content-cover">
      <span className="text-sm font-medium">{t("contentManager.cover")}</span>
      {item ? (
        <div className="flex flex-wrap items-center gap-3">
          {src ? <img src={src} alt="" className="h-20 w-36 rounded-md border border-border object-cover" /> : <span className="text-muted-foreground">{t("contentManager.noCover")}</span>}
          <Button type="button" size="sm" variant="outline" disabled={saving} onClick={() => file.current?.click()}>
            {t(src ? "pollEditor.replaceImage" : "contentManager.addCover")}
          </Button>
          {src && (
            <Button type="button" size="sm" variant="ghost" disabled={saving} onClick={() => onFile(null)}>
              {t("pollEditor.removeImage")}
            </Button>
          )}
          <input
            ref={file}
            type="file"
            accept="image/*"
            hidden
            aria-label={t("contentManager.cover")}
            onChange={(e) => {
              const f = e.target.files?.[0];
              e.target.value = "";
              if (f) onFile(f);
            }}
          />
        </div>
      ) : (
        <p className="text-xs text-muted-foreground">{t("contentManager.imagesAfterCreate")}</p>
      )}
    </div>
  );
}
