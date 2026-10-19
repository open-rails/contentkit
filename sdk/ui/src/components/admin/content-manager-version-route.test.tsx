// @vitest-environment jsdom
import "../../test/dom.js";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useEffect, useState } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/index.js";
import type { Content, ContentModel, Version } from "../../client/generated/wire.js";
import { ContentEditor, ContentManager, type ContentEditorProps } from "./content-manager.js";

const edition = { id: "v1", code: "EDITION1" } as Version;
const item = { id: "c1", code: "GALLERY1", kind: "gallery" } as Content;
const model: ContentModel = {
  languages: ["en"], tiers: [], signals: false,
  kinds: { gallery: { route: "g", presentation: "reader", versions: { languages: "one" } } },
  entity_kinds: {},
};

// Exercise the manager/session boundary independently of the reader form.
vi.mock("./reader-content-editor.js", () => ({
  ReaderContentEditor: (p: ContentEditorProps) => {
    const [note, setNote] = useState("");
    const { onLeaveGuard } = p;
    useEffect(() => {
      onLeaveGuard?.(async () => note !== "Keep draft");
      return () => onLeaveGuard?.(undefined);
    }, [note, onLeaveGuard]);
    return <>
      <input aria-label="Draft note" value={note} onChange={(e) => setNote(e.target.value)} />
      <span data-testid="version-route">{p.versionCode ?? "none"}</span>
      <a href={p.versionHref?.(edition)}>Edition link</a>
      <button onClick={() => p.onVersionChange?.(edition, { replace: true })}>Canonicalize edition</button>
      <button onClick={() => p.onVersionChange?.(null, { replace: true })}>Drop last edition</button>
      <button onClick={() => p.onCreated?.(item)}>Allocate item</button>
    </>;
  },
}));

function server(contentModel = model) {
  const calls: URL[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(String(input), "https://test.invalid");
    calls.push(url);
    if (url.pathname === "/v1/kinds") return Response.json(contentModel);
    if (url.pathname === "/v1/manage" || url.pathname === "/v1/manage/gallery") return Response.json([item]);
    return Response.json({ code: "not_found", error: "Not found" }, { status: 404 });
  });
  return { calls, client: createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch }) };
}

it("resolves only the parent code for a nested edition URL and preserves canonical navigation", async () => {
  const s = server(), navigate = vi.fn();
  render(<ContentManager client={s.client} kind="gallery" basePath="/admin/galleries"
    navigation={{ pathname: "/admin/galleries/GALLERY1/versions/EDITION1", search: "", navigate }} />);
  expect(await screen.findByTestId("version-route")).toHaveTextContent("EDITION1");
  expect(s.calls.filter((url) => url.searchParams.has("code")).map((url) => url.searchParams.get("code"))).toEqual(["GALLERY1"]);
  expect(screen.getByRole("link", { name: "Edition link" })).toHaveAttribute("href", "/admin/galleries/GALLERY1/versions/EDITION1");
  fireEvent.click(screen.getByRole("button", { name: "Canonicalize edition" }));
  expect(navigate).toHaveBeenCalledWith("/admin/galleries/GALLERY1/versions/EDITION1", { replace: true });
  fireEvent.click(screen.getByRole("button", { name: "Drop last edition" }));
  expect(navigate).toHaveBeenLastCalledWith("/admin/galleries/GALLERY1", { replace: true });
});

it("checks the editor's leave guard before returning to the content list", async () => {
  const s = server(), navigate = vi.fn();
  render(<ContentManager client={s.client} kind="gallery" basePath="/admin/galleries"
    navigation={{ pathname: "/admin/galleries/GALLERY1/versions/EDITION1", search: "", navigate }} />);
  fireEvent.change(await screen.findByLabelText("Draft note"), { target: { value: "Keep draft" } });
  fireEvent.click(screen.getByRole("button", { name: "All items" }));
  await waitFor(() => expect(navigate).not.toHaveBeenCalled());
  fireEvent.change(screen.getByLabelText("Draft note"), { target: { value: "Discard accepted" } });
  fireEvent.click(screen.getByRole("button", { name: "All items" }));
  await waitFor(() => expect(navigate).toHaveBeenCalledWith("/admin/galleries", {}));
});

it.each(["GALLERY1/other/EDITION1", "GALLERY1/versions/EDITION1/extra", "GALLERY1/versions/%2F", "%zz", "new/versions/EDITION1"])(
  "refuses malformed editor path %s without resolving it as a content code", async (view) => {
    const s = server();
    render(<ContentManager client={s.client} kind="gallery" basePath="/admin/galleries"
      navigation={{ pathname: `/admin/galleries/${view}`, search: "", navigate: vi.fn() }} />);
    expect(await screen.findByText("This item doesn't exist or isn't yours to manage.")).toBeVisible();
    expect(s.calls.some((url) => url.searchParams.has("code"))).toBe(false);
    expect(screen.queryByLabelText("Draft note")).toBeNull();
  },
);

it("preserves the draft while canonicalizing its first edition, and resets on explicit edition changes", async () => {
  const s = server();
  const props = { client: s.client, kind: "gallery", id: "c1" };
  const view = render(<ContentEditor {...props} />);
  fireEvent.change(await screen.findByLabelText("Draft note"), { target: { value: "Upload in progress" } });
  view.rerender(<ContentEditor {...props} versionCode="EDITION1" />);
  expect(screen.getByLabelText("Draft note")).toHaveValue("Upload in progress");
  view.rerender(<ContentEditor {...props} versionCode="EDITION2" />);
  await waitFor(() => expect(screen.getByLabelText("Draft note")).toHaveValue(""));
  fireEvent.change(screen.getByLabelText("Draft note"), { target: { value: "Second edition draft" } });
  view.rerender(<ContentEditor {...props} />);
  expect(screen.getByLabelText("Draft note")).toHaveValue("");
});

it("keeps an allocated new draft through its item and first-edition URL adoption", async () => {
  const s = server();
  const props = { client: s.client, kind: "gallery" };
  const view = render(<ContentEditor {...props} id={null} />);
  fireEvent.change(await screen.findByLabelText("Draft note"), { target: { value: "New upload" } });
  fireEvent.click(screen.getByRole("button", { name: "Allocate item" }));
  view.rerender(<ContentEditor {...props} id="c1" versionCode="EDITION1" />);
  expect(screen.getByLabelText("Draft note")).toHaveValue("New upload");
});

it("refuses nested edition routes for presentations without a selected-edition editor", async () => {
  const s = server({ ...model, kinds: { gallery: { ...model.kinds.gallery!, presentation: "post" } } });
  render(<ContentEditor client={s.client} kind="gallery" id="c1" versionCode="EDITION1" />);
  expect(await screen.findByText("This item doesn't exist or isn't yours to manage.")).toBeVisible();
  expect(screen.queryByLabelText("Draft note")).toBeNull();
});
