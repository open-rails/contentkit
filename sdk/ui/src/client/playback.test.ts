import { expect, it } from "vitest";
import { abrHlsConfig, capRung, classifyHlsError, classifyMediaError, hlsConfig, initialEstimate, otherCodecLevels, startRung, type Rung } from "./playback.js";

it("classifies hls.js errors by status and detail", () => {
  const net = (details: string, code?: number) => classifyHlsError({ type: "networkError", details, fatal: false, response: code === undefined ? undefined : { code } });
  expect(net("fragLoadError", 0)).toEqual({ kind: "network", code: "fragLoadError/0", status: 0 });
  expect(net("fragLoadError")).toMatchObject({ kind: "network", status: 0 });
  expect(net("manifestLoadError", 401)).toMatchObject({ kind: "access", status: 401 });
  expect(net("levelLoadError", 403)).toMatchObject({ kind: "access", code: "levelLoadError/403" });
  expect(net("manifestLoadError", 404)).toMatchObject({ kind: "not_found" });
  expect(net("fragLoadError", 410)).toMatchObject({ kind: "not_found" });
  expect(net("levelLoadError", 429)).toMatchObject({ kind: "rate_limited", status: 429 });
  expect(net("fragLoadError", 502)).toMatchObject({ kind: "network", status: 502 });
  expect(net("fragLoadTimeOut", 200)).toMatchObject({ kind: "network", code: "fragLoadTimeOut/0" });
  expect(classifyHlsError({ type: "networkError", details: "fragLoadError", networkDetails: { status: 403 } })).toMatchObject({ kind: "access" });
  expect(classifyHlsError({ type: "networkError", details: "manifestParsingError" })).toEqual({ kind: "unsupported", code: "manifestParsingError" });
  expect(classifyHlsError({ type: "mediaError", details: "bufferIncompatibleCodecsError", fatal: true })).toMatchObject({ kind: "unsupported" });
  expect(classifyHlsError({ type: "mediaError", details: "bufferStalledError", fatal: false })).toMatchObject({ kind: "unknown" });
  expect(classifyHlsError({ type: "mediaError", details: "bufferNudgeOnStall", fatal: true })).toMatchObject({ kind: "unsupported" });
});

it("classifies native media errors", () => {
  expect(classifyMediaError({ code: 2 })).toEqual({ kind: "network", code: "media/2" });
  expect(classifyMediaError({ code: 3 }).kind).toBe("unsupported");
  expect(classifyMediaError({ code: 4 }).kind).toBe("unsupported");
  expect(classifyMediaError(null)).toEqual({ kind: "unknown", code: "media/0" });
});

it("fails fast: few quick retries and never on access or absence", () => {
  const frag = hlsConfig.fragLoadPolicy.default;
  expect(frag.errorRetry.maxNumRetry).toBeLessThanOrEqual(2);
  expect(frag.timeoutRetry.maxNumRetry).toBeLessThanOrEqual(1);
  expect(hlsConfig.manifestLoadPolicy.default.maxTimeToFirstByteMs).toBeLessThanOrEqual(5000);
  const should = frag.errorRetry.shouldRetry;
  expect(should(null, 0, false, { code: 403 }, true)).toBe(false);
  expect(should(null, 0, false, { code: 404 }, true)).toBe(false);
  expect(should(null, 0, false, { code: 429 }, true)).toBe(false);
  expect(should(null, 0, false, { code: 0 }, true)).toBe(true);
  expect(should(null, 0, false, undefined, true)).toBe(true);
});

const mbps = [1.2e6, 2.8e6, 5e6, 9e6, 16e6];
const landscape: Rung[] = [
  [854, 480],
  [1280, 720],
  [1920, 1080],
  [2560, 1440],
  [3840, 2160],
].map(([width, height], i) => ({ width: width!, height: height!, bitrate: mbps[i]! }));
const portrait: Rung[] = landscape.map((r) => ({ ...r, width: r.height, height: r.width }));
const H = (rungs: Rung[], i: number) => Math.min(rungs[i]!.width, rungs[i]!.height);

it("caps to the display in device pixels, never below 1080p", () => {
  // 390 CSS px phone at DPR 3: a 16:9 frame is 1170×658 device px; 720p would cover it, 1080p is the floor.
  expect(H(landscape, capRung(landscape, 1170, 658))).toBe(1080);
  // Portrait video filling a portrait phone (390×693 CSS at DPR 3): 1080×1920 is within 10% of 1170×2079.
  expect(H(portrait, capRung(portrait, 1170, 2079))).toBe(1080);
  expect(H(portrait, capRung(portrait, 1290, 2293))).toBe(1440);
  expect(H(portrait, capRung(portrait, 1080, 1920))).toBe(1080);
  // A thumbnail-sized desktop player still allows 1080p.
  expect(H(landscape, capRung(landscape, 320, 180))).toBe(1080);
  // Fullscreen on a 4K display, a 1440p laptop panel, a DPR-2 tablet.
  expect(H(landscape, capRung(landscape, 3840, 2160))).toBe(2160);
  expect(H(landscape, capRung(landscape, 2560, 1440))).toBe(1440);
  expect(H(landscape, capRung(landscape, 2732, 2048))).toBe(1440);
  expect(H(landscape, capRung(landscape, 3200, 2000))).toBe(2160);
  // Portrait video pillarboxed on a landscape 4K screen is 2160 px tall: 1440×2560 covers it.
  expect(H(portrait, capRung(portrait, 3840, 2160))).toBe(1440);
  // Unknown size: no display cap.
  expect(capRung(landscape, 0, 0)).toBe(4);
});

it("honours the policy's floor and ceiling", () => {
  expect(H(landscape, capRung(landscape, 1170, 658, { preferHighRes: false }))).toBe(720);
  expect(H(landscape, capRung(landscape, 3840, 2160, { maxHeight: 1080 }))).toBe(1080);
  expect(H(landscape, capRung(landscape, 1170, 658, { minStartHeight: 1440 }))).toBe(1440);
  // A ladder topping out below the floor allows its top rung.
  expect(capRung(landscape.slice(0, 2), 320, 180)).toBe(1);
  expect(capRung([], 100, 100)).toBe(-1);
});

it("seeds the estimate from this page, the browser, then a high default", () => {
  expect(initialEstimate()).toBe(8e6);
  expect(initialEstimate({ defaultEstimate: 6e6 })).toBe(6e6);
  expect(initialEstimate(undefined, { downlink: 1.5 })).toBe(1.5e6);
  expect(initialEstimate(undefined, { downlink: 10 }, 23e6)).toBe(23e6);
  expect(initialEstimate(undefined, { downlink: 10, saveData: true }, 23e6)).toBe(5e5);
  expect(initialEstimate({ preferHighRes: false })).toBe(5e5);
  expect(abrHlsConfig(undefined, 8e6).abrEwmaDefaultEstimateMax).toBeGreaterThanOrEqual(8e6);
});

it("starts at the highest rung the estimate sustains under the cap", () => {
  const phone = capRung(landscape, 1170, 658);
  // No Network Information (Safari/Firefox): 8 Mbps default starts at 1080p.
  expect(H(landscape, startRung(landscape, initialEstimate(), phone))).toBe(1080);
  // Chromium reports downlink 10 (its ceiling): 1080p, not 1440p, on a phone.
  expect(H(landscape, startRung(landscape, 10e6, phone))).toBe(1080);
  // Fullscreen on 4K with a fast measured link starts at 2160.
  expect(H(landscape, startRung(landscape, 25e6, capRung(landscape, 3840, 2160)))).toBe(2160);
  expect(H(landscape, startRung(landscape, 13e6, capRung(landscape, 3840, 2160)))).toBe(1440);
  // Slow links start low; 1.5 Mbps is below even 480p's peak with headroom.
  expect(H(landscape, startRung(landscape, 1.5e6, phone))).toBe(480);
  expect(H(landscape, startRung(landscape, 4e6, phone))).toBe(720);
  // AVERAGE-BANDWIDTH is what a start has to sustain.
  const avg = landscape.map((r) => ({ ...r, averageBitrate: r.bitrate * 0.6 }));
  expect(H(avg, startRung(avg, 4.5e6, phone))).toBe(1080);
  expect(H(portrait, startRung(portrait, 8e6, capRung(portrait, 1170, 2079)))).toBe(1080);
  // Save-Data starts at the bottom whatever the estimate.
  expect(startRung(landscape, 50e6, 4, { saveData: true })).toBe(0);
});

it("keeps the codec set of the first listed level", () => {
  // hls.js sorts levels by bitrate; firstLevel is the master's first playable variant.
  const levels = [{ codecSet: "av01,mp4a" }, { codecSet: "avc1,mp4a" }, { codecSet: "av01,mp4a" }, { codecSet: "avc1,mp4a" }];
  expect(otherCodecLevels(levels, 2)).toEqual([3, 1]);
  expect(otherCodecLevels(levels, 1)).toEqual([2, 0]);
  // Without AV1 decode hls.js has already dropped the av01 levels: H.264 stays whole.
  expect(otherCodecLevels([{ codecSet: "avc1,mp4a" }, { codecSet: "avc1,mp4a" }, { codecSet: "avc1,mp4a" }], 1)).toEqual([]);
  expect(otherCodecLevels([{ codecSet: "avc1,mp4a" }, { codecSet: "avc1,mp4a" }], 0)).toEqual([]);
  expect(otherCodecLevels([{}, {}], -1)).toEqual([]);
});
