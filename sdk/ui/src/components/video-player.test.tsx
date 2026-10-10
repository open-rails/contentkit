// @vitest-environment jsdom
import "../test/dom.js";
import { expect, it } from "vitest";
import { de } from "../locales/de.js";
import { en } from "../locales/en.js";
import { es } from "../locales/es.js";
import { ja } from "../locales/ja.js";
import { ko } from "../locales/ko.js";
import { zh } from "../locales/zh.js";
import { keyAllowed } from "./video-player.js";

it("shortcut keys skip text entry, menus and a slider's arrows; page-wide, other controls keep their keys", () => {
  document.body.innerHTML = `
    <div id="player">
      <button id="fs">Fullscreen</button>
      <input id="seek" type="range" />
      <div role="menu"><div id="item" role="menuitemradio">720p</div></div>
    </div>
    <input id="search" type="search" />
    <textarea id="comment"></textarea>
    <div contenteditable="true" id="rich"></div>
    <button id="like">Like</button>
    <a id="link" href="#">x</a>
    <div role="dialog"><p id="modal">Report</p></div>
    <main><p id="text">article</p></main>`;
  const root = document.getElementById("player")!;
  const at = (id: string, key: string) => keyAllowed(document.getElementById(id), key, root);
  expect(at("fs", " ")).toBe(true);
  expect(at("fs", "k")).toBe(true);
  expect(at("seek", "ArrowLeft")).toBe(false);
  expect(at("seek", "End")).toBe(false);
  expect(at("seek", "m")).toBe(true);
  expect(at("item", "ArrowDown")).toBe(false);
  expect(at("item", "k")).toBe(false);
  expect(at("search", "k")).toBe(false);
  expect(at("comment", " ")).toBe(false);
  expect(at("rich", "f")).toBe(false);
  expect(at("like", " ")).toBe(false);
  expect(at("link", "k")).toBe(false);
  expect(at("modal", " ")).toBe(false);
  expect(at("text", "k")).toBe(true);
  expect(keyAllowed(document.body, "k", root)).toBe(true);
  expect(keyAllowed(null, "k", root)).toBe(true);
});

it("every player string is in every bundle", () => {
  const keys = (o: object, p = ""): string[] => Object.entries(o).flatMap(([k, v]) => (typeof v === "string" ? [`${p}${k}`] : keys(v as object, `${p}${k}.`)));
  const want = keys(en.player);
  for (const [lang, b] of Object.entries({ de, es, ja, ko, zh })) expect(keys(b.player ?? {}), lang).toEqual(expect.arrayContaining(want));
});
