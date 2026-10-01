import { streamSha256 } from "./hash-stream.js";

/** Hashes the posted Blob: {hashed} as it goes, then {hex} or {error}. */
const scope = self as unknown as { onmessage: (e: MessageEvent<Blob>) => void; postMessage: (m: unknown) => void };
scope.onmessage = (e) => {
  streamSha256(e.data, (hashed) => scope.postMessage({ hashed })).then(
    (hex) => scope.postMessage({ hex }),
    (err: unknown) => scope.postMessage({ error: String(err) }),
  );
};
