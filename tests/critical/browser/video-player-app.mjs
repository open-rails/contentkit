import React from "../../../sdk/ui/node_modules/react/index.js";
import { createRoot } from "../../../sdk/ui/node_modules/react-dom/client.js";
import { createContentKitClient } from "../../../sdk/ui/dist/client.js";
import { useHlsPlayer } from "../../../sdk/ui/dist/react.js";
const root = createRoot(document.querySelector("#root"));
// The shipped player hook as a host mounts it: its state is recorded.
function Player({ src, loader }) {
  const p = useHlsPlayer({ src, loader, qualityKey: null, volumeKey: null, trackKey: null });
  window.player = { status: p.status, error: p.error ?? null, levels: p.quality.levels, current: p.quality.current, play: p.play };
  return React.createElement("video", { ref: p.ref, playsInline: true, style: { width: 640, height: 360 } });
}
window.hdrPlayer = {
  open(fixture) {
    const client = createContentKitClient({ baseUrl: "/content", token: () => fixture.token });
    root.render(React.createElement(Player, { src: `${client.media.hlsBase(fixture.ref, "streams/")}master.m3u8?editor`, loader: client.media.hls }));
  },
};
