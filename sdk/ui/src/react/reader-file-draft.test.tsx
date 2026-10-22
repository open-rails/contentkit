// @vitest-environment jsdom
import "../test/dom.js";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import type { ContentKitClient } from "../client/client.js";
import type { FileInfo, Op } from "../client/generated/wire.js";
import { DraftHistoryProvider, useDraftHistory } from "./draft-history.js";
import type { UseMediaFolder } from "./folder.js";
import { useReaderFileDraft } from "./reader-file-draft.js";
import { useDraft } from "./use-draft.js";

const pages: FileInfo[] = ["cover.png", "00a.png", "06-07.png"].map((name) => ({ path: `pages/${name}`, type: "image/png", upload: true }));
const rule = { path: "pages/{name}", page: true, types: ["image/png"], max_bytes: 100 };
const titleSeed = { title: "Original" };

function fixture(fail = false) {
  const stored = pages.map((file) => ({ ...file }));
  const commit = vi.fn(async (_ref: unknown, ops: Op[]) => {
    if (fail) throw new Error("save failed");
    for (const op of ops) {
      const index = stored.findIndex((file) => file.path === op.path);
      if (op.op === "remove") stored.splice(index, 1);
      if (op.op === "rename") stored[index] = { ...stored[index]!, path: op.to! };
      if (op.op === "move") stored.splice(op.index!, 0, stored.splice(index, 1)[0]!);
    }
    return stored.map((file) => ({ ...file }));
  });
  const read = vi.fn(async () => ({ files: stored, total: stored.length, limit: 200, offset: 0, uploads: [rule] }));
  const client = { media: { commit, read } } as unknown as ContentKitClient;
  const source = { ref: { kind: "gallery", id: "gallery", version: "edition" }, groups: [{ path: rule.path, rule, files: pages }], read: { reload: vi.fn() } } as unknown as UseMediaFolder;
  function Editor() {
    const history = useDraftHistory()!;
    const title = useDraft(titleSeed);
    const files = useReaderFileDraft(source, client);
    return <>
      <input aria-label="Title" value={title.value.title} onChange={(e) => title.edit({ title: e.target.value })} />
      <output data-testid="order">{files.folder.groups[0]!.files.map((f) => files.folder.displayNames?.[f.path] ?? f.path.split("/").at(-1)).join(",")}</output>
      <output data-testid="dirty">{String(files.dirty)}</output>
      <button disabled={files.folder.updating} onClick={() => void files.folder.move(pages[0]!.path, 2)}>Move</button>
      <button disabled={files.folder.updating} onClick={() => void files.folder.remove([pages[1]!.path])}>Remove</button>
      <button disabled={files.folder.updating} onClick={() => void files.folder.rename(pages[2]!.path, "spread.png")}>Rename</button>
      <button disabled={!history.canUndo} onClick={history.undo}>Undo</button>
      <button disabled={!history.canRedo} onClick={history.redo}>Redo</button>
      <button onClick={async () => { const settle = files.checkpoint(); try { await files.commit(); settle(); history.clear(); } catch { /* edits/history stay */ } }}>Save</button>
    </>;
  }
  render(<DraftHistoryProvider><Editor /></DraftHistoryProvider>);
  return { commit, read };
}

it("stages reorder, deletion and filename changes locally and commits once on Save", async () => {
  const { commit, read } = fixture();
  fireEvent.click(screen.getByText("Move"));
  expect(screen.getByTestId("order")).toHaveTextContent("00a.png,06-07.png,cover.png");
  expect(commit).not.toHaveBeenCalled();
  expect(read).not.toHaveBeenCalled();
  fireEvent.click(screen.getByText("Undo"));
  expect(screen.getByTestId("order")).toHaveTextContent("cover.png,00a.png,06-07.png");
  expect(screen.getByTestId("dirty")).toHaveTextContent("false");
  fireEvent.click(screen.getByText("Redo"));
  fireEvent.click(screen.getByText("Remove"));
  fireEvent.click(screen.getByText("Rename"));
  expect(screen.getByTestId("order")).toHaveTextContent("spread.png,cover.png");
  expect(commit).not.toHaveBeenCalled();
  fireEvent.click(screen.getByText("Save"));
  await waitFor(() => expect(commit).toHaveBeenCalledOnce());
  expect(commit.mock.calls[0]![1]).toEqual([
    { op: "remove", path: "pages/00a.png" },
    { op: "rename", path: "pages/06-07.png", to: "pages/spread.png" },
    { op: "move", path: "pages/spread.png", index: 0 },
  ]);
  await waitFor(() => expect(screen.getByText("Undo")).toBeDisabled());
});

it("keeps shared undo/redo and dirty drafts after a failed save", async () => {
  const { commit } = fixture(true);
  fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Changed" } });
  fireEvent.click(screen.getByText("Move"));
  fireEvent.click(screen.getByText("Save"));
  await waitFor(() => expect(commit).toHaveBeenCalledOnce());
  expect(screen.getByTestId("dirty")).toHaveTextContent("true");
  fireEvent.click(screen.getByText("Undo"));
  expect(screen.getByTestId("order")).toHaveTextContent("cover.png,00a.png,06-07.png");
  fireEvent.click(screen.getByText("Undo"));
  expect(screen.getByLabelText("Title")).toHaveValue("Original");
  fireEvent.click(screen.getByText("Redo"));
  expect(screen.getByLabelText("Title")).toHaveValue("Changed");
});

it.each<[string, FileInfo[]]>([
  ["reordered", [...pages].reverse()],
  ["removed", pages.slice(1)],
  ["renamed", [{ ...pages[0]!, path: "pages/external.png" }, ...pages.slice(1)]],
])("refuses a stale save after files were %s elsewhere", async (_reason, changed) => {
  const { commit, read } = fixture();
  fireEvent.click(screen.getByText("Move"));
  read.mockResolvedValueOnce({ files: changed, total: changed.length, limit: 200, offset: 0, uploads: [rule] });
  fireEvent.click(screen.getByText("Save"));
  await waitFor(() => expect(read).toHaveBeenCalledOnce());
  expect(commit).not.toHaveBeenCalled();
  expect(screen.getByTestId("dirty")).toHaveTextContent("true");
  expect(screen.getByText("Undo")).toBeEnabled();
});

it("blocks file edits until a confirmed commit is visible in the refreshed read", async () => {
  const { commit } = fixture();
  fireEvent.click(screen.getByText("Move"));
  fireEvent.click(screen.getByText("Save"));
  await waitFor(() => expect(screen.getByText("Undo")).toBeDisabled());
  expect(screen.getByText("Remove")).toBeDisabled();
  expect(screen.getByText("Rename")).toBeDisabled();
  fireEvent.click(screen.getByText("Rename"));
  expect(commit).toHaveBeenCalledOnce();
});
