import { ContentKitProvider } from "@openrails/contentkit-ui/react";
import { CommentModeration, Comments, ContentKitUiProvider, FavoriteButton, Poll, PollEditor, ReactionButtons } from "@openrails/contentkit-ui";
import { StrictMode, useState } from "react";
import { createRoot } from "react-dom/client";
import { client, dark, item, q, theme, viewer } from "./session";

// A thread and a poll the spec seeded (e2e/support/seed.ts seedSocial):
// ?item= (a video), ?poll=; ?view=staff the staff screens, ?lang= the UI,
// ?mount=members the ContentKit that takes nothing from signed-out visitors.
const video = item("item", "video")!;
const poll = q.get("poll")!;
const view = q.get("view") ?? "public";
const lang = q.get("lang") ?? "en";

const panel: React.CSSProperties = { display: "grid", gap: 12, padding: 16, borderRadius: 14, background: dark ? "#18181b" : "#fff", border: `1px solid ${dark ? "#27272a" : "#e4e4e7"}` };

function Public() {
  const [asked, setAsked] = useState(0);
  return (
    <ContentKitProvider client={client} viewer={viewer} onSignIn={() => setAsked((n) => n + 1)}>
      <ContentKitUiProvider appearance={{ theme }} language={lang}>
        <main style={{ maxWidth: 760, margin: "0 auto", padding: 16, display: "grid", gap: 20 }}>
          <section data-demo="engagement" style={panel}>
            <h1 style={{ margin: 0, fontSize: 20 }}>Night Before the Counteroffensive</h1>
            <div style={{ display: "flex", flexWrap: "wrap", gap: 8, alignItems: "center" }}>
              <ReactionButtons item={video} />
              <FavoriteButton item={video} />
            </div>
            {asked > 0 && <p data-demo="sign-in">Sign-in requested ({asked})</p>}
          </section>
          <section data-demo="poll">
            <Poll poll={poll} />
          </section>
          <section data-demo="comments" style={panel}>
            <Comments item={video} count={4} />
          </section>
        </main>
      </ContentKitUiProvider>
    </ContentKitProvider>
  );
}

function Staff() {
  return (
    <ContentKitProvider client={client} viewer={viewer}>
      <ContentKitUiProvider appearance={{ theme }} language={lang}>
        <main style={{ maxWidth: 900, margin: "0 auto", padding: 16, display: "grid", gap: 20 }}>
          <section data-demo="moderation" style={panel}>
            <CommentModeration contentKinds={[{ value: "video", label: "Videos" }, { value: "post", label: "Posts" }]} itemHref={(i) => `#${i.kind}/${i.id}`} />
          </section>
          <section data-demo="poll-editor" style={panel}>
            <PollEditor languages={[{ value: "en", label: "English" }, { value: "ja", label: "日本語" }]} freeText />
          </section>
          <section data-demo="poll-edit" style={panel}>
            <PollEditor poll={poll} />
          </section>
        </main>
      </ContentKitUiProvider>
    </ContentKitProvider>
  );
}

createRoot(document.getElementById("root")!).render(<StrictMode>{view === "staff" ? <Staff /> : <Public />}</StrictMode>);
