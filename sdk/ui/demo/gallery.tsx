import { MediaGallery, VideoMiniPlayer, VideoPlayer, type PlayerHandoff } from "@openrails/contentkit-ui";
import type { Access, FileInfo, ReadResult } from "@openrails/contentkit-ui/client";
import { useState, type ReactNode } from "react";

const q = new URLSearchParams(location.search);
const media = `http://127.0.0.1:${q.get("media") ?? 4180}`;
const dark = q.get("theme") === "dark";

// Reads as the read API answers them: each video is an HLS folder in `hls`.
const read = (access: Access, files: FileInfo[], hls: string[] = []): ReadResult => ({ access, total: files.length, offset: 0, limit: 50, expires: 0, files, hls });
const img = (n: number, w: number, h: number): FileInfo => ({ path: `low-res/${n}.webp`, type: "image/jpeg", w, h, url: `${media}/cors/img/${n}.jpg` });
const vid = (name: string, w: number, h: number): FileInfo => ({ path: `${name}/video.mp4`, type: "video/mp4", w, h, dur: 6, url: "u" });

// 4 images and 2 videos, mixed aspects.
const post = read("full", [img(1, 1200, 900), vid("landscape", 480, 270), img(2, 900, 1125), vid("portrait", 360, 640), img(3, 1280, 720), img(4, 1000, 1000)], ["landscape/", "portrait/"]);
const locked: ReadResult = {
  ...read(
    "none",
    [1, 2, 3, 4, 5].map((n) => (n % 2 ? { path: `low-res/${n}.webp`, type: "image/jpeg", locked: true } : { path: `v${n}/video.mp4`, type: "video/mp4", locked: true })),
  ),
  previews: [`${media}/cors/img/teaser.jpg`],
};
const single = read("full", [vid("portrait", 360, 640)], ["portrait/"]);
const hlsBase = (dir: string) => `${media}/cors/${dir}`;

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
      </Post>
    </main>
  );
}

// Everything a watch page uses, over e2e/fixtures/media/tracks (two audio
// languages, two subtitle languages, a sprite). Events land in window.events.
const events: { name: string; [k: string]: unknown }[] = ((window as unknown as { events: unknown[] }).events = []) as never;
const log = (name: string) => (x: object) => events.push({ name, ...x });

export function WatchDemo() {
  const [theater, setTheater] = useState(false);
  const [handoff, setHandoff] = useState<PlayerHandoff | null>(null);
  const [start, setStart] = useState({ at: Number(q.get("t")) || undefined, play: q.get("autoplay") === "1", key: 0 });
  const [version, setVersion] = useState("v1");
  return (
    <main style={{ maxWidth: theater ? 1200 : 680, margin: "0 auto", padding: "24px 16px", display: "grid", gap: 20 }} data-theater={theater ? "" : undefined}>
      <Post title={`watch (${version})`} demo="watch">
        {handoff ? (
          <p data-demo="browsing">Browsing while the mini player plays.</p>
        ) : (
          <VideoPlayer
            key={start.key}
            base={`${media}/cors/tracks/`}
            width={480}
            height={270}
            duration={24}
            keyboard="global"
            startAt={start.at}
            autoPlay={start.play}
            theater={theater}
            onTheaterChange={setTheater}
            onMiniPlayer={setHandoff}
            onProgress={log("progress")}
            onEvent={log("event")}
            renderDownloads={({ className }) => (
              <a className={className} href={`${media}/cors/tracks/video.mp4`} download="clip.mp4" aria-label="Download" title="Download">
                ⤓
              </a>
            )}
            menuItems={[
              { label: "Version", value: version, options: [{ value: "v1", label: "Original" }, { value: "v2", label: "Director's cut" }], onChange: setVersion },
              { label: "Report a problem", onSelect: () => log("report")({}) },
            ]}
          />
        )}
      </Post>
      {handoff && (
        <VideoMiniPlayer
          handoff={handoff}
          label="Test pattern"
          onExpand={({ time, playing }) => {
            log("expand")({ time, playing });
            setHandoff(null);
            setStart((s) => ({ at: time, play: playing, key: s.key + 1 }));
          }}
          onClose={({ time }) => {
            log("close")({ time });
            setHandoff(null);
          }}
          onProgress={log("mini-progress")}
        />
      )}
    </main>
  );
}
