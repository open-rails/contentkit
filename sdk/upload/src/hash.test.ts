import { createHash } from "node:crypto";
import { expect, it } from "vitest";
import { bytes } from "../test/fake.js";
import { sha256Hex } from "./hash.js";
import { streamSha256 } from "./hash-stream.js";

const hex = (b: Uint8Array) => createHash("sha256").update(b).digest("hex");

it("hashes like node:crypto, natively and streamed across chunk boundaries", async () => {
  for (const n of [0, 1, 4 << 20, (4 << 20) + 1, 9 << 20]) {
    const b = bytes(n, n + 1);
    const seen: number[] = [];
    expect(await sha256Hex(new Blob([b]), { onProgress: (h) => seen.push(h) })).toBe(hex(b));
    if (n > 0) expect(seen.at(-1)).toBe(n);
    const streamed: number[] = [];
    expect(await streamSha256(new Blob([b]), (h) => streamed.push(h))).toBe(hex(b));
    expect(streamed).toEqual(Array.from({ length: Math.ceil(n / (4 << 20)) }, (_, i) => Math.min((i + 1) * (4 << 20), n)));
  }
});

it("streams a file past 64 MiB without workers, and stops when aborted", async () => {
  const b = bytes((64 << 20) + 5, 3);
  expect(await sha256Hex(new Blob([b]))).toBe(hex(b));
  const ctl = new AbortController();
  const err = await sha256Hex(new Blob([b]), { signal: ctl.signal, onProgress: () => ctl.abort() }).catch((e) => e);
  expect(err.code).toBe("aborted");
});
