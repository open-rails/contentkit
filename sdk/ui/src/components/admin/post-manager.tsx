import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import { imageRefs } from "../../client/content/posts.js";
import type { Post, PostInput } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useAdminLocation, useFlowClient, useStaffAccess, type AdminLocation, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { useErrorReporter } from "../../react/context.js";
import { usePost } from "../../react/posts.js";
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
import { AdminRoot, LoadFailed, NoAccess, Pager, Rows, pageOf, useConfirm, type AdminFlowProps, type AdminRouteProps } from "./shell.js";

type Choice = string | { value: string; label: string };

/** What PostManager hands its body editor. */
export interface PostEditorProps {
  /** The body as HTML, its images at URLs that show them now. */
  value: string;
  onChange: (html: string) => void;
  /** Uploads an image to the post and resolves with a URL that shows it; absent until the post exists. */
  uploadImage?: (file: File) => Promise<string>;
  disabled?: boolean;
  placeholder?: string;
  "aria-label"?: string;
}

export interface PostManagerProps extends AdminFlowProps, AdminRouteProps {
  /** Languages to filter by and write in: codes, or codes with labels; the first is a new post's. */
  languages?: readonly Choice[];
  /**
   * The body editor: `richTextEditor` from `@openrails/contentkit-ui/editor`,
   * or the host's own. Without it, the body is a plain HTML field.
   */
  renderEditor?: (props: PostEditorProps) => ReactNode;
  /** A link to a published post's page. */
  postHref?: (post: Post) => string | undefined;
  pageSize?: number;
}

/** Whether the caller may manage posts (root:posts:write). */
export function usePostManagerAccess(o: FlowClientOptions = {}): StaffAccess<"posts"> {
  return useStaffAccess(["posts"] as const, o);
}

export type PostStatus = "draft" | "scheduled" | "published" | "held" | "rejected";

/** A post's status as the staff list names it. */
export function postStatus(p: Post, now = Date.now()): PostStatus {
  if (p.moderation === "rejected") return "rejected";
  if (p.moderation === "held") return "held";
  if (p.is_draft) return "draft";
  if (p.live_at && Date.parse(p.live_at) > now) return "scheduled";
  return "published";
}

const choices = (list?: readonly Choice[]) => (list ?? []).map((c) => (typeof c === "string" ? { value: c, label: c } : c));
const badge: Record<PostStatus, "default" | "secondary" | "outline" | "destructive"> = { published: "default", scheduled: "secondary", draft: "outline", held: "secondary", rejected: "destructive" };

/**
 * Staff: every post, by language, draft state and text, deleted ones on
 * their own to restore; create and edit title, slug, language, excerpt,
 * schedule, cover and body with inline images, publish or keep a draft, and
 * delete. The list is at basePath, a new post at basePath/new and each post
 * at basePath/{id}, filters in the query.
 */
export function PostManager({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: PostManagerProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="post-manager" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Posts {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<PostManagerProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };

function Posts({ basePath, navigation, ...p }: Inner) {
  const { t } = useMessages();
  const loc = useAdminLocation(basePath, navigation);
  const access = usePostManagerAccess({ client: p.client });
  if (access.loading) return <Rows />;
  if (!access.allowed.posts) return <NoAccess>{t("postManager.noAccess")}</NoAccess>;
  if (!loc.view) return <List {...p} loc={loc} />;
  return (
    <>
      <Button variant="ghost" size="sm" className="justify-self-start" onClick={() => loc.go("")}>
        {t("postManager.back")}
      </Button>
      <PostForm key={loc.view} {...p} loc={loc} id={loc.view === "new" ? null : loc.view} />
    </>
  );
}

function List({ loc, client, languages: offered, postHref, pageSize = 20, onError }: Inner & { loc: AdminLocation }) {
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const languages = choices(offered);
  const query = loc.query.get("q") ?? "";
  const language = loc.query.get("language") ?? "";
  const status = loc.query.get("status") === "draft" ? "draft" : loc.query.get("status") === "live" ? "live" : "";
  const deleted = loc.query.get("deleted") === "1";
  const page = pageOf(loc.query);
  const filters = { q: query, language, status, deleted: deleted ? "1" : undefined };
  const filter = (next: Partial<typeof filters>) => loc.go("", { ...filters, ...next });
  const [text, setText] = useState(query);
  useEffect(() => setText(query), [query]);
  const { store, scope } = useContentScope(client);
  const list = usePaged(store, keyOf("posts-page", scope, query, language, status, deleted, pageSize), { type: "posts", scope: deleted ? "deleted" : undefined }, page, pageSize, (q, signal) =>
    client.posts.adminList(
      { ...q, q: query || undefined, language: language || undefined, draft: deleted || !status ? undefined : status === "draft", deleted: deleted || undefined },
      signal,
    ),
  );
  const del = useAction("post.save", onError);
  const { confirm, dialog } = useConfirm();
  const remove = async (post: Post) => {
    if (await confirm({ title: t("postManager.deleteTitle", { title: post.title }), description: t("postManager.deleteBody"), confirm: t("postManager.delete") })) {
      await del.run(() => client.posts.delete(post.id));
    }
  };
  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold">{t("postManager.title")}</h2>
        <Button size="sm" onClick={() => loc.go("new")}>
          {t("postManager.new")}
        </Button>
      </div>
      <div className="flex gap-2" role="group" aria-label={t("postManager.title")}>
        <Button size="sm" variant={deleted ? "outline" : "default"} aria-pressed={!deleted} onClick={() => loc.go("", { q: query, language })}>
          {t("postManager.posts")}
        </Button>
        <Button size="sm" variant={deleted ? "default" : "outline"} aria-pressed={deleted} onClick={() => loc.go("", { q: query, language, deleted: "1" })}>
          {t("postManager.deleted")}
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
            {t("postManager.search")}
          </Label>
          <Input id={`${ids}-q`} className="h-8" type="search" value={text} placeholder={t("postManager.search")} onChange={(e) => setText(e.target.value)} />
          <Button type="submit" size="sm" variant="outline">
            {t("moderation.filter")}
          </Button>
        </form>
        <div className="flex flex-wrap gap-2">
          {languages.length > 0 && (
            <Select value={language} onValueChange={(v) => filter({ language: String(v ?? "") })}>
              <SelectTrigger size="sm" aria-label={t("postManager.language")}>
                <SelectValue>{(v: string) => (v ? (languages.find((l) => l.value === v)?.label ?? v) : t("pollManager.allLanguages"))}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="">{t("pollManager.allLanguages")}</SelectItem>
                {languages.map((l) => (
                  <SelectItem key={l.value} value={l.value}>
                    {l.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          {!deleted && (
            <Select value={status} onValueChange={(v) => filter({ status: String(v ?? "") })}>
              <SelectTrigger size="sm" aria-label={t("postManager.status")}>
                <SelectValue>{(v: string) => t(v === "draft" ? "postManager.drafts" : v === "live" ? "postManager.notDrafts" : "postManager.allStatuses")}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="">{t("postManager.allStatuses")}</SelectItem>
                <SelectItem value="draft">{t("postManager.drafts")}</SelectItem>
                <SelectItem value="live">{t("postManager.notDrafts")}</SelectItem>
              </SelectContent>
            </Select>
          )}
        </div>
      </div>
      <ErrorLine error={del.error} />
      {list.loading && !list.items.length ? (
        <Rows />
      ) : list.error && !list.items.length ? (
        <LoadFailed error={list.error} message={t("postManager.loadFailed")} retry={list.reload} />
      ) : !list.items.length ? (
        <p className="text-muted-foreground">{t(deleted ? "postManager.noDeleted" : "postManager.empty")}</p>
      ) : (
        <ul className="grid divide-y divide-border rounded-lg border border-border" aria-busy={list.loading}>
          {list.items.map((post) => {
            const s = postStatus(post);
            const href = s === "published" ? postHref?.(post) : undefined;
            return (
              <li key={post.id} className="flex flex-wrap items-start gap-3 p-3" data-ckui="post-row" data-status={deleted ? "deleted" : s}>
                <div className="grid min-w-0 flex-1 gap-1">
                  {deleted ? (
                    <span className="font-medium break-words">{post.title}</span>
                  ) : (
                    <button type="button" className="text-left font-medium break-words hover:underline" onClick={() => loc.go(post.id)}>
                      {post.title}
                    </button>
                  )}
                  <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
                    {!deleted && <Badge variant={badge[s]}>{t(`postManager.${s}`)}</Badge>}
                    <span>{post.language}</span>
                    <RelativeTime at={deleted && post.deleted_at ? post.deleted_at : post.updated_at} />
                    {s === "scheduled" && post.live_at && <span>{t("pollManager.goesLive", { date: formatDate(m, post.live_at) })}</span>}
                  </div>
                </div>
                <div className="flex flex-wrap gap-2">
                  {deleted ? (
                    <Button size="sm" variant="outline" disabled={del.pending} onClick={() => void del.run(() => client.posts.restore(post.id))}>
                      {t("moderation.restore")}
                    </Button>
                  ) : (
                    <>
                      {href && (
                        <a className={buttonVariants({ size: "sm", variant: "ghost" })} href={href}>
                          {t("postManager.view")}
                        </a>
                      )}
                      <Button size="sm" variant="outline" onClick={() => loc.go(post.id)}>
                        {t("comments.edit")}
                      </Button>
                      <Button size="sm" variant="destructive" disabled={del.pending} onClick={() => void remove(post)}>
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
  slug: string;
  language: string;
  excerpt: string;
  liveAt: string;
  body: string;
}

const fromPost = (p: Post): Form => ({
  title: p.title,
  slug: p.slug ?? "",
  language: p.language,
  excerpt: p.excerpt ?? "",
  liveAt: p.live_at ? localInput(new Date(p.live_at)) : "",
  body: p.body,
});

function PostForm({ id, loc, client, languages: offered, renderEditor, postHref, onError }: Inner & { id: string | null; loc: AdminLocation }) {
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const languages = choices(offered);
  const ed = usePost(id, { client });
  const report = useErrorReporter(onError);
  // The body is kept as stored (image references); the editor shows it with URLs.
  const [form, setForm] = useState<Form | null>(id ? null : { title: "", slug: "", language: languages[0]?.value ?? "en", excerpt: "", liveAt: "", body: "" });
  const [error, setError] = useState<ContentKitError | string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const { confirm, dialog } = useConfirm();
  useEffect(() => {
    if (ed.post) setForm((f) => f ?? fromPost(ed.post!));
  }, [ed.post]);
  const { imageSrc } = ed;
  const shown = useMemo(
    () => (form?.body ?? "").replace(imageRefs(), (ref, name: string) => {
      const src = imageSrc(name);
      return src && src !== name ? src.replaceAll("&", "&amp;") : ref;
    }),
    [form?.body, imageSrc],
  );
  if (id && !form) {
    if (ed.error) return <LoadFailed error={ed.error} message={t("postManager.loadFailed")} retry={() => loc.go(id, undefined, { replace: true })} />;
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
    report(err, "post.save");
  };
  const input = (draft?: boolean): PostInput => ({
    title: f.title.trim(),
    slug: f.slug.trim() || undefined,
    language: f.language,
    excerpt: f.excerpt.trim(),
    body: f.body,
    live_at: f.liveAt ? new Date(f.liveAt).toISOString() : undefined,
    ...(draft === undefined ? {} : { is_draft: draft }),
  });
  const save = async (draft?: boolean) => {
    setError(null);
    setNotice(null);
    if (!f.title.trim()) return setError(t("postManager.titleRequired"));
    try {
      if (!ed.post) {
        const post = await ed.create({ ...input(), is_draft: true });
        loc.go(post.id, undefined, { replace: true });
        return;
      }
      const post = await ed.update(input(draft));
      setNotice(t(draft === false ? "postManager.publishedNotice" : draft === true ? "postManager.unpublishedNotice" : "pollEditor.saved"));
      setForm((cur) => ({ ...cur!, slug: post.slug ?? "" }));
    } catch (e) {
      fail(e);
    }
  };
  const remove = async () => {
    if (!ed.post || !(await confirm({ title: t("postManager.deleteTitle", { title: ed.post.title }), description: t("postManager.deleteBody"), confirm: t("postManager.delete") }))) return;
    try {
      await ed.remove();
      loc.go("", undefined, { replace: true });
    } catch (e) {
      fail(e);
    }
  };
  const post = ed.post;
  const s = post ? postStatus(post) : "draft";
  const href = post && s === "published" ? postHref?.(post) : undefined;
  const editor: PostEditorProps = {
    value: shown,
    onChange: (html) => set({ body: ed.storedBody(html) }),
    uploadImage: post ? (file) => ed.uploadImage(file) : undefined,
    disabled: ed.saving,
    placeholder: t("postManager.bodyPlaceholder"),
    "aria-label": t("postManager.body"),
  };
  return (
    <form
      className="grid gap-4"
      data-ckui="post-form"
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="text-base font-semibold">{t(post ? "postManager.editTitle" : "postManager.newTitle")}</h2>
        {post && <Badge variant={badge[s]}>{t(`postManager.${s}`)}</Badge>}
        {href && (
          <a className="text-xs text-muted-foreground hover:underline" href={href}>
            {t("postManager.view")}
          </a>
        )}
      </div>
      {post?.moderation && (
        <Alert variant={post.moderation === "rejected" ? "destructive" : "default"}>
          <AlertDescription>
            {post.moderation_reason ? t(`postManager.${post.moderation === "rejected" ? "rejectedBecause" : "heldBecause"}`, { reason: post.moderation_reason }) : t(`postManager.${post.moderation === "rejected" ? "rejectedNote" : "heldNote"}`)}
          </AlertDescription>
        </Alert>
      )}
      <div className="grid gap-1.5">
        <Label htmlFor={`${ids}-title`}>{t("postManager.postTitle")}</Label>
        <Input id={`${ids}-title`} value={f.title} required maxLength={300} onChange={(e) => set({ title: e.target.value })} />
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-slug`}>{t("postManager.slug")}</Label>
          <Input id={`${ids}-slug`} value={f.slug} placeholder={t("postManager.slugAuto")} onChange={(e) => set({ slug: e.target.value })} />
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-lang`}>{t("postManager.language")}</Label>
          {languages.length ? (
            <Select value={f.language} onValueChange={(v) => set({ language: String(v ?? "") })}>
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
          ) : (
            <Input id={`${ids}-lang`} value={f.language} onChange={(e) => set({ language: e.target.value })} />
          )}
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor={`${ids}-live`}>{t("postManager.liveAt")}</Label>
          <Input id={`${ids}-live`} type="datetime-local" value={f.liveAt} onChange={(e) => set({ liveAt: e.target.value })} />
        </div>
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor={`${ids}-excerpt`}>{t("postManager.excerpt")}</Label>
        <Textarea id={`${ids}-excerpt`} rows={2} value={f.excerpt} onChange={(e) => set({ excerpt: e.target.value })} />
      </div>
      <Cover post={post} src={ed.coverSrc} saving={ed.saving} onFile={(file) => void ed.setCover(file).catch(fail)} />
      <div className="grid gap-1.5">
        <span className="text-sm font-medium">{t("postManager.body")}</span>
        {renderEditor ? renderEditor(editor) : <Textarea aria-label={editor["aria-label"]} rows={12} value={f.body} placeholder={editor.placeholder} onChange={(e) => set({ body: e.target.value })} />}
        {!post && <p className="text-xs text-muted-foreground">{t("postManager.imagesAfterCreate")}</p>}
      </div>
      {error && <ErrorLine>{typeof error === "string" ? error : contentError(m, error)}</ErrorLine>}
      {notice && !error && (
        <p role="status" className="text-sm text-muted-foreground">
          {notice}
        </p>
      )}
      <div className="flex flex-wrap gap-2 border-t border-border pt-4">
        <Button type="submit" disabled={ed.saving}>
          {ed.saving ? t("common.saving") : t(post ? "common.save" : "postManager.createDraft")}
        </Button>
        {post && (
          <Button type="button" variant="outline" disabled={ed.saving} onClick={() => void save(!post.is_draft)}>
            {t(post.is_draft ? "postManager.publish" : "postManager.unpublish")}
          </Button>
        )}
        {post && (
          <Button type="button" variant="destructive" className="ms-auto" disabled={ed.saving} onClick={() => void remove()}>
            {t("postManager.delete")}
          </Button>
        )}
      </div>
      {dialog}
    </form>
  );
}

function Cover({ post, src, saving, onFile }: { post: Post | null; src?: string; saving: boolean; onFile: (file: File | null) => void }) {
  const { t } = useMessages();
  const file = useRef<HTMLInputElement>(null);
  return (
    <div className="grid gap-1.5" data-ckui="post-cover">
      <span className="text-sm font-medium">{t("postManager.cover")}</span>
      {post ? (
        <div className="flex flex-wrap items-center gap-3">
          {src ? <img src={src} alt="" className="h-20 w-36 rounded-md border border-border object-cover" /> : <span className="text-muted-foreground">{t("postManager.noCover")}</span>}
          <Button type="button" size="sm" variant="outline" disabled={saving} onClick={() => file.current?.click()}>
            {t(src ? "pollEditor.replaceImage" : "postManager.addCover")}
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
            aria-label={t("postManager.cover")}
            onChange={(e) => {
              const f = e.target.files?.[0];
              e.target.value = "";
              if (f) onFile(f);
            }}
          />
        </div>
      ) : (
        <p className="text-xs text-muted-foreground">{t("postManager.imagesAfterCreate")}</p>
      )}
    </div>
  );
}
