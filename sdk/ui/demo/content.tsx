import { ContentKitProvider, useContentDetail } from "@openrails/contentkit-ui/react";
import { ContentKitUiProvider, ContentListing, ContentRail, Paywall, SearchBox, SearchResults, VersionPicker, useVersionMenuItems } from "@openrails/contentkit-ui";
import type { Card, ContentQuery, ListSort } from "@openrails/contentkit-ui/client";
import { StrictMode, useMemo, useState, useSyncExternalStore } from "react";
import { createRoot } from "react-dom/client";
import { client, dark, q, theme, viewer } from "./session";

// Content pages: ?view=listing&kind=&groups= a listing (its sort and filters in
// the URL), ?view=rails&featured=&groups= a featured rail, a list rail and
// continue watching, ?view=search a search page, ?view=detail&id= a content
// page (version picker, paywall, the player's version menu). Checkouts land in window.events.
const lang = q.get("lang") ?? "en";
const groups = (q.get("groups") ?? "").split(",").filter(Boolean);
const panel: React.CSSProperties = { display: "grid", gap: 12, padding: 16, borderRadius: 14, background: dark ? "#18181b" : "#fff", border: `1px solid ${dark ? "#27272a" : "#e4e4e7"}` };
const events: { name: string; [k: string]: unknown }[] = ((window as unknown as { events: unknown[] }).events = []) as never;
const href = (c: Card) => `/content.html?view=detail&id=${c.id}&lang=${lang}${q.get("theme") ? `&theme=${q.get("theme")}` : ""}`;

const subscribe = (cb: () => void) => {
  addEventListener("popstate", cb);
  return () => removeEventListener("popstate", cb);
};
function useSearchParams(): [URLSearchParams, (set: (u: URLSearchParams) => void) => void] {
  const search = useSyncExternalStore(subscribe, () => location.search);
  return [
    useMemo(() => new URLSearchParams(search), [search]),
    (set) => {
      const u = new URL(location.href);
      set(u.searchParams);
      history.pushState(null, "", u);
      dispatchEvent(new PopStateEvent("popstate"));
    },
  ];
}

function Listing() {
  const [p, push] = useSearchParams();
  const query = useMemo<ContentQuery>(
    () => ({
      groups,
      sort: (p.get("sort") as ListSort) ?? undefined,
      language: p.get("language") ?? undefined,
      traits: p.get("traits")?.split(",").filter(Boolean),
      attribute: p.getAll("attribute"),
    }),
    [p],
  );
  return (
    <ContentListing
      kind={q.get("kind") ?? "comic"}
      query={query}
      onQueryChange={(next) =>
        push((u) => {
          for (const k of ["sort", "language", "traits", "attribute"]) u.delete(k);
          if (next.sort) u.set("sort", next.sort);
          if (next.language) u.set("language", next.language);
          if (next.traits?.length) u.set("traits", next.traits.join(","));
          for (const a of next.attribute ?? []) u.append("attribute", a);
        })
      }
      pageSize={Number(q.get("size")) || 12}
      href={href}
      labels={{ format: "Format", colored: "Colored", uncensored: "Uncensored" }}
    />
  );
}

function Rails() {
  const featured = q.get("featured");
  return (
    <>
      {featured && <ContentRail title="Featured" group={featured} href={href} />}
      <ContentRail title="Newest" list={{ kind: q.get("kind") ?? "comic", groups, sort: "newest" }} href={href} more={<a href="/content.html?view=listing">All</a>} />
      <ContentRail title="Continue watching" history={{ status: "in_progress" }} href={href} />
    </>
  );
}

function Search() {
  const [p, push] = useSearchParams();
  const text = p.get("q") ?? "";
  return (
    <>
      <SearchBox key={text} defaultValue={text} kind={q.get("kind") ?? undefined} href={(x) => ("title" in x ? href(x) : undefined)} onSearch={(s) => push((u) => u.set("q", s))} />
      <SearchResults query={{ q: text, kind: q.get("kind") ?? undefined }} href={href} />
    </>
  );
}

function Detail({ id }: { id: string }) {
  const [version, setVersion] = useState<string | null>(null);
  const d = useContentDetail(id, { version });
  const menu = useVersionMenuItems(d.versions, d.version?.id, setVersion);
  if (!d.content) return <p>{d.error ? d.error.message : "Loading…"}</p>;
  return (
    <>
      <h1 style={{ margin: 0, fontSize: 22 }} data-demo="title">
        {d.content.title}
      </h1>
      <VersionPicker versions={d.versions} value={d.version?.id} onChange={setVersion} />
      <p data-demo="version">{d.version?.id}</p>
      {d.decision && <Paywall decision={d.decision} onCheckout={(o) => events.push({ name: "checkout", ...o })} />}
      <ul data-demo="player-menu">
        {menu.map((m) => (
          <li key={m.label}>{"options" in m ? `${m.label}: ${m.options.map((o) => o.label).join(" | ")}` : m.label}</li>
        ))}
      </ul>
    </>
  );
}

function Page() {
  const view = q.get("view") ?? "listing";
  return (
    <main style={{ maxWidth: 1000, margin: "0 auto", padding: 16, display: "grid", gap: 20 }}>
      <section data-demo={view} style={panel}>
        {view === "listing" && <Listing />}
        {view === "rails" && <Rails />}
        {view === "search" && <Search />}
        {view === "detail" && <Detail id={q.get("id")!} />}
      </section>
    </main>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ContentKitProvider client={client} viewer={viewer} language={lang}>
      <ContentKitUiProvider appearance={{ theme }} language={lang}>
        <Page />
      </ContentKitUiProvider>
    </ContentKitProvider>
  </StrictMode>,
);
