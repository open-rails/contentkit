import { expect, it } from "vitest";
import { nextSpeed, parseSpriteVtt, pickTrack, playedBetween, playerKeyAction, readVolume, resumeAt, spriteCueAt } from "./player.js";

it("maps keys to player actions as YouTube does; modified keys are the browser's", () => {
  const k = (key: string, mods = {}) => playerKeyAction({ key, ...mods });
  expect(k(" ")).toEqual({ type: "toggle" });
  expect(k("K")).toEqual({ type: "toggle" });
  expect(k("ArrowLeft")).toEqual({ type: "seekBy", seconds: -5 });
  expect(k("ArrowRight")).toEqual({ type: "seekBy", seconds: 5 });
  expect(k("j")).toEqual({ type: "seekBy", seconds: -10 });
  expect(k("l")).toEqual({ type: "seekBy", seconds: 10 });
  expect(k("ArrowUp")).toEqual({ type: "volumeBy", delta: 0.05 });
  expect(k("ArrowDown")).toEqual({ type: "volumeBy", delta: -0.05 });
  expect(["m", "c", "f", "t", "i"].map((x) => k(x)?.type)).toEqual(["mute", "captions", "fullscreen", "theater", "miniPlayer"]);
  expect(k("7")).toEqual({ type: "seekTo", fraction: 0.7 });
  expect(k("0")).toEqual({ type: "seekTo", fraction: 0 });
  expect(k("Home")).toEqual({ type: "seekTo", fraction: 0 });
  expect(k("End")).toEqual({ type: "seekTo", fraction: 1 });
  expect(k("<")).toEqual({ type: "speed", step: -1 });
  expect(k(".")).toEqual({ type: "speed", step: 1 });
  expect(k("x")).toBeNull();
  expect(k("Enter")).toBeNull();
  expect(k("f", { ctrlKey: true })).toBeNull();
  expect(k("l", { metaKey: true })).toBeNull();
  expect(k("ArrowLeft", { altKey: true })).toBeNull();
});

it("steps the playback speed along the list, clamped; an odd rate snaps to its nearest", () => {
  expect(nextSpeed(1, 1)).toBe(1.25);
  expect(nextSpeed(1, -1)).toBe(0.75);
  expect(nextSpeed(2, 1)).toBe(2);
  expect(nextSpeed(0.25, -1)).toBe(0.25);
  expect(nextSpeed(1.1, 1)).toBe(1.25);
});

it("resumes at the point given, from the start when there is none or it is at the end", () => {
  expect(resumeAt(undefined, 600)).toBe(0);
  expect(resumeAt(-3, 600)).toBe(0);
  expect(resumeAt(Number.NaN, 600)).toBe(0);
  expect(resumeAt(125.5, 600)).toBe(125.5);
  expect(resumeAt(125.5)).toBe(125.5);
  expect(resumeAt(590, 600)).toBe(0); // within 2% of a 10-minute end
  expect(resumeAt(586, 600)).toBe(586);
  expect(resumeAt(22, 24)).toBe(0); // within 3 s of a short clip's end
  expect(resumeAt(20, 24)).toBe(20);
});

it("parses a sprite.vtt into tiles resolved against the cue sheet, and finds the tile for a time", () => {
  const vtt = "WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nsprite.jpg#xywh=0,0,160,90\n\n00:00:02.000 --> 00:00:04.000\nsprite.jpg#xywh=160,0,160,90\n\n01:00:04.000 --> 01:00:06.500\nhttps://cdn/s2.jpg#xywh=0,90,160,90\n";
  const cues = parseSpriteVtt(vtt, "https://m/v1/item/hls/sprite.vtt?t=tok");
  expect(cues).toEqual([
    { start: 0, end: 2, url: "https://m/v1/item/hls/sprite.jpg", x: 0, y: 0, w: 160, h: 90 },
    { start: 2, end: 4, url: "https://m/v1/item/hls/sprite.jpg", x: 160, y: 0, w: 160, h: 90 },
    { start: 3604, end: 3606.5, url: "https://cdn/s2.jpg", x: 0, y: 90, w: 160, h: 90 },
  ]);
  expect(spriteCueAt(cues, 0)?.x).toBe(0);
  expect(spriteCueAt(cues, 2)?.x).toBe(160);
  expect(spriteCueAt(cues, 3.99)?.x).toBe(160);
  expect(spriteCueAt(cues, 9999)?.url).toBe("https://cdn/s2.jpg");
  expect(spriteCueAt([], 3)).toBeUndefined();
  expect(parseSpriteVtt("WEBVTT\n\nnot a cue", "https://m/")).toEqual([]);
});

it("picks a track by exact language tag, else by its primary language", () => {
  const tracks = [{ lang: "en-GB" }, { lang: "pt-BR" }, { lang: "pt" }, {}];
  expect(pickTrack(tracks, "pt")).toBe(2);
  expect(pickTrack(tracks, "PT-br")).toBe(1);
  expect(pickTrack(tracks, "en-US")).toBe(0);
  expect(pickTrack(tracks, "ja")).toBe(-1);
  expect(pickTrack(tracks, null)).toBe(-1);
  expect(pickTrack(tracks, undefined)).toBe(-1);
});

it("counts played time only where it both elapsed and advanced the playhead", () => {
  expect(playedBetween({ at: 0, pos: 10 }, { at: 1000, pos: 11 })).toBe(1);
  expect(playedBetween({ at: 0, pos: 10 }, { at: 1000, pos: 70 })).toBe(1); // a seek forward
  expect(playedBetween({ at: 0, pos: 10 }, { at: 1000, pos: 2 })).toBe(0); // a seek back
  expect(playedBetween({ at: 0, pos: 10 }, { at: 5000, pos: 10 })).toBe(0); // stalled
  expect(playedBetween({ at: 0, pos: 10 }, { at: 2000, pos: 14 })).toBe(2); // 2× speed counts wall time
});

it("reads volume preferences with safe defaults when storage is absent", () => {
  expect(readVolume("ckui.player.volume")).toEqual({ volume: 1, muted: false });
  expect(readVolume(null)).toEqual({ volume: 1, muted: false });
});
