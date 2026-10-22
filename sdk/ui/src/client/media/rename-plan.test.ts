import { expect, it } from "vitest";
import { planFileRenames } from "./rename-plan.js";

it("vacates destinations before renaming the source", () => {
  expect(planFileRenames(["pages/1.png", "pages/01.png"], { "pages/1.png": "pages/01.png", "pages/01.png": "pages/01 (2).png" })).toEqual([
    { op: "rename", path: "pages/01.png", to: "pages/01 (2).png" },
    { op: "rename", path: "pages/1.png", to: "pages/01.png" },
  ]);
});

it("resolves swaps using only temporary manifest renames", () => {
  expect(planFileRenames(["pages/A.png", "pages/B.png"], { "pages/A.png": "pages/B.png", "pages/B.png": "pages/A.png" })).toEqual([
    { op: "rename", path: "pages/A.png", to: "pages/__ck_rename_1.png" },
    { op: "rename", path: "pages/B.png", to: "pages/A.png" },
    { op: "rename", path: "pages/__ck_rename_1.png", to: "pages/B.png" },
  ]);
});

it("refuses case-insensitive collisions with unchanged filenames", () => {
  expect(() => planFileRenames(["pages/A.png", "pages/b.png"], { "pages/A.png": "pages/B.png" })).toThrow("already used");
  expect(planFileRenames(["pages/A.png"], { "pages/A.png": "pages/a.png" })).toEqual([{ op: "rename", path: "pages/A.png", to: "pages/a.png" }]);
});

it("refuses vanished rename sources instead of dropping the edit", () => {
  expect(() => planFileRenames(["pages/B.png"], { "pages/A.png": "pages/C.png" })).toThrow("changed on the server");
});
