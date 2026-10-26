import { ContentKitProvider, useContent } from "@openrails/contentkit-ui/react";
import { StrictMode, useRef } from "react";
import { createRoot } from "react-dom/client";
import { client, dark, q, viewer } from "./session";

// A blog item the spec seeded (?post=): ?view=editor a minimal rich-text
// editor over useContent, else the item as a reader sees it. Bodies are HTML;
// the harness sanitizes them like a host.
const id = q.get("post")!;
const view = q.get("view") ?? "reader";
const panel: React.CSSProperties = { display: "grid", gap: 12, padding: 16, borderRadius: 14, background: dark ? "#18181b" : "#fff", border: `1px solid ${dark ? "#27272a" : "#e4e4e7"}` };

function Editor() {
  const editor = useContent(id, "blog", "en");
  const field = useRef<HTMLDivElement>(null);
  const item = editor.content;
  if (!item || !editor.text) return <p>{editor.error ? editor.error.code : "Loading…"}</p>;
  const draft = item.state === "draft";
  return (
    <section data-demo="editor" style={panel}>
      <h1 style={{ margin: 0, fontSize: 20 }}>{editor.text.title}</h1>
      <div ref={field} data-demo="body" contentEditable suppressContentEditableWarning style={{ minHeight: 80 }} dangerouslySetInnerHTML={{ __html: editor.text.body ?? "" }} />
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
        <input
          type="file"
          accept="image/*"
          aria-label="Add image"
          onChange={async (e) => {
            const file = e.target.files?.[0];
            if (!file) return;
            const url = await editor.uploadImage(file);
            field.current!.insertAdjacentHTML("beforeend", `<p><img src="${url}" alt=""></p>`);
          }}
        />
        <button type="button" disabled={editor.saving} onClick={() => editor.saveText({ title: editor.text!.title, body: field.current!.innerHTML })}>
          Save
        </button>
        <button type="button" disabled={editor.saving} onClick={() => (draft ? editor.publish() : editor.unpublish())}>
          {draft ? "Publish" : "Unpublish"}
        </button>
      </div>
      <p data-demo="state">{editor.saving ? "Saving" : draft ? "Draft" : "Published"}</p>
    </section>
  );
}

function Reader() {
  const { content, error } = useContent(id, "blog", "en");
  if (!content) return <p data-demo="missing">{error ? error.code : "Loading…"}</p>;
  return (
    <article data-demo="post" style={panel}>
      <h1 style={{ margin: 0, fontSize: 20 }}>{content.title}</h1>
      <div dangerouslySetInnerHTML={{ __html: content.body ?? "" }} />
    </article>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ContentKitProvider client={client} viewer={viewer}>
      <main style={{ maxWidth: 760, margin: "0 auto", padding: 16 }}>{view === "editor" ? <Editor /> : <Reader />}</main>
    </ContentKitProvider>
  </StrictMode>,
);
