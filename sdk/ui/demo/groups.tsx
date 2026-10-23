import { ContentKitProvider } from "@openrails/contentkit-ui/react";
import { ContentKitUiProvider, EntityDirectory, EntityHeader, MembershipEditor } from "@openrails/contentkit-ui";
import { StrictMode, useSyncExternalStore } from "react";
import { createRoot } from "react-dom/client";
import { client, dark, q, theme, viewer } from "./session";

// Entity pages: ?kind=&parent= a directory (its letter in the URL, ?layout=grid),
// ?group= a group's header, ?content=&content_kind= a content's groups (staff)
// with ?versions=id:label,…; ?lang= the UI.
const lang = q.get("lang") ?? "en";
const versions = (q.get("versions") ?? "").split(",").filter(Boolean).map((v) => ({ id: v.split(":")[0]!, label: v.split(":")[1] ?? v }));
const panel: React.CSSProperties = { display: "grid", gap: 12, padding: 16, borderRadius: 14, background: dark ? "#18181b" : "#fff", border: `1px solid ${dark ? "#27272a" : "#e4e4e7"}` };

const subscribe = (cb: () => void) => {
  addEventListener("popstate", cb);
  return () => removeEventListener("popstate", cb);
};
function useLetter(): [string | null, (l: string | null) => void] {
  const letter = useSyncExternalStore(subscribe, () => new URLSearchParams(location.search).get("letter"));
  return [
    letter,
    (l) => {
      const u = new URL(location.href);
      if (l) u.searchParams.set("letter", l);
      else u.searchParams.delete("letter");
      history.pushState(null, "", u);
      dispatchEvent(new PopStateEvent("popstate"));
    },
  ];
}

function Page() {
  const [letter, setLetter] = useLetter();
  const group = q.get("group");
  const content = q.get("content");
  return (
    <main style={{ maxWidth: 900, margin: "0 auto", padding: 16, display: "grid", gap: 20 }}>
      {q.get("kind") && (
        <section data-demo="directory" style={panel}>
          <EntityDirectory
            kind={q.get("kind")!}
            parent={q.get("parent") ?? undefined}
            language={lang}
            layout={q.get("layout") === "grid" ? "grid" : "list"}
            letter={letter}
            onLetterChange={setLetter}
            href={(g) => `/groups.html?group=${g.id}&lang=${lang}${q.get("theme") ? `&theme=${q.get("theme")}` : ""}`}
          />
        </section>
      )}
      {group && (
        <section data-demo="header" style={panel}>
          <EntityHeader id={group} language={lang} />
        </section>
      )}
      {content && (
        <section data-demo="memberships" style={panel}>
          <MembershipEditor content={content} contentKind={q.get("content_kind") ?? "comic"} versions={versions} language={lang} />
        </section>
      )}
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
