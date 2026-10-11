import { ContentKitProvider, usePost } from "@openrails/contentkit-ui/react";
import { StrictMode, useRef } from "react";
import { createRoot } from "react-dom/client";
import { client, dark, q, viewer } from "./session";

// A post the spec seeded (?post=): ?view=editor a minimal rich-text editor
// over usePost, else the post as a reader sees it. Bodies are HTML; the
// harness sanitizes them like a host.
const id = q.get("post")!;
const view = q.get("view") ?? "reader";
const panel: React.CSSProperties = { display: "grid", gap: 12, padding: 16, borderRadius: 14, background: dark ? "#18181b" : "#fff", border: `1px solid ${dark ? "#27272a" : "#e4e4e7"}` };

function Editor() {
  const editor = usePost(id);
  const field = useRef<HTMLDivElement>(null);
  if (!editor.post) return <p>{editor.error ? editor.error.code : "Loading…"}</p>;
  return (
    <section data-demo="editor" style={panel}>
      <h1 style={{ margin: 0, fontSize: 20 }}>{editor.post.title}</h1>
      <div ref={field} data-demo="body" contentEditable suppressContentEditableWarning style={{ minHeight: 80 }} dangerouslySetInnerHTML={{ __html: editor.editorBody ?? "" }} />
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
        <button type="button" disabled={editor.saving} onClick={() => editor.update({ body: field.current!.innerHTML })}>
          Save
        </button>
        <button type="button" disabled={editor.saving} onClick={() => editor.update({ is_draft: !editor.post!.is_draft })}>
          {editor.post.is_draft ? "Publish" : "Unpublish"}
        </button>
      </div>
      <p data-demo="state">{editor.saving ? "Saving" : editor.post.is_draft ? "Draft" : "Published"}</p>
    </section>
  );
}

function Reader() {
  const { post, error } = usePost(id);
  if (!post) return <p data-demo="missing">{error ? error.code : "Loading…"}</p>;
  return (
    <article data-demo="post" style={panel}>
      <h1 style={{ margin: 0, fontSize: 20 }}>{post.title}</h1>
      <div dangerouslySetInnerHTML={{ __html: post.body }} />
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
