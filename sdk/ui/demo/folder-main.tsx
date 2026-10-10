import { createContentKitClient, type UploadRule } from "@openrails/contentkit-ui/client";
import { ContentKitProvider, useContentKitClient } from "@openrails/contentkit-ui/react";
import { ContentKitUiProvider, MediaFolderEditor, MediaGallery, MediaReadinessNotice, type ContentKitUiTheme, type MediaFolderEditorHandle } from "@openrails/contentkit-ui";
import { StrictMode, useRef, useState, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { DemoServer } from "./fake";

const q = new URLSearchParams(location.search);
const theme = (q.get("theme") ?? "light") as ContentKitUiTheme;
const dark = theme === "dark";
document.documentElement.style.colorScheme = dark ? "dark" : "light";
document.body.style.cssText = `margin:0;font-family:Inter,ui-sans-serif,system-ui,sans-serif;background:${dark ? "#09090b" : "#fafafa"};color:${dark ? "#fafafa" : "#09090b"}`;

const MiB = 1 << 20;
const rules: UploadRule[] = [
  { path: "images/{name}", types: ["image/jpeg", "image/png", "image/webp"], max_bytes: 25 * MiB, max: 6 },
  { path: "videos/{name}", types: ["video/mp4"], max_bytes: 1024 * MiB, max: 2, min_aspect: 1 / 2.4, max_aspect: 2.4 },
];
const server = new DemoServer(`http://127.0.0.1:${q.get("media") ?? 4180}`);
server.rules.set("post", rules);
server.delay = Number(q.get("delay") ?? 120);
const client = createContentKitClient({ baseUrl: "", mounts: { upload: "/api", media: "/read" }, fetch: server.fetch, media: { transport: server.transport } });
const post = { kind: "post", id: "0192f000-0000-7000-8000-000000000101" };
const draft = { kind: "post", id: "0192f000-0000-7000-8000-000000000102" };

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

// A draft composer: files commit as they finish; discarding stops uploads and removes what landed.
function Composer() {
  const handle = useRef<MediaFolderEditorHandle>(null);
  const { media } = useContentKitClient();
  const [discarded, setDiscarded] = useState(0);
  const discard = async () => {
    const folder = handle.current!.folder;
    handle.current!.discard();
    const paths = folder.uploads.map((f) => f.path);
    if (paths.length) await media.commit(draft, paths.map((path) => ({ op: "remove", path })));
    setDiscarded((n) => n + 1);
  };
  return (
    <>
      <MediaFolderEditor key={discarded} item={draft} ref={handle} commit="auto" label="New post" />
      <button type="button" onClick={() => void discard()} style={{ justifySelf: "start" }}>
        Discard draft
      </button>
    </>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ContentKitProvider client={client}>
      <ContentKitUiProvider appearance={{ theme }} language={q.get("lang") ?? undefined}>
        <main style={{ maxWidth: 760, margin: "0 auto", padding: "28px 16px", display: "grid", gap: 20 }}>
          <Card title="Post editor" demo="editor">
            <MediaFolderEditor item={post} footer={<small style={{ opacity: 0.7 }}>Readers without access see the first image, blurred.</small>} />
            <MediaReadinessNotice item={post} />
          </Card>
          <Card title="Post as readers see it" demo="post">
            <MediaGallery item={post} prefix="low-res/" storageKey={null} />
          </Card>
          <Card title="Composer" demo="composer">
            <Composer />
          </Card>
        </main>
      </ContentKitUiProvider>
    </ContentKitProvider>
  </StrictMode>,
);
