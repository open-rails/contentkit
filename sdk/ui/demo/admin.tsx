import { CommentModeration, ContentManager, GroupManager, MediaItemPanel, PollManager, UploadQueue, UserCommentStanding, type AdminNavigation } from "@openrails/contentkit-ui";
import { richTextEditor } from "@openrails/contentkit-ui/editor";
import { StrictMode, useMemo, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Link, Route, Routes, useLocation, useNavigate, useParams } from "react-router";
import type { Membership, RefBody } from "@openrails/contentkit-ui/client";
import { auth, client, dark, q, viewer } from "./session";

// A host's admin area. /admin/comments/* CommentModeration, /admin/users/{id}
// UserCommentStanding, /admin/polls/* PollManager, /admin/posts/* ContentManager of blog posts
// (with the package's rich-text editor), /admin/groups/* GroupManager, /admin/media/{kind}/{id}[/{version}] MediaItemPanel, /admin/uploads UploadQueue (each
// file a new ?kind= item at ?path=; [data-demo=created] counts the items made,
// across reloads; localStorage demo.last is the last one). ?router=1 mounts each flow in React Router through
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

const kinds = [{ value: "video", label: "Videos" }, { value: "blog", label: "Posts" }];
const languages = [{ value: "en", label: "English" }, { value: "ja", label: "日本語" }];
const contentHref = (c: { path: string }) => `#${c.path}`;
const subtitleLanguages = [{ value: "en", label: "English" }, { value: "ja", label: "Japanese" }];
const createItem = async (): Promise<RefBody> => {
  const res = await fetch("/__test/items", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ kind: q.get("kind") ?? "file", owner: viewer }) });
  const it = (await res.json()) as RefBody;
  // Every file of a content lives in its version.
  localStorage.setItem("demo.last", it.id);
  localStorage.setItem("demo.version", it.version ?? "");
  // ?hold=1: the item's third browser PUT waits until the test clears it.
  if (q.get("hold")) {
    await fetch("/__test/faults", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify([{ item: it.id, fault: "hold", from: "browser", skip: 2 }]) });
  }
  const n = Number(localStorage.getItem("demo.created") ?? 0) + 1;
  localStorage.setItem("demo.created", String(n));
  document.querySelector("[data-demo=created]")!.textContent = String(n);
  return { kind: it.kind, id: it.id, version: it.version };
};
function Created() {
  return <p data-demo="created">{localStorage.getItem("demo.created") ?? "0"}</p>;
}
const itemHref = (i: { kind: string; id: string }) => `#${i.kind}/${i.id}`;
const member = (m: Membership) => <span data-demo="member">{m.content_id}</span>;

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
      ) : at.startsWith("/admin/media/") ? (
        <MediaItemPanel {...common} item={{ kind: at.split("/")[3]!, id: at.split("/")[4]!, version: at.split("/")[5] || undefined }} languages={subtitleLanguages} />
      ) : at.startsWith("/admin/uploads") ? (
        <>
          <Created />
          <UploadQueue {...common} createItem={createItem} path={q.get("path") ?? "file"} />
        </>
      ) : at.startsWith("/admin/groups") ? (
        <GroupManager {...common} basePath="/admin/groups" renderContent={member} />
      ) : at.startsWith("/admin/posts") ? (
        <ContentManager {...common} kind="blog" basePath="/admin/posts" languages={languages} renderEditor={richTextEditor} contentHref={contentHref} />
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
        <Link to="/admin/groups">Groups</Link>
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
        <Route path="/admin/media/:kind/:id/:version?" element={<MediaPage />} />
        <Route
          path="/admin/uploads"
          element={
            <>
              <Created />
              <UploadQueue client={client} createItem={createItem} path={q.get("path") ?? "file"} language={lang} />
            </>
          }
        />
        <Route
          path="/admin/groups/*"
          element={<GroupManager client={client} basePath="/admin/groups" navigation={navigation} renderContent={member} pageSize={size} language={lang} />}
        />
        <Route path="/admin/polls/*" element={<PollManager client={client} basePath="/admin/polls" navigation={navigation} languages={languages} pageSize={size} language={lang} />} />
        <Route
          path="/admin/posts/*"
          element={<ContentManager client={client} kind="blog" basePath="/admin/posts" navigation={navigation} languages={languages} renderEditor={richTextEditor} contentHref={contentHref} pageSize={size} language={lang} />}
        />
      </Routes>
    </Page>
  );
}

function MediaPage() {
  const { kind, id, version } = useParams();
  return <MediaItemPanel client={client} item={{ kind: kind!, id: id!, version }} languages={subtitleLanguages} language={lang} />;
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
