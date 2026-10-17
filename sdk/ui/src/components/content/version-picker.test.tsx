// @vitest-environment jsdom
import "../../test/dom.js";
import { act, render, renderHook, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { Version } from "../../client/generated/wire.js";
import { ContentKitProvider, type Navigate } from "../../react/index.js";
import type { PlayerMenuItem } from "../video-player.js";
import { LanguageFallbackNotice, VersionPicker, useVersionMenuItems } from "./paywall.js";

const version = (id: string, code: string, languages: string[], traits: string[] = []): Version => ({
  id,
  code,
  position: 0,
  languages,
  traits,
  live: true,
  has_audio: false,
  attributes: {},
  segments: [],
  decision: { allowed: true },
});
const versions = [version("v1", "AB12", ["en"]), version("v2", "CD34", ["en"], ["colored"]), version("v3", "EF56", ["ja"])];
const page = "/en/c/G4VRQ3ZQ5/atlas";

const wrap = (o: { language?: string; navigate?: Navigate } = {}) =>
  function Wrapper({ children }: { children: ReactNode }) {
    const client = createContentKitClient({ baseUrl: "", fetch: vi.fn() });
    return (
      <ContentKitProvider client={client} language={o.language} navigate={o.navigate}>
        {children}
      </ContentKitProvider>
    );
  };

beforeEach(() => history.replaceState(null, "", page));

it("shows the page's ?v= version and writes the chosen one's code to ?v=", async () => {
  history.replaceState(null, "", `${page}?p=2&v=cd34#top`);
  render(<VersionPicker versions={versions} />, { wrapper: wrap({ language: "en" }) });
  expect(screen.getByRole("radio", { name: /Colored/ })).toHaveAttribute("aria-checked", "true");
  await userEvent.click(screen.getByRole("radio", { name: /English/ }));
  expect(location.pathname + location.search + location.hash).toBe(`${page}?p=2&v=AB12#top`);
  expect(screen.getByRole("radio", { name: /English/ })).toHaveAttribute("aria-checked", "true");
});

it("writes ?v= through the host router's navigate, as a replace", async () => {
  const navigate = vi.fn<Navigate>();
  render(<VersionPicker versions={versions} />, { wrapper: wrap({ navigate }) });
  await userEvent.click(screen.getByRole("radio", { name: /Colored/ }));
  expect(navigate).toHaveBeenCalledWith(`${page}?v=CD34`, { replace: true });
});

it("an unknown ?v= shows the reader's language's default; the player menu writes ?v= too", () => {
  history.replaceState(null, "", `${page}?v=ZZ99`);
  const { result } = renderHook(() => useVersionMenuItems(versions) as Extract<PlayerMenuItem, { options: unknown }>[], { wrapper: wrap({ language: "ja" }) });
  expect(result.current.map((m) => m.value)).toEqual(["ja"]);
  act(() => result.current[0]!.onChange("en"));
  expect(location.search).toBe("?v=AB12");
  expect(result.current.map((m) => m.value)).toEqual(["en", "v1"]);
});

it("value and onChange take the choice over", async () => {
  const onChange = vi.fn();
  render(<VersionPicker versions={versions} value="v2" onChange={onChange} />, { wrapper: wrap() });
  await userEvent.click(screen.getByRole("radio", { name: /English/ }));
  expect(onChange).toHaveBeenCalledWith(versions[0]);
  expect(location.search).toBe("");
});

it("without ?v= opens the reader's language's first version by position", () => {
  const sorted = [{ ...versions[0]!, position: 1 }, { ...versions[1]!, position: 0 }, { ...versions[2]!, position: 2 }];
  render(<VersionPicker versions={sorted} />, { wrapper: wrap({ language: "en" }) });
  expect(screen.getByRole("radio", { name: /Colored/ })).toHaveAttribute("aria-checked", "true");
});

it("notes when the page's language isn't served, showing the fallback and each served language", async () => {
  const onChange = vi.fn();
  const { rerender } = render(<LanguageFallbackNotice versions={versions} language="de" onChange={onChange} />, { wrapper: wrap({ language: "de" }) });
  expect(screen.getByRole("note")).toHaveTextContent("Not available in German, showing English.");
  expect(screen.getByRole("button", { name: "English" })).toHaveAttribute("aria-pressed", "true");
  await userEvent.click(screen.getByRole("button", { name: "Japanese" }));
  expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ id: "v3" }));
  rerender(<LanguageFallbackNotice versions={versions} language="en" />);
  expect(screen.queryByRole("note")).toBeNull();
});
