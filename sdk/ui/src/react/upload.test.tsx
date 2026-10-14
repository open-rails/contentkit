// @vitest-environment jsdom
import { act, renderHook } from "@testing-library/react";
import { expect, it } from "vitest";
import { useCrop, useUpload } from "./index.js";

// Client-side only; the upload hooks against the server are in e2e/integration/upload-hooks.test.tsx.
it("hooks without a client or provider say how to get one", () => {
  expect(() => renderHook(() => useUpload())).toThrow(/ContentKitProvider/);
});

it("useCrop keeps a crop in original pixels at the aspect and yields the edit", () => {
  const source = { width: 400, height: 200 };
  const { result } = renderHook(() => useCrop({ source, aspect: "1:2" }));
  expect(result.current.crop).toEqual({ x: 150, y: 0, w: 100, h: 200 });
  act(() => result.current.setFromDisplay({ x: 100, y: 0, w: 60, h: 999 }, { width: 200, height: 100 }));
  expect(result.current.edit).toEqual({ crop: { x: 200, y: 0, w: 100, h: 200 } });
  act(() => result.current.rotateBy(90));
  expect(result.current.rotate).toBe(90);
  expect(result.current.edit).toEqual({ crop: { x: 200, y: 0, w: 100, h: 50 }, rotate: 90 });
  act(() => result.current.rotateBy(-90));
  act(() => result.current.setCrop({ x: 0, y: 0, w: 1000, h: 0 }));
  expect(result.current.crop).toEqual({ x: 0, y: 0, w: 100, h: 200 });

  const free = renderHook(() => useCrop({ source, initial: { crop: { x: 10, y: 10, w: 50, h: 40 }, rotate: 180 } }));
  expect(free.result.current.edit).toEqual({ crop: { x: 10, y: 10, w: 50, h: 40 }, rotate: 180 });
  act(() => free.result.current.reset());
  expect(free.result.current.edit).toBeNull();
});
