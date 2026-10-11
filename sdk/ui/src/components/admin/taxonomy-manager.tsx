import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import type { ContentKitClient } from "../../client/client.js";
import { toContentKitError, type ContentKitError } from "../../client/errors.js";
import type { Assignment, AssignmentPage, ContentRef, Edge, MergeReport, Name, NodeDetail, NodePage, NodeRow } from "../../client/generated/wire.js";
import { useMessages } from "../../i18n/context.js";
import { useAdminLocation, useFlowClient, useStaffAccess, type AdminLocation, type FlowClientOptions, type StaffAccess } from "../../react/admin.js";
import { useErrorReporter } from "../../react/context.js";
import { usePublicImage } from "../../react/public.js";
import { keyOf, useContentScope, useResource } from "../../react/use-resource.js";
import { Badge } from "#ckui/ui/badge";
import { Button } from "#ckui/ui/button";
import { Input } from "#ckui/ui/input";
import { Label } from "#ckui/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "#ckui/ui/select";
import { Skeleton } from "#ckui/ui/skeleton";
import { Tabs, TabsList, TabsTrigger } from "#ckui/ui/tabs";
import { CoverUpload } from "../slot-upload.js";
import { contentError, ErrorLine, RelativeTime, useAction } from "../social/parts.js";
import { AdminRoot, LoadFailed, NoAccess, Pager, Rows, pageOf, useConfirm, type AdminFlowProps, type AdminRouteProps } from "./shell.js";

type Choice = string | { value: string; label: string };
export type TaxonomyRelation = Edge["relation"];

/** One node kind the flow manages. */
export interface TaxonomyKind {
  /** The kind as the store registers it, e.g. "tag", "series". */
  kind: string;
  label: string;
  /** Edges a node of this kind may have, "node is <relation> of a <to> node": e.g. a character member_of a series. */
  relations?: readonly { relation: TaxonomyRelation; to: string; label: string; inverseLabel?: string }[];
  /** A cover in the host's media: the media kind whose items are node ids, its upload path ("cover") and public preset ("cover"). */
  cover?: { kind: string; path?: string; preset?: string };
}

export interface TaxonomyManagerProps extends AdminFlowProps, AdminRouteProps {
  /** The kinds, in tab order; the first is at basePath. */
  kinds: readonly TaxonomyKind[];
  /** Languages names are written in; the first resolves the list's names. */
  languages?: readonly Choice[];
  /** Counts of this content kind; default every kind. */
  contentKind?: string;
  /** Host-owned fields of a node (a creator's type, a tag's restricted flag), below its names. */
  renderFields?: (node: NodeDetail) => ReactNode;
  /** One assigned content, e.g. a link with its title. Default its kind and id. */
  renderContent?: (ref: ContentRef, assignment: Assignment) => ReactNode;
  pageSize?: number;
}

/** Whether the caller may manage the taxonomy (root:taxonomy:write). */
export function useTaxonomyManagerAccess(o: FlowClientOptions = {}): StaffAccess<"taxonomy"> {
  return useStaffAccess(["taxonomy"] as const, o);
}

const choices = (list?: readonly Choice[]) => (list ?? []).map((c) => (typeof c === "string" ? { value: c, label: c } : c));
const SORTS = ["name", "count", "created", "updated", "oldest"] as const;
const STATES = ["active", "deleted", "merged"] as const;
type NodeState = (typeof STATES)[number];

/** A slug from a name: lowercase words joined by dashes. */
export function slugify(name: string): string {
  return name
    .normalize("NFKD")
    .replace(/[̀-ͯ]/g, "")
    .toLowerCase()
    .trim()
    .replace(/[\s/]+/g, "-")
    .replace(/[^\p{L}\p{N}-]+/gu, "")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, 200);
}

/**
 * Staff: the taxonomy per kind. Each kind's nodes by name, state, language,
 * minimum count and order; a node's names and aliases per language, host
 * fields, relations (with a typeahead), cover, counts and assigned contents;
 * create, delete, restore and merge. Kinds are at basePath/{kind}, a node at
 * basePath/{kind}/{id}, filters in the query.
 */
export function TaxonomyManager({ client: own, baseUrl, auth, guard, appearance, className, language, messages, ...p }: TaxonomyManagerProps) {
  const client = useFlowClient({ client: own, baseUrl, auth });
  return (
    <AdminRoot flow="taxonomy-manager" appearance={appearance} className={className} language={language} messages={messages} guard={guard}>
      <Taxonomy {...p} client={client} />
    </AdminRoot>
  );
}

type Inner = Omit<TaxonomyManagerProps, "client" | "baseUrl" | "auth" | "guard" | "appearance" | "className" | "language" | "messages"> & { client: ContentKitClient };
type Ctx = Inner & { loc: AdminLocation; kind: TaxonomyKind; lang: string };

function Taxonomy({ basePath, navigation, ...p }: Inner) {
  const { t } = useMessages();
  const loc = useAdminLocation(basePath, navigation);
  const access = useTaxonomyManagerAccess({ client: p.client });
  if (access.loading) return <Rows />;
  if (!access.allowed.taxonomy) return <NoAccess>{t("taxonomyManager.noAccess")}</NoAccess>;
  if (!p.kinds.length) return null;
  const [k, sub] = loc.view.split("/");
  const kind = p.kinds.find((x) => x.kind === k) ?? p.kinds[0]!;
  const ctx: Ctx = { ...p, loc, kind, lang: choices(p.languages)[0]?.value ?? "en" };
  return (
    <>
      {p.kinds.length > 1 && (
        <Tabs value={kind.kind} onValueChange={(v) => loc.go(String(v))}>
          <TabsList>
            {p.kinds.map((x) => (
              <TabsTrigger key={x.kind} value={x.kind}>
                {x.label}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>
      )}
      {!sub ? <NodeList key={kind.kind} {...ctx} /> : sub === "new" ? <NodeCreate key={kind.kind} {...ctx} /> : <NodeView key={sub} {...ctx} id={sub} />}
    </>
  );
}

function NodeList({ loc, kind, client, languages: offered, contentKind, pageSize = 25, lang }: Ctx) {
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const languages = choices(offered);
  const query = loc.query.get("q") ?? "";
  const stateParam = loc.query.get("state") ?? "";
  const state: NodeState = (STATES as readonly string[]).includes(stateParam) ? (stateParam as NodeState) : "active";
  const language = loc.query.get("language") || lang;
  const minCount = Math.max(0, Number(loc.query.get("min_count")) || 0);
  const sortParam = loc.query.get("sort") ?? "";
  const sort = (SORTS as readonly string[]).includes(sortParam) ? sortParam : "name";
  const page = pageOf(loc.query);
  const filters: { q?: string; state?: NodeState; language?: string; min_count?: number; sort?: string } = {
    q: query,
    state: state === "active" ? undefined : state,
    language: language === lang ? undefined : language,
    min_count: minCount || undefined,
    sort: sort === "name" ? undefined : sort,
  };
  const filter = (next: Partial<typeof filters>) => loc.go(kind.kind, { ...filters, ...next });
  const [text, setText] = useState(query);
  useEffect(() => setText(query), [query]);
  const { store, scope } = useContentScope(client);
  const offset = (page - 1) * pageSize;
  const r = useResource<NodePage>(store, keyOf("taxonomy-nodes", scope, kind.kind, query, state, language, minCount, sort, contentKind, offset, pageSize), { type: "taxonomy-nodes" }, (signal) =>
    client.taxonomy.nodes(
      { kind: kind.kind, q: query || undefined, state: [state], language, min_count: minCount || undefined, sort, content_kind: contentKind, offset, limit: pageSize },
      signal,
    ),
  );
  const nodes = r.data?.nodes ?? [];
  const total = r.data?.total ?? 0;
  const loading = r.loading || !r.loaded;
  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold">{kind.label}</h2>
        <Button size="sm" onClick={() => loc.go(`${kind.kind}/new`)}>
          {t("taxonomyManager.new")}
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
            {t("taxonomyManager.search")}
          </Label>
          <Input id={`${ids}-q`} className="h-8" type="search" value={text} placeholder={t("taxonomyManager.search")} onChange={(e) => setText(e.target.value)} />
          <Button type="submit" size="sm" variant="outline">
            {t("moderation.filter")}
          </Button>
        </form>
        <div className="flex flex-wrap gap-2">
          <Select value={state} onValueChange={(v) => filter({ state: v === "active" ? undefined : (String(v) as NodeState) })}>
            <SelectTrigger size="sm" aria-label={t("taxonomyManager.state")}>
              <SelectValue>{(v: string) => t(`taxonomyManager.${v as NodeState}`)}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {STATES.map((s) => (
                <SelectItem key={s} value={s}>
                  {t(`taxonomyManager.${s}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          {languages.length > 1 && (
            <Select value={language} onValueChange={(v) => filter({ language: v === lang ? undefined : String(v) })}>
              <SelectTrigger size="sm" aria-label={t("taxonomyManager.language")}>
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
          )}
          <Select value={sort} onValueChange={(v) => filter({ sort: v === "name" ? undefined : String(v) })}>
            <SelectTrigger size="sm" aria-label={t("taxonomyManager.sort")}>
              <SelectValue>{(v: string) => t(`taxonomyManager.sort_${v as (typeof SORTS)[number]}`)}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {SORTS.map((s) => (
                <SelectItem key={s} value={s}>
                  {t(`taxonomyManager.sort_${s}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <div className="flex items-center gap-1.5">
            <Label htmlFor={`${ids}-min`} className="text-xs text-muted-foreground">
              {t("taxonomyManager.minCount")}
            </Label>
            <Input
              id={`${ids}-min`}
              className="h-8 w-20"
              type="number"
              min={0}
              value={minCount || ""}
              onChange={(e) => filter({ min_count: Number(e.target.value) || undefined })}
            />
          </div>
        </div>
      </div>
      {loading && !nodes.length ? (
        <Rows />
      ) : r.error && !nodes.length ? (
        <LoadFailed error={r.error} message={t("taxonomyManager.loadFailed")} retry={r.reload} />
      ) : !nodes.length ? (
        <p className="text-muted-foreground">{t("taxonomyManager.empty")}</p>
      ) : (
        <ul className="grid divide-y divide-border rounded-lg border border-border" aria-busy={loading}>
          {nodes.map((n) => (
            <li key={n.taxonomy_id} className="flex flex-wrap items-center gap-3 p-3" data-ckui="node-row" data-state={n.state}>
              <div className="grid min-w-0 flex-1 gap-0.5">
                <button type="button" className="text-left font-medium break-words hover:underline" onClick={() => loc.go(`${kind.kind}/${n.taxonomy_id}`)}>
                  {n.name || n.slug}
                </button>
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
                  <span>{n.slug}</span>
                  {n.state !== "active" && <Badge variant="outline">{t(`taxonomyManager.${n.state}`)}</Badge>}
                  <RelativeTime at={n.updated_at} />
                </div>
              </div>
              <span className="text-sm tabular-nums text-muted-foreground" title={t("taxonomyManager.count")}>
                {n.count}
              </span>
            </li>
          ))}
        </ul>
      )}
      {total > 0 && <p className="text-center text-xs text-muted-foreground">{m.plural("taxonomyManager.total", total)}</p>}
      <Pager page={page} hasNext={offset + nodes.length < total} disabled={loading} onPage={(n) => loc.go(kind.kind, { ...filters, page: n > 1 ? n : undefined })} />
    </div>
  );
}

function NodeCreate({ loc, kind, client, languages: offered, lang, onError }: Ctx) {
  const m = useMessages();
  const { t } = m;
  const ids = useId();
  const languages = choices(offered);
  const [names, setNames] = useState<Record<string, string>>({});
  const [slug, setSlug] = useState("");
  const [slugEdited, setSlugEdited] = useState(false);
  const create = useAction("taxonomy.save", onError);
  const first = names[lang] ?? "";
  useEffect(() => {
    if (!slugEdited) setSlug(slugify(first));
  }, [first, slugEdited]);
  const list = languages.length ? languages : [{ value: lang, label: lang }];
  const submit = async () => {
    const given: Name[] = list.filter((l) => names[l.value]?.trim()).map((l) => ({ language: l.value, kind: "name", name: names[l.value]!.trim(), source_revision: 0 }));
    const made = await create.run(() => client.taxonomy.createNodes([{ kind: kind.kind, slug: slug.trim(), names: given, source_revision: 0 }]));
    if (made?.[0]) loc.go(`${kind.kind}/${made[0].taxonomy_id}`, undefined, { replace: true });
  };
  return (
    <form
      className="grid gap-4"
      data-ckui="node-create"
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <Button type="button" variant="ghost" size="sm" className="justify-self-start" onClick={() => loc.go(kind.kind)}>
        {t("taxonomyManager.back", { kind: kind.label })}
      </Button>
      <h2 className="text-base font-semibold">{t("taxonomyManager.newTitle", { kind: kind.label })}</h2>
      {list.map((l, i) => (
        <div key={l.value} className="grid gap-1.5">
          <Label htmlFor={`${ids}-${l.value}`}>{t("taxonomyManager.nameIn", { language: l.label })}</Label>
          <Input id={`${ids}-${l.value}`} required={i === 0} value={names[l.value] ?? ""} onChange={(e) => setNames((n) => ({ ...n, [l.value]: e.target.value }))} />
        </div>
      ))}
      <div className="grid gap-1.5">
        <Label htmlFor={`${ids}-slug`}>{t("taxonomyManager.slug")}</Label>
        <Input
          id={`${ids}-slug`}
          required
          value={slug}
          onChange={(e) => {
            setSlugEdited(true);
            setSlug(e.target.value);
          }}
        />
      </div>
      {create.error && <ErrorLine>{contentError(m, create.error)}</ErrorLine>}
      <Button type="submit" className="justify-self-start" disabled={create.pending || !first.trim() || !slug.trim()}>
        {create.pending ? t("common.saving") : t("taxonomyManager.create")}
      </Button>
    </form>
  );
}

function NodeView({ id, loc, kind, client, lang, ...p }: Ctx & { id: string }) {
  const m = useMessages();
  const { t } = m;
  const { store, scope } = useContentScope(client);
  const r = useResource<NodeDetail>(store, keyOf("taxonomy-node", scope, id), { type: "taxonomy-node", id }, (signal) => client.taxonomy.node(id, signal));
  const node = r.data;
  const partnerIds = useMemo(() => [...new Set((node?.edges ?? []).flatMap((e) => [e.from_taxonomy_id, e.to_taxonomy_id]).filter((x) => x !== id))].sort(), [node?.edges, id]);
  const partners = useResource<NodePage>(
    store,
    partnerIds.length ? keyOf("taxonomy-partners", scope, lang, ...partnerIds) : null,
    { type: "taxonomy-partners" },
    (signal) => client.taxonomy.nodes({ id: partnerIds, state: ["active", "merged", "deleted"], language: lang, limit: Math.min(500, partnerIds.length) }, signal),
  );
  const byId = useMemo(() => new Map((partners.data?.nodes ?? []).map((n) => [n.taxonomy_id, n])), [partners.data]);
  const save = useAction("taxonomy.save", p.onError);
  const { confirm, dialog } = useConfirm();
  const back = (
    <Button variant="ghost" size="sm" className="justify-self-start" onClick={() => loc.go(kind.kind)}>
      {t("taxonomyManager.back", { kind: kind.label })}
    </Button>
  );
  if (!node) {
    return (
      <>
        {back}
        {r.error ? <LoadFailed error={r.error} message={t("taxonomyManager.nodeLoadFailed")} retry={r.reload} /> : <Skeleton className="h-40 w-full" />}
      </>
    );
  }
  const canonical = node.names.find((n) => n.kind === "name" && n.language === lang) ?? node.names.find((n) => n.kind === "name");
  const title = canonical?.name ?? node.slug;
  const setState = async (state: "active" | "deleted") => {
    if (state === "deleted" && !(await confirm({ title: t("taxonomyManager.deleteTitle", { name: title }), description: t("taxonomyManager.deleteBody"), confirm: t("taxonomyManager.delete") }))) return;
    await save.run(() => client.taxonomy.updateNode(id, { state }));
  };
  return (
    <div className="grid gap-5" data-ckui="node-view" data-state={node.state}>
      {back}
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="text-base font-semibold break-words">{title}</h2>
        <Badge variant={node.state === "active" ? "outline" : "secondary"}>{t(`taxonomyManager.${node.state}`)}</Badge>
        <div className="ms-auto flex flex-wrap gap-2">
          {node.state === "active" && <MergeButton node={node} title={title} kind={kind} client={client} loc={loc} lang={lang} onError={p.onError} />}
          {node.state === "active" ? (
            <Button size="sm" variant="destructive" disabled={save.pending} onClick={() => void setState("deleted")}>
              {t("taxonomyManager.delete")}
            </Button>
          ) : node.state === "deleted" ? (
            <Button size="sm" variant="outline" disabled={save.pending} onClick={() => void setState("active")}>
              {t("taxonomyManager.restore")}
            </Button>
          ) : null}
        </div>
      </div>
      {node.state === "merged" && <MergedInto node={node} byId={byId} kind={kind} loc={loc} />}
      <ErrorLine error={save.error} />
      <SlugField node={node} client={client} onError={p.onError} />
      <Names node={node} client={client} languages={p.languages} lang={lang} onError={p.onError} />
      {p.renderFields && <section data-ckui="node-fields">{p.renderFields(node)}</section>}
      {kind.cover && <Cover node={node} cover={kind.cover} client={client} />}
      <Relations node={node} kind={kind} kinds={p.kinds} byId={byId} client={client} loc={loc} lang={lang} onError={p.onError} />
      <Counts node={node} />
      <Contents id={id} client={client} renderContent={p.renderContent} />
      {dialog}
    </div>
  );
}

function Section({ title, children, ckui }: { title: ReactNode; children: ReactNode; ckui: string }) {
  return (
    <section className="grid gap-2" data-ckui={ckui}>
      <h3 className="font-medium">{title}</h3>
      {children}
    </section>
  );
}

function SlugField({ node, client, onError }: { node: NodeDetail; client: ContentKitClient; onError?: Inner["onError"] }) {
  const { t } = useMessages();
  const ids = useId();
  const [slug, setSlug] = useState(node.slug);
  useEffect(() => setSlug(node.slug), [node.slug]);
  const save = useAction("taxonomy.save", onError);
  return (
    <form
      className="flex flex-wrap items-end gap-2"
      onSubmit={(e) => {
        e.preventDefault();
        void save.run(() => client.taxonomy.updateNode(node.taxonomy_id, { slug: slug.trim() }));
      }}
    >
      <div className="grid min-w-48 flex-1 gap-1.5">
        <Label htmlFor={`${ids}-slug`}>{t("taxonomyManager.slug")}</Label>
        <Input id={`${ids}-slug`} className="h-8" value={slug} onChange={(e) => setSlug(e.target.value)} />
      </div>
      <Button type="submit" size="sm" variant="outline" disabled={save.pending || !slug.trim() || slug.trim() === node.slug}>
        {t("common.save")}
      </Button>
      <ErrorLine error={save.error} className="basis-full" />
    </form>
  );
}

function Names({ node, client, languages: offered, lang, onError }: { node: NodeDetail; client: ContentKitClient; languages?: readonly Choice[]; lang: string; onError?: Inner["onError"] }) {
  const { t } = useMessages();
  const ids = useId();
  const languages = choices(offered);
  const list = languages.length ? languages : [{ value: lang, label: lang }];
  const [language, setLanguage] = useState(list[0]!.value);
  const [kind, setKind] = useState<Name["kind"]>("alias");
  const [text, setText] = useState("");
  const save = useAction("taxonomy.save", onError);
  const id = node.taxonomy_id;
  const label = (code: string) => list.find((l) => l.value === code)?.label ?? code;
  const sorted = [...node.names].sort((a, b) => a.language.localeCompare(b.language) || (a.kind === b.kind ? a.name.localeCompare(b.name) : a.kind === "name" ? -1 : 1));
  return (
    <Section title={t("taxonomyManager.names")} ckui="node-names">
      {sorted.length ? (
        <ul className="grid divide-y divide-border rounded-lg border border-border">
          {sorted.map((n) => (
            <li key={`${n.language}:${n.name}`} className="flex flex-wrap items-center gap-2 p-2.5" data-ckui="node-name" data-kind={n.kind}>
              <Badge variant="outline">{label(n.language)}</Badge>
              <span className="min-w-0 flex-1 break-words">{n.name}</span>
              <span className="text-xs text-muted-foreground">{t(n.kind === "name" ? "taxonomyManager.canonical" : "taxonomyManager.alias")}</span>
              {n.kind === "alias" && (
                <Button size="xs" variant="ghost" disabled={save.pending} onClick={() => void save.run(() => client.taxonomy.names(id, "add", [{ ...n, kind: "name" }]))}>
                  {t("taxonomyManager.makeCanonical")}
                </Button>
              )}
              <Button size="xs" variant="ghost" disabled={save.pending} aria-label={t("taxonomyManager.removeName", { name: n.name })} onClick={() => void save.run(() => client.taxonomy.names(id, "remove", [n]))}>
                {t("common.remove")}
              </Button>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-muted-foreground">{t("taxonomyManager.noNames")}</p>
      )}
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          const name = text.trim();
          if (!name) return;
          void save.run(() => client.taxonomy.names(id, "add", [{ language, kind, name, source_revision: 0 }])).then((d) => d && setText(""));
        }}
      >
        {list.length > 1 && (
          <Select value={language} onValueChange={(v) => setLanguage(String(v))}>
            <SelectTrigger size="sm" aria-label={t("taxonomyManager.language")}>
              <SelectValue>{(v: string) => label(v)}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              {list.map((l) => (
                <SelectItem key={l.value} value={l.value}>
                  {l.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
        <Select value={kind} onValueChange={(v) => setKind(v as Name["kind"])}>
          <SelectTrigger size="sm" aria-label={t("taxonomyManager.nameKind")}>
            <SelectValue>{(v: string) => t(v === "name" ? "taxonomyManager.canonical" : "taxonomyManager.alias")}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="alias">{t("taxonomyManager.alias")}</SelectItem>
            <SelectItem value="name">{t("taxonomyManager.canonical")}</SelectItem>
          </SelectContent>
        </Select>
        <Label htmlFor={`${ids}-name`} className="sr-only">
          {t("taxonomyManager.newName")}
        </Label>
        <Input id={`${ids}-name`} className="h-8 min-w-40 flex-1" value={text} placeholder={t("taxonomyManager.newName")} onChange={(e) => setText(e.target.value)} />
        <Button type="submit" size="sm" variant="outline" disabled={save.pending || !text.trim()}>
          {t("taxonomyManager.addName")}
        </Button>
      </form>
      <ErrorLine error={save.error} />
    </Section>
  );
}

function Cover({ node, cover, client }: { node: NodeDetail; cover: NonNullable<TaxonomyKind["cover"]>; client: ContentKitClient }) {
  const { t } = useMessages();
  const item = { kind: cover.kind, id: node.taxonomy_id };
  const { image } = usePublicImage(cover.kind, node.taxonomy_id, cover.preset ?? "cover", { client });
  return (
    <section className="grid max-w-xl gap-2" data-ckui="node-cover">
      <CoverUpload item={item} path={cover.path ?? "cover"} image={image} client={client} removable label={t("taxonomyManager.cover")} />
    </section>
  );
}

function MergedInto({ node, byId, kind, loc }: { node: NodeDetail; byId: Map<string, NodeRow>; kind: TaxonomyKind; loc: AdminLocation }) {
  const { t } = useMessages();
  const into = node.edges.find((e) => e.relation === "alias_of" && e.from_taxonomy_id === node.taxonomy_id)?.to_taxonomy_id;
  if (!into) return null;
  const target = byId.get(into);
  return (
    <p className="text-muted-foreground" data-ckui="merged-into">
      {t("taxonomyManager.mergedInto")}{" "}
      <button type="button" className="font-medium text-foreground hover:underline" onClick={() => loc.go(`${target?.kind ?? kind.kind}/${into}`)}>
        {target?.name || target?.slug || into}
      </button>
    </p>
  );
}

function MergeButton({ node, title, kind, client, loc, lang, onError }: { node: NodeDetail; title: string; kind: TaxonomyKind; client: ContentKitClient; loc: AdminLocation; lang: string; onError?: Inner["onError"] }) {
  const { t } = useMessages();
  const [picking, setPicking] = useState(false);
  const merge = useAction("taxonomy.save", onError);
  const { confirm, dialog } = useConfirm();
  const [report, setReport] = useState<MergeReport | null>(null);
  const pick = async (into: NodeRow) => {
    setPicking(false);
    const ok = await confirm({
      title: t("taxonomyManager.mergeTitle", { from: title, into: into.name || into.slug }),
      description: t("taxonomyManager.mergeBody"),
      confirm: t("taxonomyManager.merge"),
      typed: t("taxonomyManager.mergeWord"),
    });
    if (!ok) return;
    const r = await merge.run(() => client.taxonomy.merge(node.taxonomy_id, into.taxonomy_id));
    if (r) {
      setReport(r);
      loc.go(`${kind.kind}/${into.taxonomy_id}`);
    }
  };
  return (
    <>
      {picking ? (
        <div className="flex flex-wrap items-center gap-2" data-ckui="merge-picker">
          <NodePicker kind={kind.kind} exclude={node.taxonomy_id} client={client} lang={lang} label={t("taxonomyManager.mergeInto")} onPick={(n) => void pick(n)} />
          <Button size="sm" variant="ghost" onClick={() => setPicking(false)}>
            {t("common.cancel")}
          </Button>
        </div>
      ) : (
        <Button size="sm" variant="outline" disabled={merge.pending} onClick={() => setPicking(true)}>
          {t("taxonomyManager.merge")}
        </Button>
      )}
      {merge.error && <ErrorLine error={merge.error} className="basis-full" />}
      {report && (
        <p role="status" className="sr-only">
          {t("taxonomyManager.mergeDone", { count: report.assignments_moved })}
        </p>
      )}
      {dialog}
    </>
  );
}

/** A typeahead over one kind's active nodes. */
function NodePicker({ kind, exclude, client, lang, label, onPick }: { kind: string; exclude?: string; client: ContentKitClient; lang: string; label: string; onPick: (n: NodeRow) => void }) {
  const { t } = useMessages();
  const id = useId();
  const [text, setText] = useState("");
  const [results, setResults] = useState<NodeRow[]>([]);
  const [active, setActive] = useState(0);
  const [error, setError] = useState<ContentKitError | null>(null);
  const seq = useRef(0);
  useEffect(() => {
    const q = text.trim();
    const n = ++seq.current;
    if (!q) {
      setResults([]);
      return;
    }
    const ctl = new AbortController();
    const timer = setTimeout(() => {
      client.taxonomy.nodes({ kind, q, language: lang, limit: 8, sort: "name" }, ctl.signal).then(
        (page) => {
          if (n !== seq.current) return;
          setError(null);
          setResults(page.nodes.filter((x) => x.taxonomy_id !== exclude));
          setActive(0);
        },
        (e) => {
          const err = toContentKitError(e);
          if (err.code !== "aborted" && n === seq.current) setError(err);
        },
      );
    }, 200);
    return () => {
      clearTimeout(timer);
      ctl.abort();
    };
  }, [text, kind, lang, exclude, client]);
  const open = results.length > 0;
  return (
    <div className="relative grid min-w-56 flex-1 gap-1" data-ckui="node-picker">
      <Input
        role="combobox"
        aria-label={label}
        aria-expanded={open}
        aria-controls={`${id}-list`}
        aria-activedescendant={open ? `${id}-${active}` : undefined}
        autoComplete="off"
        className="h-8"
        value={text}
        placeholder={t("taxonomyManager.searchNodes")}
        autoFocus
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (!open) return;
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setActive((a) => Math.min(results.length - 1, a + 1));
          } else if (e.key === "ArrowUp") {
            e.preventDefault();
            setActive((a) => Math.max(0, a - 1));
          } else if (e.key === "Enter") {
            e.preventDefault();
            onPick(results[active]!);
          } else if (e.key === "Escape") setResults([]);
        }}
      />
      {open && (
        <ul id={`${id}-list`} role="listbox" aria-label={label} className="absolute top-full z-10 mt-1 grid w-full overflow-hidden rounded-md border border-border bg-popover text-popover-foreground shadow-md">
          {results.map((n, i) => (
            <li
              key={n.taxonomy_id}
              id={`${id}-${i}`}
              role="option"
              aria-selected={i === active}
              className={`cursor-pointer px-3 py-1.5 ${i === active ? "bg-accent text-accent-foreground" : ""}`}
              onMouseEnter={() => setActive(i)}
              onMouseDown={(e) => {
                e.preventDefault();
                onPick(n);
              }}
            >
              {n.name || n.slug} <span className="text-xs text-muted-foreground">{n.slug}</span>
            </li>
          ))}
        </ul>
      )}
      {error && <ErrorLine error={error} />}
    </div>
  );
}

function Relations({
  node,
  kind,
  kinds,
  byId,
  client,
  loc,
  lang,
  onError,
}: {
  node: NodeDetail;
  kind: TaxonomyKind;
  kinds: readonly TaxonomyKind[];
  byId: Map<string, NodeRow>;
  client: ContentKitClient;
  loc: AdminLocation;
  lang: string;
  onError?: Inner["onError"];
}) {
  const { t } = useMessages();
  const save = useAction("taxonomy.save", onError);
  const [adding, setAdding] = useState<number | null>(null);
  const id = node.taxonomy_id;
  const out = node.edges.filter((e) => e.from_taxonomy_id === id && e.relation !== "alias_of");
  const incoming = node.edges.filter((e) => e.to_taxonomy_id === id && e.relation !== "alias_of");
  const own = kind.relations ?? [];
  // Relations other kinds declare toward this one, shown from this side.
  const inverse = kinds.flatMap((k) => (k.relations ?? []).filter((r) => r.to === kind.kind).map((r) => ({ ...r, from: k })));
  const link = (other: string, fallbackKind: string) => {
    const n = byId.get(other);
    return (
      <button type="button" className="font-medium hover:underline" onClick={() => loc.go(`${n?.kind ?? fallbackKind}/${other}`)}>
        {n?.name || n?.slug || other}
      </button>
    );
  };
  if (!own.length && !inverse.length && !out.length && !incoming.length) return null;
  return (
    <Section title={t("taxonomyManager.relations")} ckui="node-relations">
      {own.map((r, i) => {
        const edges = out.filter((e) => e.relation === r.relation && (byId.get(e.to_taxonomy_id)?.kind ?? r.to) === r.to);
        return (
          <div key={`${r.relation}:${r.to}`} className="grid gap-1.5" data-ckui="relation" data-relation={r.relation}>
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-sm text-muted-foreground">{r.label}</span>
              {edges.map((e) => (
                <span key={e.to_taxonomy_id} className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-0.5" data-ckui="edge">
                  {link(e.to_taxonomy_id, r.to)}
                  <button
                    type="button"
                    className="text-muted-foreground hover:text-destructive"
                    aria-label={t("taxonomyManager.removeRelation", { name: byId.get(e.to_taxonomy_id)?.name ?? e.to_taxonomy_id })}
                    disabled={save.pending}
                    onClick={() => void save.run(() => client.taxonomy.edges("remove", [e]))}
                  >
                    ×
                  </button>
                </span>
              ))}
              {adding !== i && (
                <Button size="xs" variant="ghost" onClick={() => setAdding(i)}>
                  {t("taxonomyManager.addRelation", { relation: r.label })}
                </Button>
              )}
            </div>
            {adding === i && (
              <div className="flex flex-wrap items-center gap-2">
                <NodePicker
                  kind={r.to}
                  exclude={id}
                  client={client}
                  lang={lang}
                  label={r.label}
                  onPick={(n) => {
                    setAdding(null);
                    void save.run(() => client.taxonomy.edges("add", [{ from_taxonomy_id: id, relation: r.relation, to_taxonomy_id: n.taxonomy_id, source_revision: 0 }]));
                  }}
                />
                <Button size="sm" variant="ghost" onClick={() => setAdding(null)}>
                  {t("common.cancel")}
                </Button>
              </div>
            )}
          </div>
        );
      })}
      {inverse.map((r) => {
        const edges = incoming.filter((e) => e.relation === r.relation && (byId.get(e.from_taxonomy_id)?.kind ?? r.from.kind) === r.from.kind);
        if (!edges.length) return null;
        return (
          <div key={`in:${r.relation}:${r.from.kind}`} className="flex flex-wrap items-center gap-2" data-ckui="relation" data-relation={`inverse:${r.relation}`}>
            <span className="text-sm text-muted-foreground">{r.inverseLabel ?? r.from.label}</span>
            {edges.map((e) => (
              <span key={e.from_taxonomy_id} className="rounded-md border border-border px-2 py-0.5" data-ckui="edge">
                {link(e.from_taxonomy_id, r.from.kind)}
              </span>
            ))}
          </div>
        );
      })}
      <ErrorLine error={save.error} />
    </Section>
  );
}

function Counts({ node }: { node: NodeDetail }) {
  const { t } = useMessages();
  return (
    <Section title={t("taxonomyManager.counts")} ckui="node-counts">
      {node.counts.length ? (
        <ul className="flex flex-wrap gap-2">
          {node.counts.map((c) => (
            <li key={`${c.content_kind}:${c.language}`} className="rounded-md border border-border px-2 py-1 text-xs">
              {c.content_kind} · {c.language}: <span className="tabular-nums font-medium">{c.count}</span>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-muted-foreground">{t("taxonomyManager.noCounts")}</p>
      )}
    </Section>
  );
}

function Contents({ id, client, renderContent }: { id: string; client: ContentKitClient; renderContent?: Inner["renderContent"] }) {
  const { t } = useMessages();
  const report = useErrorReporter();
  const [pages, setPages] = useState<AssignmentPage[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ContentKitError | null>(null);
  const load = async (cursor?: string) => {
    setLoading(true);
    setError(null);
    try {
      const page = await client.taxonomy.nodeAssignments(id, { cursor, limit: 25 });
      setPages((ps) => (cursor ? [...ps, page] : [page]));
    } catch (e) {
      const err = toContentKitError(e);
      setError(err);
      report(err, "taxonomy.load");
    } finally {
      setLoading(false);
    }
  };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => void load(), [id, client]);
  const items = pages.flatMap((p) => p.assignments);
  const next = pages.at(-1)?.next_cursor;
  return (
    <Section title={t("taxonomyManager.contents")} ckui="node-contents">
      {loading && !items.length ? (
        <Rows count={2} />
      ) : error && !items.length ? (
        <LoadFailed error={error} message={t("taxonomyManager.contentsLoadFailed")} retry={() => void load()} />
      ) : !items.length ? (
        <p className="text-muted-foreground">{t("taxonomyManager.noContents")}</p>
      ) : (
        <ul className="grid divide-y divide-border rounded-lg border border-border">
          {items.map((a) => (
            <li
              key={`${a.content_kind}/${a.content_id}/${a.content_version_id ?? ""}/${a.relation}`}
              className="flex flex-wrap items-center gap-2 p-2.5"
              data-ckui="node-content"
              data-state={a.state}
            >
              <span className="min-w-0 flex-1 break-words">
                {renderContent ? renderContent(a, a) : `${a.content_kind}/${a.content_id}${a.content_version_id ? ` (${a.content_version_id})` : ""}`}
              </span>
              <span className="text-xs text-muted-foreground">{a.relation}</span>
              {a.state === "proposed" && <Badge variant="secondary">{t("taxonomyManager.proposed")}</Badge>}
            </li>
          ))}
        </ul>
      )}
      {next && (
        <Button variant="outline" size="sm" className="justify-self-center" disabled={loading} onClick={() => void load(next)}>
          {t("moderation.loadMore")}
        </Button>
      )}
    </Section>
  );
}
