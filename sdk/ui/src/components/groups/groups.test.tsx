// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/index.js";
import type { ContentModel, GroupAccess, GroupDetail, Offer } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/index.js";
import { MembershipBox } from "./banner.js";
import { GroupEditor } from "./editor.js";
import { GroupAvatar, GroupCover } from "./parts.js";

type Handler = (req: { url: URL }) => unknown;

function server(routes: Record<string, Handler>) {
  const calls: string[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://x");
    const key = `${init?.method ?? "GET"} ${url.pathname.replace(/^\/v1/, "")}`;
    calls.push(key);
    const h = routes[key];
    if (!h) return Response.json({ error: "not found", code: "not_found" }, { status: 404 });
    const out = await h({ url });
    return out instanceof Response ? out : Response.json(out);
  });
  return { calls, client: createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch }) };
}

const wrap = (client: ContentKitClient) =>
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ContentKitProvider client={client} viewer="fan">
        {children}
      </ContentKitProvider>
    );
  };

const key = "t:group:g1";
const group = (access: GroupAccess) => ({ id: "g1", kind: "channel", name: "No Offer Club", access });
const free = { key, target: { kind: "group" as const, id: "g1" }, terms: { amount: "0.00", currency: "USD" } };

it("MembershipBox: no Join where the group sells no membership; members stay members", async () => {
  const s = server({});
  const { container, rerender } = render(<MembershipBox group={group({ key, member: false })} />, { wrapper: wrap(s.client) });
  expect(container.querySelector('[data-ckui="membership-box"]')).toBeNull();
  expect(screen.queryByRole("button", { name: "Join" })).toBeNull();

  rerender(<MembershipBox group={group({ key, member: true })} />);
  expect(screen.getByText("Member")).toBeInTheDocument();
});

it("MembershipBox: a free group's Join refused as not joinable says so, not that something changed elsewhere", async () => {
  const s = server({ "POST /groups/g1/join": () => Response.json({ error: "offers: not free to join", code: "not_joinable" }, { status: 409 }) });
  const user = userEvent.setup();
  render(<MembershipBox group={group({ key, member: false, offer: free })} />, { wrapper: wrap(s.client) });
  await user.click(screen.getByRole("button", { name: "Join" }));
  expect(await screen.findByText("This group isn't open to join right now.")).toBeInTheDocument();
  expect(screen.queryByText(/changed elsewhere/)).toBeNull();
});

const model: ContentModel = { languages: ["en"], tiers: [], content_filters: [], signals: false, kinds: {}, group_kinds: { channel: { owner: "user", sellable: true, auth_scope: true } } };
const at = "2026-10-13T00:00:00Z";
const offer: Offer = { id: "o1", target: { kind: "group", id: "g1" }, key, terms: { amount: "4.99", currency: "USD", renew_hours: 720 }, state: "active", synced: true, revision: 2, updated_at: at };
const detail: GroupDetail = {
  id: "g1",
  kind: "channel",
  slug: "studio",
  state: "active",
  attributes: {},
  owner_user_id: "fan",
  created_at: at,
  updated_at: at,
  revision: 1,
  name: "Studio",
  names: [{ language: "en", kind: "name", name: "Studio" }],
  text: [],
  children: [],
  edges: [],
  counts: [],
  access: { key, member: false, editor: true },
  offer,
};

it("GroupEditor: its editor sees the membership it sells, though no paywall offers it to them", async () => {
  const s = server({ "GET /kinds": () => model, "GET /groups/g1": () => detail });
  const { container } = render(<GroupEditor id="g1" kind="channel" />, { wrapper: wrap(s.client) });
  await waitFor(() => expect(container.querySelector('[data-ckui="group-offer"] [data-ckui="current-offer"]')).not.toBeNull());
  const section = container.querySelector<HTMLElement>('[data-ckui="group-offer"]')!;
  expect(within(section).getByText("On sale")).toBeInTheDocument();
  expect(within(section).getByText(/\$4\.99/)).toBeInTheDocument();
  expect(within(section).queryByText("Not for sale.")).toBeNull();
});

it("GroupAvatar and GroupCover are styling roots of their own outside a ContentKit surface", () => {
  const g = { id: "g1", kind: "channel", name: "Studio", slug: "studio" };
  const { container } = render(
    <div>
      <GroupAvatar group={g} className="size-10" />
      <GroupCover group={g} />
    </div>,
  );
  for (const slot of ["group-avatar", "group-cover"]) {
    const el = container.querySelector(`[data-ckui="${slot}"]`)!;
    expect(el).toHaveClass("ckui");
    expect(el).toHaveAttribute("data-ckui-theme");
  }
  expect(container.querySelector('[data-ckui="group-avatar"]')).toHaveClass("rounded-full", "size-10");
});
