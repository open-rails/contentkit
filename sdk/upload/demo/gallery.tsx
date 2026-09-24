import type { FileInfo, ReadResult } from "@openrails/contentkit-upload";
import { MediaGallery, VideoPlayer } from "@openrails/contentkit-upload/ui";
import { useState, type ReactNode } from "react";

const q = new URLSearchParams(location.search);
const media = `http://127.0.0.1:${q.get("media") ?? 4180}`;
const dark = q.get("theme") === "dark";

const read = (access: string, files: FileInfo[]): ReadResult => ({ access, total: files.length, preview_limit: 0, offset: 0, limit: 50, expires: 0, files });
const img = (index: number, n: number, w: number, h: number): FileInfo => ({ index, name: `${n}.jpg`, type: "image/jpeg", w, h, url: `${media}/cors/img/${n}.jpg` });
const vid = (index: number, name: string, w: number, h: number): FileInfo => ({ index, name, type: "video/mp4", w, h, duration: 6, hls: true });

// 4 images and 2 videos, mixed aspects.
const post = read("full", [img(0, 1, 1200, 900), vid(1, "landscape", 480, 270), img(2, 2, 900, 1125), vid(3, "portrait", 360, 640), img(4, 3, 1280, 720), img(5, 4, 1000, 1000)]);
const locked = read("none", [
  { index: 0, name: "teaser", type: "image/jpeg", w: 480, h: 360, teaser: true, url: `${media}/cors/img/teaser.jpg` },
  ...[1, 2, 3, 4, 5].map((index) => ({ index, type: index % 2 ? "image/jpeg" : "video/mp4", locked: true })),
]);
const single = read("full", [vid(0, "portrait", 360, 640)]);
const hlsBase = (f: FileInfo) => `${media}/cors/${f.name}/`;

function Post({ title, children, demo }: { title: string; children: ReactNode; demo: string }) {
  return (
    <article
      data-demo={demo}
      style={{ background: dark ? "#18181b" : "#fff", border: `1px solid ${dark ? "#27272a" : "#e4e4e7"}`, borderRadius: 14, padding: 16, display: "grid", gap: 12 }}
    >
      <h2 style={{ margin: 0, fontSize: 16, fontWeight: 600 }}>{title}</h2>
      {children}
    </article>
  );
}

const unlock = ({ count }: { count: number }) => (
  <button type="button" style={{ border: 0, borderRadius: 8, padding: "8px 14px", fontWeight: 600, background: "#fafafa", color: "#18181b", cursor: "pointer" }}>
    Unlock {count} for $5
  </button>
);

export function GalleryDemo() {
  return (
    <main style={{ maxWidth: 680, margin: "0 auto", padding: "24px 16px", display: "grid", gap: 20 }}>
      <Post title="Weekend shoot: 4 photos and 2 clips" demo="post">
        <MediaGallery read={post} hlsBase={hlsBase} storageKey="demo.gallery.view" />
      </Post>
      <Post title="Members only" demo="locked">
        <MediaGallery read={locked} renderLocked={unlock} storageKey={null} />
      </Post>
      <Post title="A vertical clip" demo="single">
        <MediaGallery read={single} hlsBase={hlsBase} />
      </Post>
    </main>
  );
}

// Failure modes of the player against e2e/media-server.ts.
export function PlayerDemo() {
  const [token, setToken] = useState("expired");
  const card = (demo: string, children: ReactNode) => (
    <Post title={demo} demo={demo}>
      {children}
    </Post>
  );
  return (
    <main style={{ maxWidth: 680, margin: "0 auto", padding: "24px 16px", display: "grid", gap: 20 }}>
      {card("no-cors", <VideoPlayer base={`${media}/nocors/landscape/`} width={480} height={270} duration={6} />)}
      {card("refresh", <VideoPlayer base={`${media}/auth/${token}/landscape/`} width={480} height={270} refresh={() => setToken("fresh")} />)}
      {card("denied", <VideoPlayer base={`${media}/auth/revoked/landscape/`} width={480} height={270} refresh={() => {}} />)}
      {card("busy", <VideoPlayer base={`${media}/busy/landscape/`} width={480} height={270} />)}
      {card("missing", <VideoPlayer base={`${media}/missing/landscape/`} width={480} height={270} />)}
    </main>
  );
}

// Adaptive bitrate against the synthetic 480–2160 ladder in e2e/media-server.ts.
export function AbrDemo() {
  return (
    <main style={{ maxWidth: 680, margin: "0 auto", padding: "24px 16px", display: "grid", gap: 20 }}>
      <Post title="abr" demo="abr">
        <VideoPlayer base={`${media}/abr/landscape/`} width={1920} height={1080} duration={60} />
        <button type="button" onClick={() => document.querySelector<HTMLElement>("[data-demo=abr] [data-ckui=video-player]")?.requestFullscreen()}>
          Fullscreen
        </button>
      </Post>
    </main>
  );
}
