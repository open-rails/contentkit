import { createUploadClient } from "@openrails/contentkit-upload";
import {
  HoverPreviewPicker,
  VideoPoster,
  VideoPosterPicker,
  AvatarUpload,
  CoverUpload,
  SlotEditError,
  SlotEditMenu,
  SlotEditor,
  SlotImage,
  UploadUiProvider,
  useSlotEditor,
  type UploadUiTheme,
} from "@openrails/contentkit-upload/ui";
import { StrictMode, useState } from "react";
import type { VideoImages } from "@openrails/contentkit-upload";
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

// A host layout: the header draws the images; SlotEditor adds only the flow and
// SlotEditMenu renders host-styled icon triggers over them.
const iconButton: React.CSSProperties = {
  display: "inline-flex",
  alignItems: "center",
  justifyContent: "center",
  width: 34,
  height: 34,
  borderRadius: 999,
  border: 0,
  background: "rgba(0,0,0,.55)",
  color: "#fff",
  cursor: "pointer",
};

function HeaderImage({ round }: { round?: boolean }) {
  const { image } = useSlotEditor();
  return <SlotImage manifest={image.manifest} round={round} sizes={round ? "96px" : "760px"} style={{ width: "100%", height: "100%" }} />;
}

function ChannelHeader() {
  return (
    <div data-demo="header" style={{ position: "relative", paddingBottom: 56 }}>
      <SlotEditor item={channel} slot="cover" aspect={3}>
        <div style={{ position: "relative", borderRadius: 14, overflow: "hidden", aspectRatio: "3" }}>
          <HeaderImage />
          <div style={{ position: "absolute", top: 10, right: 10 }}>
            <SlotEditMenu label="Change cover" iconOnly render={<button style={iconButton} />} />
          </div>
        </div>
        <SlotEditError />
      </SlotEditor>
      <SlotEditor item={channel} slot="avatar" aspect={1}>
        <div style={{ position: "absolute", left: 20, bottom: 0, width: 112, height: 112, borderRadius: 999, border: `4px solid ${dark ? "#18181b" : "#fff"}` }}>
          <HeaderImage round />
          <div style={{ position: "absolute", right: -2, bottom: -2 }}>
            <SlotEditMenu label="Change avatar" iconOnly render={<button style={iconButton} />} align="start" />
          </div>
        </div>
      </SlotEditor>
    </div>
  );
}

const video = { kind: "post", id: "1" };
await client.setVideoPoster(video, { source: "auto" });
const seededVideo = await client.setHoverPreview(video, {});

function VideoCard() {
  const [images, setImages] = useState<VideoImages>(seededVideo);
  const [open, setOpen] = useState<"poster" | "preview" | null>(null);
  return (
    <div data-demo="video" style={{ display: "grid", gap: 12 }}>
      <VideoPoster poster={images.poster} preview={images.hover_preview} sizes="(min-width: 760px) 360px, 100vw" style={{ maxWidth: 360 }} tabIndex={0} />
      <div style={{ display: "flex", gap: 8 }}>
        <button type="button" onClick={() => setOpen("poster")}>Choose poster</button>
        <button type="button" onClick={() => setOpen("preview")}>Hover preview</button>
      </div>
      <VideoPosterPicker open={open === "poster"} onOpenChange={(o) => setOpen(o ? "poster" : null)} item={video} images={images} onChange={setImages} />
      <HoverPreviewPicker open={open === "preview"} onOpenChange={(o) => setOpen(o ? "preview" : null)} item={video} images={images} onChange={setImages} />
    </div>
  );
}

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
        <Card title="Channel header (SlotEditor)">
          <ChannelHeader />
        </Card>
        <Card title="Channel profile">
          <CoverUpload item={channel} />
          <AvatarUpload item={channel} />
        </Card>
        <Card title="Video poster and hover preview">
          <VideoCard />
        </Card>
        <Card title="New channel">
          <CoverUpload item={empty} />
          <AvatarUpload item={empty} />
        </Card>
      </main>
    </UploadUiProvider>
  </StrictMode>,
);
