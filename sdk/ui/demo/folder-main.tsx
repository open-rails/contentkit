import { ContentKitProvider, useContentKitClient, useMediaFolder } from "@openrails/contentkit-ui/react";
import { ContentKitUiProvider, MediaFolderEditor, MediaGallery, MediaReadinessNotice } from "@openrails/contentkit-ui";
import type { RefBody } from "@openrails/contentkit-ui/client";
import { StrictMode, useEffect, useRef, useState, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { client, dark, item, q, theme } from "./session";

// ?post= and ?draft=: album items the signed-in user owns; the upload rules
// come from the server (the album kind in e2e/server/kinds.json).
const post = item("post", "album");
const draft = item("draft", "album");

function Card({ title, children, demo }: { title: string; children: ReactNode; demo: string }) {
  return (
    <section
      data-demo={demo}
      style={{ background: dark ? "#18181b" : "#fff", border: `1px solid ${dark ? "#27272a" : "#e4e4e7"}`, borderRadius: 14, padding: 20, display: "grid", gap: 16 }}
    >
      <h2 style={{ margin: 0, fontSize: 15, fontWeight: 600 }}>{title}</h2>
      {children}
    </section>
  );
}

// A draft composer that owns its folder: the draft is created on the first
// drop, files commit as they finish, and Publish waits for them. Discarding
// stops uploads and removes what landed.
function Composer({ draft }: { draft: RefBody }) {
  const { media } = useContentKitClient();
  const [ref, setRef] = useState<RefBody | null>(null);
  const [published, setPublished] = useState(false);
  const folder = useMediaFolder(ref, { commit: "auto" });
  const creating = useRef(false);
  useEffect(() => {
    if (ref || creating.current || !folder.fileCount) return;
    creating.current = true;
    // The host's create-draft request.
    setTimeout(() => setRef(draft), 600);
  }, [ref, folder.fileCount, draft]);
  const discard = async () => {
    folder.discard();
    const paths = folder.uploads.map((f) => f.path);
    if (paths.length) await media.commit(draft, paths.map((path) => ({ op: "remove", path })));
    creating.current = false;
    setRef(null);
    setPublished(false);
  };
  return (
    <>
      <MediaFolderEditor folder={folder} label="New post" />
      <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
        <button type="button" disabled={folder.busy || !folder.fileCount} onClick={() => setPublished(true)}>
          Publish
        </button>
        <button type="button" onClick={() => void discard()}>
          Discard draft
        </button>
        <small data-demo="files" style={{ opacity: 0.7 }}>
          {published ? "Published" : `${folder.fileCount} files${folder.busy ? ", uploading" : ""}`}
        </small>
      </div>
    </>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ContentKitProvider client={client}>
      <ContentKitUiProvider appearance={{ theme }} language={q.get("lang") ?? undefined}>
        <main style={{ maxWidth: 760, margin: "0 auto", padding: "28px 16px", display: "grid", gap: 20 }}>
          {post && (
            <>
              <Card title="Post editor" demo="editor">
                <MediaFolderEditor item={post} footer={<small style={{ opacity: 0.7 }}>Readers without access see the first image, blurred.</small>} />
                <MediaReadinessNotice item={post} />
              </Card>
              <Card title="Post as readers see it" demo="post">
                <MediaGallery item={post} prefix="large/" storageKey={null} />
              </Card>
            </>
          )}
          {draft && (
            <Card title="Composer" demo="composer">
              <Composer draft={draft} />
            </Card>
          )}
        </main>
      </ContentKitUiProvider>
    </ContentKitProvider>
  </StrictMode>,
);
