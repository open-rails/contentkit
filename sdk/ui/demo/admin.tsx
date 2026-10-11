import { CommentModeration, PollManager, PostManager, TaxonomyManager, UserCommentStanding, type AdminNavigation, type TaxonomyKind } from "@openrails/contentkit-ui";
import { richTextEditor } from "@openrails/contentkit-ui/editor";
import { StrictMode, useMemo, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Link, Route, Routes, useLocation, useNavigate, useParams } from "react-router";
import { auth, client, dark, q } from "./session";

// A host's admin area. /admin/comments/* CommentModeration, /admin/users/{id}
// UserCommentStanding, /admin/polls/* PollManager, /admin/posts/* PostManager
// (with the package's rich-text editor), /admin/taxonomy/* TaxonomyManager
// (tags; series with covers; characters, members of series). ?router=1 mounts each flow in React Router through
// `navigation` (with the client); otherwise alone over the history API (with
// baseUrl and auth). ?size= pages, ?lang= the UI language. The flows take the
// host's shadcn theme ("inherit", their default); ?theme=dark adds `.dark`.
const router = q.get("router") === "1";
const size = Number(q.get("size")) || undefined;
const lang = q.get("lang") ?? undefined;
const name = q.get("name") ?? undefined;

const light = { background: "oklch(1 0 0)", foreground: "oklch(0.2 0.03 265)", primary: "oklch(0.5 0.2 285)", "primary-foreground": "oklch(0.98 0 0)", muted: "oklch(0.96 0.01 285)", "muted-foreground": "oklch(0.5 0.02 285)", border: "oklch(0.9 0.01 285)", accent: "oklch(0.95 0.02 285)", destructive: "oklch(0.58 0.22 27)" };
const darkVars = { background: "oklch(0.17 0.02 285)", foreground: "oklch(0.96 0.01 285)", primary: "oklch(0.72 0.16 285)", "primary-foreground": "oklch(0.17 0.02 285)", muted: "oklch(0.26 0.02 285)", "muted-foreground": "oklch(0.72 0.02 285)", border: "oklch(1 0 0 / 12%)", accent: "oklch(0.3 0.03 285)", destructive: "oklch(0.7 0.19 22)" };
const css = (vars: Record<string, string>) => Object.entries(vars).map(([k, v]) => `--${k}: ${v};`).join(" ");
const style = document.createElement("style");
style.textContent = `:root { ${css(light)} } .dark { ${css(darkVars)} } body { background: var(--background) !important; color: var(--foreground) !important; }`;
document.head.append(style);
document.documentElement.classList.toggle("dark", dark);

const kinds = [{ value: "video", label: "Videos" }, { value: "post", label: "Posts" }];
const languages = [{ value: "en", label: "English" }, { value: "ja", label: "日本語" }];
const postHref = (p: { code: string; url_slug: string }) => `#/blog/${p.code}/${p.url_slug}`;
const taxonomyKinds: TaxonomyKind[] = [
  { kind: "tag", label: "Tags" },
  { kind: "series", label: "Series", cover: { kind: "series" } },
  { kind: "character", label: "Characters", relations: [{ relation: "member_of", to: "series", label: "Series", inverseLabel: "Characters" }] },
];
const restricted = (n: { taxonomy_id: string }) => (
  <label data-demo="host-field" style={{ display: "flex", gap: 8, alignItems: "center" }}>
    <input type="checkbox" /> Restricted (host field of {n.taxonomy_id.slice(0, 8)})
  </label>
);
const contentLink = (ref: { content_kind: string; content_id: string }) => <a href={`#${ref.content_kind}/${ref.content_id}`}>{`${ref.content_kind} ${ref.content_id.slice(0, 8)}`}</a>;
const itemHref = (i: { kind: string; id: string }) => `#${i.kind}/${i.id}`;

function Page({ children }: { children: ReactNode }) {
  return (
    <main style={{ maxWidth: 900, margin: "0 auto", padding: 16, display: "grid", gap: 16 }}>
      <div data-demo="host-foreground" style={{ color: "var(--foreground)", position: "absolute", width: 1, height: 1, overflow: "hidden" }}>
        host
      </div>
      {children}
    </main>
  );
}

function Alone() {
  const at = location.pathname;
  const user = at.match(/^\/admin\/users\/([^/]+)/)?.[1];
  const common = { baseUrl: "/api/contentkit", auth, pageSize: size, language: lang };
  return (
    <Page>
      {user ? (
        <UserCommentStanding {...common} user={decodeURIComponent(user)} name={name} itemHref={itemHref} />
      ) : at.startsWith("/admin/polls") ? (
        <PollManager {...common} basePath="/admin/polls" languages={languages} />
      ) : at.startsWith("/admin/taxonomy") ? (
        <TaxonomyManager {...common} basePath="/admin/taxonomy" kinds={taxonomyKinds} languages={languages} renderFields={restricted} renderContent={contentLink} />
      ) : at.startsWith("/admin/posts") ? (
        <PostManager {...common} basePath="/admin/posts" languages={languages} renderEditor={richTextEditor} postHref={postHref} />
      ) : (
        <CommentModeration {...common} basePath="/admin/comments" contentKinds={kinds} itemHref={itemHref} />
      )}
    </Page>
  );
}

function Hosted() {
  const loc = useLocation();
  const navigate = useNavigate();
  const navigation = useMemo<AdminNavigation>(() => ({ pathname: loc.pathname, search: loc.search, navigate: (to, o) => void navigate(to, o) }), [loc, navigate]);
  return (
    <Page>
      <nav data-demo="host-nav" style={{ display: "flex", flexWrap: "wrap", gap: 12, minWidth: 0 }}>
        <Link to="/admin/comments">Comments</Link>
        <Link to="/admin/comments/bans">Bans</Link>
        <Link to="/admin/polls">Polls</Link>
        <Link to="/admin/posts">Posts</Link>
        <Link to="/admin/taxonomy/series">Series</Link>
        <span data-demo="router-location" style={{ minWidth: 0, overflowWrap: "anywhere" }}>
          {loc.pathname + loc.search}
        </span>
      </nav>
      <Routes>
        <Route
          path="/admin/comments/*"
          element={<CommentModeration client={client} basePath="/admin/comments" navigation={navigation} contentKinds={kinds} itemHref={itemHref} pageSize={size} language={lang} />}
        />
        <Route path="/admin/users/:id" element={<UserPage />} />
        <Route
          path="/admin/taxonomy/*"
          element={
            <TaxonomyManager client={client} basePath="/admin/taxonomy" navigation={navigation} kinds={taxonomyKinds} languages={languages} renderFields={restricted} renderContent={contentLink} pageSize={size} language={lang} />
          }
        />
        <Route path="/admin/polls/*" element={<PollManager client={client} basePath="/admin/polls" navigation={navigation} languages={languages} pageSize={size} language={lang} />} />
        <Route
          path="/admin/posts/*"
          element={<PostManager client={client} basePath="/admin/posts" navigation={navigation} languages={languages} renderEditor={richTextEditor} postHref={postHref} pageSize={size} language={lang} />}
        />
      </Routes>
    </Page>
  );
}

function UserPage() {
  const { id } = useParams();
  return <UserCommentStanding client={client} user={id!} name={name} itemHref={itemHref} pageSize={size} language={lang} />;
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    {router ? (
      <BrowserRouter>
        <Hosted />
      </BrowserRouter>
    ) : (
      <Alone />
    )}
  </StrictMode>,
);
