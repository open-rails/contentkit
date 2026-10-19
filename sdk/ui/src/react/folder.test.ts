import { expect, it } from "vitest";
import type { FileInfo } from "../client/generated/wire.js";
import { leftoverUploads } from "./folder.js";

const upload = (path: string, create_id?: string): FileInfo => ({ path, create_id, type: "image/png", upload: true, unattached: true, size: 4 });

it("leaves out staged copies the queue holds by path or by create ID, and keeps an earlier visit's", () => {
  const items = [{ id: "page-1", path: "pages/001.png" }, { id: "page-3", path: "pages/001.png" }];
  const unattached = [upload("pages/001.png", "page-1"), upload("pages/003.png", "page-3"), upload("pages/007.png", "earlier")];
  expect(leftoverUploads(unattached, items).map((f) => f.path)).toEqual(["pages/007.png"]);
});
