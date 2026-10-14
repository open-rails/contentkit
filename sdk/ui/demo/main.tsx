import { ContentKitProvider, useSlotImage } from "@openrails/contentkit-ui/react";
import {
  VideoPoster,
  VideoPosterPicker,
  AvatarUpload,
  CoverUpload,
  EncodeProgress,
  SlotEditError,
  SlotEditMenu,
  SlotEditor,
  SlotImage,
  ContentKitUiProvider,
  useSlotEditor,
} from "@openrails/contentkit-ui";
import type { PublicPreset, RefBody } from "@openrails/contentkit-ui/client";
import { StrictMode, useState } from "react";
import { createRoot } from "react-dom/client";
import { client, dark, item, theme } from "./session";

// ?channel= (with a cover and avatar), ?empty= (a channel without), ?video= (with a source).
const channel = item("channel", "channel");
const empty = item("empty", "channel");
const video = item("video", "video");

const preset = (name: string, aspect: string): PublicPreset => ({ preset: name, aspect, renditions: [] });
const cover = preset("cover", "3:1");
const avatar = preset("avatar", "1:1");
const poster = preset("poster", "16:9");

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
  const { has, image } = useSlotEditor();
  return <SlotImage image={has ? { preset: "", aspect: image.aspect, renditions: image.renditions } : null} round={round} style={{ width: "100%", height: "100%" }} />;
}

function ChannelHeader({ item }: { item: RefBody }) {
  return (
    <div data-demo="header" style={{ position: "relative", paddingBottom: 56 }}>
      <SlotEditor item={item} path="cover" image={cover}>
        <div style={{ position: "relative", borderRadius: 14, overflow: "hidden", aspectRatio: "3" }}>
          <HeaderImage />
          <div style={{ position: "absolute", top: 10, right: 10 }}>
            <SlotEditMenu label="Change cover" iconOnly render={<button style={iconButton} />} />
          </div>
        </div>
        <SlotEditError />
      </SlotEditor>
      <SlotEditor item={item} path="avatar" image={avatar}>
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

function VideoCard({ item }: { item: RefBody }) {
  const [open, setOpen] = useState(false);
  const image = useSlotImage({ ref: item, path: "poster", image: poster });
  const shown = { ...poster, renditions: image.renditions };
  return (
    <div data-demo="video" style={{ display: "grid", gap: 12 }}>
      <VideoPoster poster={shown} style={{ maxWidth: 360 }} />
      <div style={{ display: "flex", gap: 8 }}>
        <button type="button" onClick={() => setOpen(true)}>Set cover</button>
      </div>
      <VideoPosterPicker open={open} onOpenChange={setOpen} item={item} image={shown} onChange={() => image.reload()} />
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
    <ContentKitProvider client={client}>
      <ContentKitUiProvider appearance={{ theme }}>
        <main style={{ maxWidth: 760, margin: "0 auto", padding: "28px 16px", display: "grid", gap: 20 }}>
          {channel && (
            <>
              <Card title="Channel header (SlotEditor)">
                <ChannelHeader item={channel} />
              </Card>
              <Card title="Channel profile">
                <CoverUpload item={channel} image={cover} />
                <AvatarUpload item={channel} image={avatar} />
              </Card>
            </>
          )}
          {video && (
            <Card title="Video poster">
              <VideoCard item={video} />
            </Card>
          )}
          <Card title="Video encode progress">
            <div data-demo="encode" style={{ display: "grid", gap: 16 }}>
              <EncodeProgress progress={{ phase: "queued", queue_position: 3, percent: 0, at: Date.now() }} />
              <EncodeProgress progress={{ phase: "encoding", segments_done: 5, segments_total: 27, percent: 22, speed: 2.4, eta: 40, at: Date.now() }} />
              <EncodeProgress progress={{ phase: "uploading", segments_done: 27, segments_total: 27, percent: 91, eta: 6, at: Date.now() }} />
            </div>
          </Card>
          {empty && (
            <Card title="New channel">
              <CoverUpload item={empty} image={cover} />
              <AvatarUpload item={empty} image={avatar} />
            </Card>
          )}
        </main>
      </ContentKitUiProvider>
    </ContentKitProvider>
  </StrictMode>,
);
