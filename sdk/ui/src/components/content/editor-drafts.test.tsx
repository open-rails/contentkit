// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/client.js";
import type { Content, ContentModel, ContentPatch } from "../../client/generated/wire.js";
import { useContent } from "../../react/contents.js";
import { ContentKitProvider } from "../../react/provider.js";
import { ContentSections } from "./editors.js";

const at = "2026-10-16T00:00:00Z";
const content = (id = "p1"): Content => ({
  id, kind: "post", code: id, path: `/p/${id}`, language: "en", title: id,
  decision: { allowed: true }, state: "draft", comments: "open",
  live: false, access: { level: "public" }, restrictions: [], attributes: { caption: `Stored ${id}` },
  creators: [], entities: [], versions: [], editor: true, revision: 3, created_at: at, updated_at: at,
});
const model: ContentModel = {
  languages: ["en"], tiers: ["member"], signals: false, entity_kinds: {},
  kinds: { post: { route: "p", presentation: "post", versions: { languages: "one", hidden: true }, attributes: { caption: { type: "string" } } } },
};

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => { resolve = done; });
  return { promise, resolve };
}

function server(customModel = model) {
  const records = new Map(["p1", "p2"].map((id) => [id, content(id)]));
  const writes: { id: string; patch: ContentPatch }[] = [];
  const controls: { readError?: boolean; writeError?: boolean; writeGate?: Promise<void>; readGate?: Promise<void>; modelGate?: Promise<void> } = {};
  const client = createContentKitClient({
    baseUrl: "",
    fetch: async (input, init) => {
      const url = new URL(String(input), "http://test");
      const path = url.pathname.replace(/^\/v1/, "");
      if (path === "/kinds") { await controls.modelGate; return Response.json(customModel); }
      const id = path.match(/^\/contents\/(p[12])$/)?.[1];
      if (!id) return Response.json({ code: "not_found", error: path }, { status: 404 });
      const current = records.get(id)!;
      if (init?.method !== "PATCH") {
        await controls.readGate;
        return controls.readError ? Response.json({ code: "unavailable" }, { status: 503 }) : Response.json(current);
      }
      const patch = JSON.parse(String(init.body)) as ContentPatch;
      writes.push({ id, patch });
      await controls.writeGate;
      if (controls.writeError) return Response.json({ code: "unavailable" }, { status: 503 });
      if (patch.revision !== current.revision) return Response.json({ code: "conflict", error: "Changed elsewhere" }, { status: 409 });
      const next = { ...current, ...patch, revision: current.revision + 1 };
      if (patch.attributes?.caption) next.attributes = { ...patch.attributes, caption: String(patch.attributes.caption).toUpperCase() };
      records.set(id, next);
      return Response.json(next);
    },
  });
  return { client, writes, records, controls };
}

function Sections({ client, id = "p1" }: { client: ContentKitClient; id?: string }) {
  const ed = useContent(id, "post", "en", { client });
  return <>
    <button onClick={ed.reload}>Refresh record</button>
    <output aria-label="Record revision">{ed.content?.revision}</output>
    {ed.content && <ContentSections content={ed.content} media={false} client={client} />}
  </>;
}

function mount(client: ContentKitClient, onError = vi.fn()) {
  const result = render(<ContentKitProvider client={client} viewer="creator" onError={onError}><Sections client={client} /></ContentKitProvider>);
  const section = (name: string) => within(result.container.querySelector<HTMLElement>(`[data-ckui="${name}"]`)!);
  return { ...result, details: () => section("content-details"), access: () => section("access-editor") };
}

async function choose(user: ReturnType<typeof userEvent.setup>, name: string, option: string) {
  await user.click(screen.getByRole("combobox", { name }));
  await user.click(await screen.findByRole("option", { name: option }));
}

it("ContentSections keeps unsaved details across an access save, uses the fresh revision, and settles its own save", async () => {
  const s = server();
  const user = userEvent.setup();
  const ui = mount(s.client);
  const caption = await screen.findByLabelText("Caption");
  await user.clear(caption);
  await user.type(caption, "Draft caption");
  await user.click(ui.details().getByRole("combobox", { name: "Comments" }));
  await user.click(await screen.findByRole("option", { name: "Closed (kept, no new ones)" }));
  await user.click(ui.access().getByRole("combobox", { name: "Who can open it" }));
  await user.click(await screen.findByRole("option", { name: "Buyers" }));
  await user.click(ui.access().getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.getByLabelText("Record revision")).toHaveTextContent("4"));

  expect(caption).toHaveValue("Draft caption");
  expect(ui.details().getByRole("combobox", { name: "Comments" })).toHaveTextContent("Closed");
  await user.click(ui.details().getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.writes.at(-1)?.patch).toEqual({ comments: "closed", attributes: { caption: "Draft caption" }, revision: 4 }));
  expect(await ui.details().findByRole("status")).toHaveTextContent("Saved.");
  expect(caption).toHaveValue("DRAFT CAPTION");

  s.records.set("p1", { ...s.records.get("p1")!, revision: 6, attributes: { caption: "Fresh after own save" } });
  await user.click(screen.getByRole("button", { name: "Refresh record" }));
  await waitFor(() => expect(caption).toHaveValue("Fresh after own save"));
});

it("ContentSections keeps an unsaved access rule across a details save and settles with the current revision", async () => {
  const s = server();
  const user = userEvent.setup();
  const ui = mount(s.client);
  await screen.findByLabelText("Caption");
  await user.click(ui.access().getByRole("combobox", { name: "Who can open it" }));
  await user.click(await screen.findByRole("option", { name: "Tier members" }));
  await user.click(ui.access().getByRole("switch", { name: /Member/ }));
  await user.click(ui.details().getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.getByLabelText("Record revision")).toHaveTextContent("4"));

  expect(ui.access().getByRole("combobox", { name: "Who can open it" })).toHaveTextContent("Tier members");
  expect(ui.access().getByRole("switch", { name: /Member/ })).toBeChecked();
  await user.click(ui.access().getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.writes.at(-1)?.patch).toEqual({ access: { level: "tier", tiers: ["member"] }, revision: 4 }));
  expect(await ui.access().findByRole("status")).toHaveTextContent("Saved.");

  s.records.set("p1", { ...s.records.get("p1")!, revision: 6, access: { level: "public" } });
  await user.click(screen.getByRole("button", { name: "Refresh record" }));
  await waitFor(() => expect(ui.access().getByRole("combobox", { name: "Who can open it" })).toHaveTextContent("Everyone"));
});

it("rebases clean fields and attributes without replacing edited fields", async () => {
  const s = server({ ...model, kinds: { post: { ...model.kinds.post!, attributes: { caption: { type: "string" }, note: { type: "string" } } } } });
  const user = userEvent.setup();
  const ui = mount(s.client);
  const caption = await screen.findByLabelText("Caption");
  await user.clear(caption);
  await user.type(caption, "Keep this draft");
  await choose(user, "Who can open it", "Tier members");
  await user.click(ui.access().getByRole("switch", { name: /Member/ }));

  s.records.set("p1", { ...s.records.get("p1")!, revision: 4, comments: "closed", attributes: { caption: "Remote caption", note: "Remote note" }, access: { level: "public", releases: [{ at: "2027-01-01T00:00:00Z", level: "public" }] } });
  await user.click(screen.getByRole("button", { name: "Refresh record" }));
  await waitFor(() => expect(screen.getByLabelText("Note")).toHaveValue("Remote note"));
  expect(caption).toHaveValue("Keep this draft");
  expect(ui.details().getByRole("combobox", { name: "Comments" })).toHaveTextContent("Closed");
  expect(ui.access().getByRole("combobox", { name: "Who can open it" })).toHaveTextContent("Tier members");
  expect(ui.access().getByRole("switch", { name: /Member/ })).toBeChecked();
  expect(ui.access().getByLabelText("Free for everyone from")).not.toHaveValue("");
  await user.click(ui.details().getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.writes.at(-1)?.patch).toEqual({ comments: "closed", attributes: { caption: "Keep this draft", note: "Remote note" }, revision: 4 }));
});

it("retains drafts on conflict and failed reload, then Cancel reloads only its own section", async () => {
  const s = server();
  const onError = vi.fn();
  const user = userEvent.setup();
  const ui = mount(s.client, onError);
  const caption = await screen.findByLabelText("Caption");
  await user.clear(caption);
  await user.type(caption, "Conflicting draft");
  await choose(user, "Who can open it", "Buyers");
  s.records.set("p1", { ...s.records.get("p1")!, revision: 4, attributes: { caption: "Remote version" } });
  await user.click(ui.details().getByRole("button", { name: "Save" }));
  expect(await ui.details().findByRole("alert")).toHaveTextContent("Reload and try again");
  expect(caption).toHaveValue("Conflicting draft");
  expect(onError).toHaveBeenCalledWith(expect.objectContaining({ code: "conflict" }), expect.objectContaining({ operation: "content.save" }));

  s.controls.readError = true;
  await user.click(ui.details().getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(onError).toHaveBeenCalledTimes(2));
  expect(caption).toHaveValue("Conflicting draft");
  s.controls.readError = false;
  await user.click(ui.details().getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(caption).toHaveValue("Remote version"));
  expect(ui.details().queryByRole("alert")).toBeNull();
  expect(ui.details().queryByRole("status")).toBeNull();
  expect(ui.access().getByRole("combobox", { name: "Who can open it" })).toHaveTextContent("Buyers");
  await user.click(ui.access().getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.writes.at(-1)?.patch).toEqual({ access: { level: "ppv" }, revision: 4 }));
  expect(await ui.access().findByRole("status")).toHaveTextContent("Saved.");
});

it("retains an access draft on write error and Cancel does not discard dirty details", async () => {
  const s = server();
  const user = userEvent.setup();
  const ui = mount(s.client);
  const caption = await screen.findByLabelText("Caption");
  await user.type(caption, " edited");
  await choose(user, "Who can open it", "Buyers");
  s.controls.writeError = true;
  await user.click(ui.access().getByRole("button", { name: "Save" }));
  await ui.access().findByRole("alert");
  expect(ui.access().getByRole("combobox", { name: "Who can open it" })).toHaveTextContent("Buyers");
  expect(ui.access().queryByRole("status")).toBeNull();
  await user.click(ui.access().getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(ui.access().getByRole("combobox", { name: "Who can open it" })).toHaveTextContent("Everyone"));
  expect(caption).toHaveValue("Stored p1 edited");
});

it("keeps edits made during a details save unsaved and uses the response revision on the next save", async () => {
  const s = server();
  const user = userEvent.setup();
  const ui = mount(s.client);
  const caption = await screen.findByLabelText("Caption");
  await user.clear(caption);
  await user.type(caption, "Submitted");
  const gate = deferred();
  s.controls.writeGate = gate.promise;
  await user.click(ui.details().getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.writes).toHaveLength(1));
  await user.clear(caption);
  await user.type(caption, "Later draft");
  gate.resolve();
  await waitFor(() => expect(screen.getByLabelText("Record revision")).toHaveTextContent("4"));
  expect(caption).toHaveValue("Later draft");
  expect(ui.details().queryByRole("status")).toBeNull();
  await user.click(ui.details().getByRole("button", { name: "Save" }));
  expect(await ui.details().findByRole("status")).toHaveTextContent("Saved.");
  expect(s.writes.at(-1)?.patch).toMatchObject({ revision: 4, attributes: { caption: "Later draft" } });
  expect(caption).toHaveValue("LATER DRAFT");
});

it("keeps access edits made during a save unsaved until the next successful save", async () => {
  const s = server();
  const user = userEvent.setup();
  const ui = mount(s.client);
  await screen.findByLabelText("Caption");
  await choose(user, "Who can open it", "Buyers");
  const gate = deferred();
  s.controls.writeGate = gate.promise;
  await user.click(ui.access().getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.writes).toHaveLength(1));
  await choose(user, "Who can open it", "Tier members");
  await user.click(ui.access().getByRole("switch", { name: /Member/ }));
  gate.resolve();
  await waitFor(() => expect(screen.getByLabelText("Record revision")).toHaveTextContent("4"));
  expect(ui.access().getByRole("combobox", { name: "Who can open it" })).toHaveTextContent("Tier members");
  expect(ui.access().getByRole("switch", { name: /Member/ })).toBeChecked();
  expect(ui.access().queryByRole("status")).toBeNull();
  await user.click(ui.access().getByRole("button", { name: "Save" }));
  expect(await ui.access().findByRole("status")).toHaveTextContent("Saved.");
  expect(s.writes.at(-1)?.patch).toEqual({ revision: 4, access: { level: "tier", tiers: ["member"] } });
});

it("seeds late model attributes even after another details field was edited", async () => {
  const s = server();
  const gate = deferred();
  s.controls.modelGate = gate.promise;
  const user = userEvent.setup();
  const ui = mount(s.client);
  await screen.findByRole("combobox", { name: "Comments" });
  await choose(user, "Comments", "Closed (kept, no new ones)");
  gate.resolve();
  expect(await screen.findByLabelText("Caption")).toHaveValue("Stored p1");
  expect(ui.details().getByRole("combobox", { name: "Comments" })).toHaveTextContent("Closed");
});

it("Cancel cannot roll back a sibling save completed while its reload was pending", async () => {
  const s = server();
  const user = userEvent.setup();
  const ui = mount(s.client);
  const caption = await screen.findByLabelText("Caption");
  await user.type(caption, " draft");
  const gate = deferred();
  s.controls.readGate = gate.promise;
  await user.click(ui.details().getByRole("button", { name: "Cancel" }));
  await choose(user, "Who can open it", "Buyers");
  await user.click(ui.access().getByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.getByLabelText("Record revision")).toHaveTextContent("4"));
  gate.resolve();
  await waitFor(() => expect(caption).toHaveValue("Stored p1"));
  expect(screen.getByLabelText("Record revision")).toHaveTextContent("4");
  expect(ui.access().getByRole("combobox", { name: "Who can open it" })).toHaveTextContent("Buyers");
  await user.click(ui.details().getByRole("button", { name: "Save" }));
  expect(await ui.details().findByRole("status")).toHaveTextContent("Saved.");
  expect(s.writes.at(-1)?.patch.revision).toBe(4);
});

it("a late save from the previous client cannot settle a new editor context", async () => {
  const s = server();
  const other = server();
  const user = userEvent.setup();
  const tree = (client: ContentKitClient) => <ContentKitProvider client={client} viewer="creator"><Sections client={client} /></ContentKitProvider>;
  const ui = render(tree(s.client));
  const caption = await screen.findByLabelText("Caption");
  await user.type(caption, " old draft");
  const gate = deferred();
  s.controls.writeGate = gate.promise;
  await user.click(within(ui.container.querySelector<HTMLElement>('[data-ckui="content-details"]')!).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.writes).toHaveLength(1));
  ui.rerender(tree(other.client));
  await waitFor(() => expect(screen.getByLabelText("Caption")).toHaveValue("Stored p1"));
  await user.type(screen.getByLabelText("Caption"), " new draft");
  gate.resolve();
  await waitFor(() => expect(s.records.get("p1")?.revision).toBe(4));
  expect(screen.getByLabelText("Caption")).toHaveValue("Stored p1 new draft");
  expect(within(ui.container.querySelector<HTMLElement>('[data-ckui="content-details"]')!).queryByRole("status")).toBeNull();
  expect(screen.getByLabelText("Record revision")).toHaveTextContent("3");
});

it.each(["item", "client", "viewer", "access revision"])("resets both editor drafts when the %s identity changes", async (change) => {
  const s = server();
  const other = server();
  const user = userEvent.setup();
  const record = content();
  const tree = (client: ContentKitClient, item: Content, viewer = "creator", accessRevision = 0) => <ContentKitProvider client={client} viewer={viewer} accessRevision={accessRevision}><ContentSections content={item} media={false} /></ContentKitProvider>;
  const ui = render(tree(s.client, record));
  const caption = await screen.findByLabelText("Caption");
  await user.type(caption, " draft");
  await choose(user, "Who can open it", "Buyers");
  ui.rerender(tree(change === "client" ? other.client : s.client, change === "item" ? content("p2") : record, change === "viewer" ? "other creator" : "creator", change === "access revision" ? 1 : 0));
  await waitFor(() => expect(screen.getByLabelText("Caption")).toHaveValue(change === "item" ? "Stored p2" : "Stored p1"));
  expect(screen.getByRole("combobox", { name: "Who can open it" })).toHaveTextContent("Everyone");
});
