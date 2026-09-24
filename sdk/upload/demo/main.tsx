import { createUploadClient } from "@openrails/contentkit-upload";
import { AvatarUpload, CoverUpload, UploadUiProvider, type UploadUiTheme } from "@openrails/contentkit-upload/ui";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { DemoServer, sampleAvatar, sampleImage } from "./fake";

const q = new URLSearchParams(location.search);
const theme = (q.get("theme") ?? "light") as UploadUiTheme;
const dark = theme === "dark";
document.documentElement.style.colorScheme = dark ? "dark" : "light";
document.body.style.cssText = `margin:0;font-family:Inter,ui-sans-serif,system-ui,sans-serif;background:${dark ? "#09090b" : "#fafafa"};color:${dark ? "#fafafa" : "#09090b"}`;

const server = new DemoServer();
const client = createUploadClient({ endpoint: "/api", fetch: server.fetch, transport: server.transport });
const channel = { kind: "channel", id: "1" };
const empty = { kind: "channel", id: "2" };

await server.seed(channel, "cover", await sampleImage(3600, 1600, 210), { crop: { x: 0, y: 320, w: 3600, h: 1200 } });
await server.seed(channel, "avatar", await sampleAvatar(900));

function Card({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section
      style={{
        background: dark ? "#18181b" : "#fff",
        border: `1px solid ${dark ? "#27272a" : "#e4e4e7"}`,
        borderRadius: 14,
        padding: 20,
        display: "grid",
        gap: 20,
      }}
    >
      <h2 style={{ margin: 0, fontSize: 15, fontWeight: 600 }}>{title}</h2>
      {children}
    </section>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <UploadUiProvider client={client} appearance={{ theme }}>
      <main style={{ maxWidth: 760, margin: "0 auto", padding: "28px 16px", display: "grid", gap: 20 }}>
        <Card title="Channel profile">
          <CoverUpload item={channel} />
          <AvatarUpload item={channel} />
        </Card>
        <Card title="New channel">
          <CoverUpload item={empty} />
          <AvatarUpload item={empty} />
        </Card>
      </main>
    </UploadUiProvider>
  </StrictMode>,
);
