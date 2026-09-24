import { expect, it } from "vitest";
import { classifyHlsError, classifyMediaError, hlsConfig } from "./playback.js";

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
