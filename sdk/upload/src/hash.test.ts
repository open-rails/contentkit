import { createHash } from "node:crypto";
import { expect, it } from "vitest";
import { bytes } from "../test/fake.js";
import { sha256Hex } from "./hash.js";

it("hashes across chunk boundaries like node:crypto", async () => {
  for (const n of [0, 1, 4 << 20, (4 << 20) + 1, 9 << 20]) {
    const b = bytes(n, n + 1);
    const seen: number[] = [];
    const got = await sha256Hex(new Blob([b]), { onProgress: (h) => seen.push(h) });
    expect(got).toBe(createHash("sha256").update(b).digest("hex"));
    if (n > 0) expect(seen.at(-1)).toBe(n);
  }
});
