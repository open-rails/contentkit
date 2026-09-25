// Serves e2e/fixtures/media like media-access, with failure modes by prefix:
//   /cors/…        CORS for the requesting origin (the healthy path)
//   /nocors/…      playlists with CORS, segments and sprites without (the missing MEDIA_ACCESS_CORS_ORIGINS incident)
//   /auth/{token}/ 404 unless token is "fresh" (media-access answers a bad token like a missing object)
//   /missing/…     404
//   /busy/…        429
//   /abr/{name}/   a synthetic 480–2160 ladder over {name}'s segments, repeated to 60 s,
//                  each padded with MPEG-TS null packets to its rung's bitrate
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import path from "node:path";

const root = path.join(import.meta.dirname, "fixtures", "media");
const port = Number(process.env.MEDIA_PORT ?? 4180);
const types: Record<string, string> = {
  ".m3u8": "application/vnd.apple.mpegurl",
  ".seg": "video/mp2t",
  ".vtt": "text/vtt",
  ".jpg": "image/jpeg",
};

// Short side → bits/s, as a ContentKit master lists them (the 1080p rung first).
const ladder: [number, number][] = [
  [1080, 5_000_000],
  [2160, 16_000_000],
  [1440, 9_000_000],
  [720, 2_800_000],
  [480, 1_200_000],
];
const SEGMENT_SECONDS = 2;
const REPEATS = 10;
const nullPacket = Buffer.alloc(188, 0xff);
nullPacket.set([0x47, 0x1f, 0xff, 0x10]);

async function abr(name: string, file: string[], url: URL): Promise<[string, Buffer | string] | null> {
  const dir = path.join(root, name, "video");
  const portrait = name === "portrait";
  const size = (short: number) => {
    const long = Math.round((short * 16) / 9 / 2) * 2;
    return portrait ? `${short}x${long}` : `${long}x${short}`;
  };
  if (file.join("/") === "master.m3u8") {
    let m = "#EXTM3U\n#EXT-X-VERSION:3\n";
    for (const [h, bw] of ladder) m += `#EXT-X-STREAM-INF:BANDWIDTH=${bw},AVERAGE-BANDWIDTH=${bw},RESOLUTION=${size(h)},CODECS="avc1.4d401e,mp4a.40.2"\nvideo/${h}.m3u8\n`;
    return [".m3u8", m];
  }
  const [, rungFile] = file;
  const pl = rungFile?.match(/^(\d+)\.m3u8$/);
  if (file[0] === "video" && pl) {
    let m = `#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:${SEGMENT_SECONDS}\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n`;
    for (let r = 0; r < REPEATS; r++) {
      if (r) m += "#EXT-X-DISCONTINUITY\n";
      for (let k = 0; k < 3; k++) m += `#EXTINF:${SEGMENT_SECONDS}.000000,\ns${k}.seg?rung=${pl[1]}&n=${r * 3 + k}\n`;
    }
    return [".m3u8", m + "#EXT-X-ENDLIST\n"];
  }
  const seg = rungFile?.match(/^s\d\.seg$/);
  const rung = ladder.find(([h]) => h === Number(url.searchParams.get("rung")));
  if (file[0] !== "video" || !seg || !rung) return null;
  const body = await readFile(path.join(dir, rungFile!));
  const want = Math.ceil((rung[1] * SEGMENT_SECONDS) / 8 / 188) * 188;
  const pad = Math.max(0, want - body.length) / 188;
  return [".seg", Buffer.concat([body, ...Array.from({ length: pad }, () => nullPacket)])];
}

createServer(async (req, res) => {
  const url = new URL(req.url ?? "/", "http://x");
  const [, mode, ...rest] = url.pathname.split("/");
  let file = rest;
  const origin = req.headers.origin;
  const cors = () => {
    if (!origin) return;
    res.setHeader("Access-Control-Allow-Origin", origin);
    res.setHeader("Access-Control-Allow-Credentials", "true");
    res.setHeader("Vary", "Origin");
  };
  if (mode === "auth") {
    const [token, ...f] = rest;
    file = f;
    cors();
    if (token !== "fresh") return res.writeHead(404).end("not found");
  } else if (mode === "abr") {
    cors();
    const out = await abr(rest[0]!, rest.slice(1), url).catch(() => null);
    if (!out) return res.writeHead(404).end("not found");
    return res.writeHead(200, { "Content-Type": types[out[0]]!, "Cache-Control": "no-store" }).end(out[1]);
  } else if (mode === "cors" || mode === "missing" || mode === "busy") cors();
  else if (mode === "nocors") {
    if (url.pathname.endsWith(".m3u8")) cors();
  } else return res.writeHead(404).end();
  if (mode === "missing") return res.writeHead(404).end("not found");
  if (mode === "busy") return res.writeHead(429, { "Retry-After": "5" }).end("rate limited");
  const p = path.join(root, ...file);
  if (!p.startsWith(root)) return res.writeHead(400).end();
  try {
    const body = await readFile(p);
    res.writeHead(200, { "Content-Type": types[path.extname(p)] ?? "application/octet-stream", "Cache-Control": "no-store" }).end(body);
  } catch {
    res.writeHead(404).end("not found");
  }
}).listen(port, "127.0.0.1");
