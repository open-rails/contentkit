import { createContentKitClient } from "@openrails/contentkit-ui/client";
import { ContentKitProvider } from "@openrails/contentkit-ui/react";
import { CommentModeration, Comments, ContentKitUiProvider, FavoriteButton, Poll, PollEditor, ReactionButtons, type ContentKitUiTheme } from "@openrails/contentkit-ui";
import { StrictMode, useState } from "react";
import { createRoot } from "react-dom/client";

// The real content handlers (e2e/content-server.ts, proxied at /ck). ?actor=
// picks the caller ("" signs out), ?view=staff the staff screens, ?lang= the UI,
// ?mount=members a ContentKit that takes nothing from signed-out visitors.
const q = new URLSearchParams(location.search);
const theme = (q.get("theme") ?? "light") as ContentKitUiTheme;
const dark = theme === "dark";
const actor = q.get("actor") ?? "alice";
const view = q.get("view") ?? "public";
const lang = q.get("lang") ?? "en";
document.documentElement.style.colorScheme = dark ? "dark" : "light";
document.body.style.cssText = `margin:0;font-family:Inter,ui-sans-serif,system-ui,sans-serif;background:${dark ? "#09090b" : "#fafafa"};color:${dark ? "#fafafa" : "#09090b"}`;

const as = (who: string, baseUrl = "/ck") => {
  const ip = `10.7.${Math.floor(Math.random() * 250)}.1`;
  return createContentKitClient({ baseUrl, headers: (): Record<string, string> => (who ? { "X-Test-Actor": who } : { "X-Test-IP": ip }), folders: { post: "ckpost", poll: "ckpoll" } });
};
const client = as(actor, q.get("mount") === "members" ? "/ck-members" : "/ck");

function uuid7(): string {
  const hex = Date.now().toString(16).padStart(12, "0") + "7" + crypto.randomUUID().replace(/-/g, "").slice(0, 19);
  const v = hex.slice(0, 16) + "8" + hex.slice(17);
  return `${v.slice(0, 8)}-${v.slice(8, 12)}-${v.slice(12, 16)}-${v.slice(16, 20)}-${v.slice(20, 32)}`;
}

// A fresh item, thread and poll per page load.
const item = { kind: "video", id: uuid7() };
const [bob, carol, erin, editor] = ["bob", "carol", "erin", "editor"].map((who) => as(who));
const first = await bob!.comments.create(item, { body: "First! This trailer looks great." });
await carol!.comments.create(item, { body: "Agreed, the soundtrack at 1:20 is the best part.\nCan't wait for the full episode.", reply_to_id: first.id });
const gone = await carol!.comments.create(item, { body: "Oops, wrong thread." });
await carol!.comments.delete(gone.id);
await erin!.comments.create(item, {
  body:
    "I rewatched the first season before this. The pacing in the middle episodes was slow, but the payoff in the finale made up for it, and the animation in the battle scenes is some of the best I've seen this year. " +
    "If this keeps that quality I'm in. The only worry is whether they rush the adaptation now that the source material is nearly caught up.",
});
await bob!.comments.react(first.id, 1);
await bob!.reactions.set(item, 1);
await bob!.favorites.set(item, true);
await carol!.favorites.set(item, true);
await carol!.reactions.set(item, -1);
if (actor === "alice") await client.comments.create(item, { body: "Is this the director's cut? [hold]" });
const poll = await editor!.polls.create({ question: "Which season should we cover next?", language: lang, options: ["Spring 2026", "Summer 2026", "Autumn 2026"].map((label, position) => ({ label, position })) });
await bob!.polls.vote(poll.id, poll.options[0]!.id);
await carol!.polls.vote(poll.id, poll.options[1]!.id);
await erin!.polls.vote(poll.id, poll.options[0]!.id);
if (view === "staff") {
  await erin!.comments.create(item, { body: "Selling cheap keys, DM me [hold]" });
  await as("moderator").bans.ban("global", "spammer", { reason: "Repeated spam links" });
}

const panel: React.CSSProperties = { display: "grid", gap: 12, padding: 16, borderRadius: 14, background: dark ? "#18181b" : "#fff", border: `1px solid ${dark ? "#27272a" : "#e4e4e7"}` };

function Public() {
  const [asked, setAsked] = useState(0);
  return (
    <ContentKitProvider client={client} viewer={actor || null} onSignIn={() => setAsked((n) => n + 1)}>
      <ContentKitUiProvider appearance={{ theme }} language={lang}>
        <main style={{ maxWidth: 760, margin: "0 auto", padding: 16, display: "grid", gap: 20 }}>
          <section data-demo="engagement" style={panel}>
            <h1 style={{ margin: 0, fontSize: 20 }}>Night Before the Counteroffensive</h1>
            <div style={{ display: "flex", flexWrap: "wrap", gap: 8, alignItems: "center" }}>
              <ReactionButtons item={item} />
              <FavoriteButton item={item} />
            </div>
            {asked > 0 && <p data-demo="sign-in">Sign-in requested ({asked})</p>}
          </section>
          <section data-demo="poll">
            <Poll poll={poll.id} />
          </section>
          <section data-demo="comments" style={panel}>
            <Comments item={item} count={4} />
          </section>
        </main>
      </ContentKitUiProvider>
    </ContentKitProvider>
  );
}

function Staff() {
  return (
    <ContentKitProvider client={client} viewer={actor || null}>
      <ContentKitUiProvider appearance={{ theme }} language={lang}>
        <main style={{ maxWidth: 900, margin: "0 auto", padding: 16, display: "grid", gap: 20 }}>
          <section data-demo="moderation" style={panel}>
            <CommentModeration contentKinds={[{ value: "video", label: "Videos" }, { value: "post", label: "Posts" }]} itemHref={(i) => `#${i.kind}/${i.id}`} />
          </section>
          <section data-demo="poll-editor" style={panel}>
            <PollEditor languages={[{ value: "en", label: "English" }, { value: "ja", label: "日本語" }]} freeText />
          </section>
          <section data-demo="poll-edit" style={panel}>
            <PollEditor poll={poll.id} />
          </section>
        </main>
      </ContentKitUiProvider>
    </ContentKitProvider>
  );
}

createRoot(document.getElementById("root")!).render(<StrictMode>{view === "staff" ? <Staff /> : <Public />}</StrictMode>);
