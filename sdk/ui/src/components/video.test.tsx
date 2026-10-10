// @vitest-environment jsdom
import "../test/dom.js";
import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { VideoPoster } from "../index.js";

// Client-free: the poster picker against the real ContentKit is in e2e/integration/video.test.tsx.
const poster = { preset: "poster", aspect: "16:9", renditions: [480, 960].map((w) => ({ url: `https://cdn/v1/app/post/x/public/poster-${w}-generation.webp`, w, h: (w * 9) / 16 })) };

function reducedMotion(on: boolean) {
  window.matchMedia = vi.fn().mockImplementation((q: string) => ({ matches: on && q.includes("reduce"), addEventListener() {}, removeEventListener() {} }));
}

it("VideoPoster: the video's aspect from the preset, uncropped", () => {
  const tall = { ...poster, renditions: [{ url: poster.renditions[0]!.url, w: 480, h: 853 }], aspect: "480:853" };
  const { container } = render(<VideoPoster poster={tall} alt="tall" />);
  expect(container.firstElementChild).toHaveStyle({ aspectRatio: String(480 / 853) });
  expect(screen.getByRole("img", { name: "tall" })).toHaveClass("object-contain");
});

it("VideoPoster: srcset at the poster's aspect; a cover only, it never plays", () => {
  reducedMotion(false);
  const { container } = render(
    <VideoPoster poster={poster} alt="clip">
      <a href="#post">open</a>
    </VideoPoster>,
  );
  const root = container.firstElementChild!;
  expect(root).toHaveClass("ckui");
  expect(root).toHaveStyle({ aspectRatio: String(16 / 9) });
  expect(screen.getByRole("img", { name: "clip" })).toHaveAttribute("src", poster.renditions[0]!.url);
  fireEvent.pointerEnter(root);
  fireEvent.focus(screen.getByRole("link"));
  expect(container.querySelector("video")).toBeNull();
});
