import { MediaGallery, VideoMiniPlayer, VideoPlayer, type PlayerHandoff } from "@openrails/contentkit-ui";
import type { FileInfo, RefBody } from "@openrails/contentkit-ui/client";
import { useMediaRead } from "@openrails/contentkit-ui/react";
import { useCallback, useState, type ReactNode } from "react";
import { client, dark, item, q } from "./session";

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

/** An item's media as this viewer reads it, through the client. */
function Album({ at, storageKey, locked, maxHeight }: { at: RefBody; storageKey?: string | null; locked?: boolean; maxHeight?: string }) {
  return <MediaGallery item={at} storageKey={storageKey} renderLocked={locked ? unlock : undefined} maxHeight={maxHeight} />;
}

// ?post= (4 images and 2 clips), ?locked= (locked to this viewer), ?single= (one vertical clip).
export function GalleryDemo() {
  const post = item("post", "album");
  const locked = item("locked", "album");
  const single = item("single", "album");
  return (
    <main style={{ maxWidth: 680, margin: "0 auto", padding: "24px 16px", display: "grid", gap: 20 }}>
      {post && (
        <Post title="Weekend shoot: 4 photos and 2 clips" demo="post">
          <Album at={post} storageKey="demo.gallery.view" />
        </Post>
      )}
      {locked && (
        <Post title="Members only" demo="locked">
          <Album at={locked} storageKey={null} locked />
        </Post>
      )}
      {single && (
        <Post title="A vertical clip" demo="single">
          <Album at={single} maxHeight="80svh" />
        </Post>
      )}
    </main>
  );
}

/** The item's first ladder in a bare player; refresh re-reads and counts. */
function Player({ at }: { at: RefBody }) {
  const { read, refresh: again } = useMediaRead(at);
  const [refreshed, setRefreshed] = useState(0);
  const refresh = useCallback(() => {
    setRefreshed((n) => n + 1);
    again();
  }, [again]);
  const dir = read?.hls?.[0];
  const video = read?.files.find((f: FileInfo) => dir && f.path.startsWith(dir) && f.type.startsWith("video/"));
  if (!dir || !video) return null;
  return (
    <>
      <VideoPlayer base={client.media.hlsBase(at, dir)} xhrSetup={client.media.xhrSetup} width={video.w} height={video.h} duration={video.dur} refresh={refresh} />
      <span data-demo="refreshed">{refreshed}</span>
    </>
  );
}

// Failure modes: each item's requests to media-gateway carry the fault the test set for it.
export function PlayerDemo() {
  return (
    <main style={{ maxWidth: 680, margin: "0 auto", padding: "24px 16px", display: "grid", gap: 20 }}>
      {["no-cors", "refresh", "denied", "busy", "missing"].map((demo) => {
        const at = item(demo, "album");
        return (
          at && (
            <Post key={demo} title={demo} demo={demo}>
              <Player at={at} />
            </Post>
          )
        );
      })}
    </main>
  );
}

// Adaptive bitrate over the worker's 480–2160 ladder of ?abr=.
export function AbrDemo() {
  const at = item("abr", "abr")!;
  const { read, refresh } = useMediaRead(at);
  const dir = read?.hls?.[0];
  return (
    <main style={{ maxWidth: 680, margin: "0 auto", padding: "24px 16px", display: "grid", gap: 20 }}>
      <Post title="abr" demo="abr">
        {dir && <VideoPlayer base={client.media.hlsBase(at, dir)} xhrSetup={client.media.xhrSetup} width={1920} height={1080} refresh={refresh} />}
        <button type="button" onClick={() => document.querySelector<HTMLElement>("[data-demo=abr] [data-ckui=video-player]")?.requestFullscreen()}>
          Fullscreen
        </button>
      </Post>
    </main>
  );
}

// Everything a watch page uses, over ?watch= (the worker's ladder of a 24 s
// source with English and Japanese audio, English and Spanish subtitle
// uploads, a seek sprite and an MP4 download). Events land in window.events.
const events: { name: string; [k: string]: unknown }[] = ((window as unknown as { events: unknown[] }).events = []) as never;
const log = (name: string) => (x: object) => events.push({ name, ...x });

export function WatchDemo() {
  const at = item("watch", "watch")!;
  const { read, refresh } = useMediaRead(at, { download: true });
  const [theater, setTheater] = useState(false);
  const [handoff, setHandoff] = useState<PlayerHandoff | null>(null);
  const [start, setStart] = useState({ at: Number(q.get("t")) || undefined, play: q.get("autoplay") === "1", key: 0 });
  const [version, setVersion] = useState("v1");
  const dir = read?.hls?.[0];
  const video = read?.files.find((f: FileInfo) => dir && f.path.startsWith(dir) && f.type.startsWith("video/"));
  const download = read?.files.find((f: FileInfo) => f.path === "video.mp4");
  return (
    <main style={{ maxWidth: theater ? 1200 : 680, margin: "0 auto", padding: "24px 16px", display: "grid", gap: 20 }} data-theater={theater ? "" : undefined}>
      <Post title={`watch (${version})`} demo="watch">
        {handoff ? (
          <p data-demo="browsing">Browsing while the mini player plays.</p>
        ) : (
          dir &&
          video && (
            <VideoPlayer
              key={start.key}
              base={client.media.hlsBase(at, dir)}
              xhrSetup={client.media.xhrSetup}
              refresh={refresh}
              width={video.w}
              height={video.h}
              duration={video.dur}
              keyboard="global"
              startAt={start.at}
              autoPlay={start.play}
              theater={theater}
              onTheaterChange={setTheater}
              onMiniPlayer={setHandoff}
              onProgress={log("progress")}
              onEvent={log("event")}
              renderDownloads={({ className }) => (
                <a className={className} href={download?.url} download="clip.mp4" aria-label="Download" title="Download">
                  ⤓
                </a>
              )}
              menuItems={[
                { label: "Version", value: version, options: [{ value: "v1", label: "Original" }, { value: "v2", label: "Director's cut" }], onChange: setVersion },
                { label: "Report a problem", onSelect: () => log("report")({}) },
              ]}
            />
          )
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
