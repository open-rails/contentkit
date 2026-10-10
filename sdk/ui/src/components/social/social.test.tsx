// @vitest-environment jsdom
import "../../test/dom.js";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../../client/index.js";
import type { AdminComment, Comment, CommentStanding, Config, HeldItem, Poll as PollData, PollOption } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/index.js";
import { CommentModeration, Comments, FavoriteButton, Poll, PollEditor, ReactionButtons } from "../../index.js";

type Handler = (req: { url: URL; body: unknown }) => unknown;

/** A fetch answering "METHOD /path" from routes (JSON, or a Response); calls lists what was asked. */
function server(routes: Record<string, Handler>) {
  const calls: string[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://x");
    const key = `${init?.method ?? "GET"} ${url.pathname}`;
    calls.push(key + url.search);
    const h = routes[key];
    if (!h) return json(404, { error: "not found", code: "not_found" });
    const out = await h({ url, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    return out instanceof Response ? out : out === undefined ? new Response(null, { status: 204 }) : json(200, out);
  });
  return { calls, client: createContentKitClient({ baseUrl: "", fetch: fetch as typeof globalThis.fetch }) };
}
const json = (status: number, body: unknown, headers?: Record<string, string>) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
const refusal = (status: number, code: string, extra: object = {}) => json(status, { error: code.replace("_", " "), code, ...extra });

function wrap(client: ContentKitClient, o: { viewer?: string | null; onSignIn?: () => void } = {}) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ContentKitProvider client={client} viewer={o.viewer} onSignIn={o.onSignIn}>
        {children}
      </ContentKitProvider>
    );
  };
}

const item = { kind: "video", id: "v1" };
const at = new Date(Date.now() - 3 * 60_000).toISOString();
const comment = (id: string, o: Partial<Comment> = {}): Comment => ({
  id,
  body: `body ${id}`,
  deleted: false,
  likes: 0,
  dislikes: 0,
  mine: 0,
  reply_count: 0,
  user_id: "bob",
  author: { id: "bob", username: "bob" },
  created_at: at,
  updated_at: at,
  ...o,
});
const standing = (o: Partial<CommentStanding> = {}): CommentStanding => ({ can_comment: true, anonymous: false, max_length: 400, user_id: "alice", moderate: false, ban_scopes: [], ...o });
const config = (anonymous: Partial<Config["anonymous"]> = {}) => () => ({ anonymous: { comments: false, reactions: false, votes: false, ...anonymous } });

it("Comments: a thread with tombstones and the author's held comment; posting lands in place; Ctrl+Enter posts", async () => {
  const posted = comment("c9", { body: "hello there", user_id: "alice", author: { id: "alice", username: "alice" } });
  const s = server({
    "GET /video/v1/comments": () => [comment("c1"), comment("c2", { deleted: true, body: "[deleted]", author: undefined, user_id: undefined }), comment("c3", { user_id: "alice", author: { id: "alice", username: "alice" }, moderation: "held", moderation_reason: "needs a look" })],
    "GET /video/v1/can-comment": () => standing(),
    "POST /video/v1/comments": ({ body }) => (expect(body).toEqual({ body: "hello there" }), json(201, posted)),
  });
  const user = userEvent.setup();
  render(<Comments item={item} count={3} />, { wrapper: wrap(s.client) });
  expect(await screen.findByText("body c1")).toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "3 comments" })).toBeInTheDocument();
  expect(screen.getByText("This comment was deleted.")).toBeInTheDocument();
  expect(screen.getByText("Awaiting review")).toBeInTheDocument();
  expect(screen.getAllByText("3 minutes ago")).toHaveLength(3);

  const box = screen.getByRole("textbox", { name: "Add a comment…" });
  await user.click(screen.getByRole("button", { name: "Post" }));
  await user.type(box, "   ");
  await user.click(screen.getByRole("button", { name: "Post" }));
  expect(screen.getByText("Write something first.")).toBeInTheDocument();
  await user.clear(box);
  await user.type(box, "hello there");
  await user.keyboard("{Control>}{Enter}{/Control}");
  expect(await screen.findByText("hello there")).toBeInTheDocument();
  expect(box).toHaveValue("");
  expect(s.calls.filter((c) => c.startsWith("GET /video/v1/comments"))).toHaveLength(1);
});

it("Comments: a refused like rolls back; a ban or a rate limit is explained", async () => {
  let refuse = true;
  const s = server({
    "GET /video/v1/comments": () => [comment("c1", { likes: 2 })],
    "GET /video/v1/can-comment": () => standing(),
    "POST /comments/c1/like": () => (refuse ? refusal(404, "not_found") : { likes: 3, dislikes: 0, mine: 1 }),
    "POST /video/v1/comments": () => refusal(429, "rate_limited", { retry_after: 12, action: "comment" }),
  });
  const user = userEvent.setup();
  render(<Comments item={item} />, { wrapper: wrap(s.client) });
  const like = await screen.findByRole("button", { name: "Like" });
  await user.click(like);
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("This is no longer available."));
  expect(like).toHaveAttribute("aria-pressed", "false");
  expect(like).toHaveTextContent("2");
  refuse = false;
  await user.click(like);
  await waitFor(() => expect(like).toHaveAttribute("aria-pressed", "true"));
  expect(like).toHaveTextContent("3");

  await user.type(screen.getByRole("textbox", { name: "Add a comment…" }), "spam");
  await user.click(screen.getByRole("button", { name: "Post" }));
  expect(await screen.findByText("You're commenting too quickly. Try again in 12 s.")).toBeInTheDocument();
});

it("Comments: the longest comment is the server's: the field takes no more, and a refusal says the limit", async () => {
  const s = server({
    "GET /video/v1/comments": () => [],
    "GET /video/v1/can-comment": () => standing({ max_length: 20 }),
    "POST /video/v1/comments": () => refusal(422, "comment_too_long", { details: { max: 20 } }),
  });
  const user = userEvent.setup();
  render(<Comments item={item} />, { wrapper: wrap(s.client) });
  const box = await screen.findByRole("textbox", { name: "Add a comment…" });
  await waitFor(() => expect(box).toHaveAttribute("maxlength", "20"));
  await user.type(box, "seventeen letters");
  expect(screen.getByText("3 characters left")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Post" }));
  expect(await screen.findByText("A comment is at most 20 characters.")).toBeInTheDocument();
});

it("Comments: a banned caller sees why instead of the composer", async () => {
  const s = server({
    "GET /video/v1/comments": () => [],
    "GET /video/v1/can-comment": () => standing({ can_comment: false, ban: { scope: "owner:creator", reason: "spam", until: "2030-01-02T03:04:00Z" } }),
  });
  render(<Comments item={item} />, { wrapper: wrap(s.client) });
  const notice = await screen.findByText("You can't comment on this creator's content.");
  expect(notice.closest("[data-ckui=ban-notice]")).toHaveTextContent("Reason: spam");
  expect(screen.queryByRole("textbox")).toBeNull();
  expect(screen.getByText("No comments yet. Be the first to comment!")).toBeInTheDocument();
});

it("Comments: signed out, the server decides: a sign-in where it takes no anonymous comments or reactions, a name where it does", async () => {
  const signIn = vi.fn();
  const members = {
    "GET /video/v1/comments": () => [comment("c1")],
    "GET /video/v1/can-comment": () => standing({ user_id: undefined, can_comment: false }),
    "GET /config": config(),
  };
  const user = userEvent.setup();
  let view = render(<Comments item={item} />, { wrapper: wrap(server(members).client, { viewer: null, onSignIn: signIn }) });
  await user.click(await screen.findByRole("button", { name: "Sign in to comment" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Like" })).toBeEnabled());
  await user.click(screen.getByRole("button", { name: "Like" }));
  expect(signIn).toHaveBeenCalledTimes(2);
  expect(screen.queryByRole("button", { name: "Reply" })).toBeInTheDocument();
  view.unmount();

  // No sign-in to offer: said, not offered; reactions and replies are off.
  view = render(<Comments item={item} />, { wrapper: wrap(server(members).client, { viewer: null }) });
  expect(await screen.findByText("Sign in to comment")).not.toHaveRole("button");
  await waitFor(() => expect(screen.getByRole("button", { name: "Like" })).toBeDisabled());
  expect(screen.queryByRole("button", { name: "Reply" })).toBeNull();
  expect(screen.queryByRole("textbox")).toBeNull();
  view.unmount();

  // Anonymous comments and reactions on: a name is asked although the host offers a sign-in.
  const s = server({
    "GET /video/v1/comments": () => [comment("c1")],
    "GET /video/v1/can-comment": () => standing({ user_id: undefined, anonymous: true }),
    "GET /config": config({ comments: true, reactions: true }),
    "POST /video/v1/comments": ({ body }) => json(201, comment("c2", { anon_name: (body as { anon_name: string }).anon_name, user_id: undefined, author: undefined, body: "hi" })),
    "POST /comments/c1/like": () => ({ likes: 1, dislikes: 0, mine: 1 }),
  });
  signIn.mockClear();
  render(<Comments item={item} />, { wrapper: wrap(s.client, { viewer: null, onSignIn: signIn }) });
  await user.type(await screen.findByRole("textbox", { name: "Add a comment…" }), "hi");
  await user.click(screen.getByRole("button", { name: "Post" }));
  expect(screen.getByText("Enter a name.")).toBeInTheDocument();
  await user.type(screen.getByLabelText("Name"), "Guest");
  await user.click(screen.getByRole("button", { name: "Post" }));
  expect(await screen.findByText("Guest")).toBeInTheDocument();
  await waitFor(() => expect(s.calls).toContain("GET /config"));
  const like = within(screen.getByText("body c1").closest("article")!).getByRole("button", { name: "Like" });
  await user.click(like);
  await waitFor(() => expect(like).toHaveAttribute("aria-pressed", "true"));
  expect(signIn).not.toHaveBeenCalled();
});

it("Comments: replies open under their comment; authors edit, moderators delete others' comments after confirming", async () => {
  const s = server({
    "GET /video/v1/comments": () => [comment("c1", { reply_count: 1 }), comment("c2", { user_id: "alice", author: { id: "alice", username: "alice" } })],
    "GET /video/v1/can-comment": () => standing({ moderate: true }),
    "GET /comments/c1/replies": () => [comment("r1", { reply_to_id: "c1", body: "a reply" })],
    "PATCH /comments/c2": ({ body }) => comment("c2", { body: (body as { body: string }).body, user_id: "alice", author: undefined }),
    "DELETE /comments/c1": () => undefined,
  });
  const user = userEvent.setup();
  render(<Comments item={item} />, { wrapper: wrap(s.client) });
  await user.click(await screen.findByRole("button", { name: "Show 1 reply" }));
  expect(await screen.findByText("a reply")).toBeInTheDocument();

  const mine = screen.getByText("body c2").closest("article")!;
  await user.click(within(mine).getByRole("button", { name: "Comment actions" }));
  await user.click(await screen.findByRole("menuitem", { name: "Edit" }));
  const edit = within(mine).getByRole("textbox");
  await user.clear(edit);
  await user.type(edit, "edited text");
  await user.click(within(mine).getByRole("button", { name: "Save" }));
  expect(await within(mine).findByText("edited text")).toBeInTheDocument();
  expect(within(mine).getByText("alice")).toBeInTheDocument();

  const theirs = screen.getByText("body c1").closest("article")!;
  await user.click(within(theirs).getByRole("button", { name: "Comment actions" }));
  await user.click(await screen.findByRole("menuitem", { name: "Delete" }));
  const dialog = await screen.findByRole("alertdialog");
  await user.click(within(dialog).getByRole("button", { name: "Delete" }));
  expect(await screen.findByText("This comment was deleted.")).toBeInTheDocument();
});

it("ReactionButtons and FavoriteButton change at once and roll back when refused; signed out they ask to sign in", async () => {
  let fail = false;
  const s = server({
    "GET /video/v1/reaction": () => ({ likes: 4, dislikes: 1, mine: 0 }),
    "POST /video/v1/like": () => (fail ? refusal(429, "rate_limited", { retry_after: 5 }) : { likes: 5, dislikes: 1, mine: 1 }),
    "POST /video/v1/neutral": () => ({ likes: 4, dislikes: 1, mine: 0 }),
    "GET /video/v1/favorite": () => ({ favorited: false, count: 10 }),
    "POST /video/v1/favorite": () => ({ favorited: true, count: 11 }),
  });
  const user = userEvent.setup();
  const onChange = vi.fn();
  render(
    <>
      <ReactionButtons item={item} />
      <FavoriteButton item={item} onChange={onChange} />
    </>,
    { wrapper: wrap(s.client, { viewer: "alice" }) },
  );
  const like = screen.getByRole("button", { name: "Like" });
  await waitFor(() => expect(like).toHaveTextContent("4"));
  await user.click(like);
  await waitFor(() => expect(like).toHaveAttribute("aria-pressed", "true"));
  expect(like).toHaveTextContent("5");
  await user.click(like);
  await waitFor(() => expect(like).toHaveTextContent("4"));
  fail = true;
  await user.click(like);
  expect(await screen.findByText("Too many requests. Try again in 5 s.")).toBeInTheDocument();
  expect(like).toHaveAttribute("aria-pressed", "false");

  const fav = screen.getByRole("button", { name: /Add to favorites/ });
  await waitFor(() => expect(fav).toHaveTextContent("10"));
  await user.click(fav);
  await waitFor(() => expect(onChange).toHaveBeenCalledWith(true));
  expect(screen.getByRole("button", { name: /Remove from favorites/ })).toHaveTextContent("11");

  const signIn = vi.fn();
  const out = server({ "GET /video/v1/favorite": () => ({ favorited: false, count: 7 }) });
  render(<FavoriteButton item={item} />, { wrapper: wrap(out.client, { viewer: null, onSignIn: signIn }) });
  // Signed out: the server's count shows; a click asks to sign in and writes nothing.
  const signedOut = screen.getByRole("button", { name: /Sign in to save favorites/ });
  await waitFor(() => expect(signedOut).toHaveTextContent("7"));
  await user.click(signedOut);
  expect(signIn).toHaveBeenCalled();
  expect(out.calls.filter((c) => !c.startsWith("GET"))).toEqual([]);
});

it("ReactionButtons signed out: ask to sign in where the server takes no anonymous reactions, react where it does", async () => {
  const user = userEvent.setup();
  const signIn = vi.fn();
  const routes = { "GET /video/v1/reaction": () => ({ likes: 2, dislikes: 0, mine: 0 }), "POST /video/v1/like": () => ({ likes: 3, dislikes: 0, mine: 1 }) };
  const off = server({ ...routes, "GET /config": config() });
  let view = render(<ReactionButtons item={item} />, { wrapper: wrap(off.client, { viewer: null, onSignIn: signIn }) });
  await waitFor(() => expect(off.calls).toContain("GET /config"));
  await user.click(screen.getByRole("button", { name: "Like" }));
  expect(signIn).toHaveBeenCalledTimes(1);
  expect(off.calls).not.toContain("POST /video/v1/like");
  view.unmount();

  view = render(<ReactionButtons item={item} />, { wrapper: wrap(server({ ...routes, "GET /config": config() }).client, { viewer: null }) });
  await waitFor(() => expect(screen.getByRole("button", { name: "Like" })).toBeDisabled());
  view.unmount();

  const on = server({ ...routes, "GET /config": config({ reactions: true }) });
  render(<ReactionButtons item={item} />, { wrapper: wrap(on.client, { viewer: null, onSignIn: signIn }) });
  await waitFor(() => expect(on.calls).toContain("GET /config"));
  const like = screen.getByRole("button", { name: "Like" });
  await user.click(like);
  await waitFor(() => expect(like).toHaveAttribute("aria-pressed", "true"));
  expect(signIn).toHaveBeenCalledTimes(1);
});

const option = (id: string, label: string, position: number, votes = 0): PollOption => ({ id, label, position, vote_count: votes });
const poll = (o: Partial<PollData> = {}): PollData => ({
  id: "p1",
  kind: "multiple_choice",
  question: "Tea or coffee?",
  language: "en",
  is_active: true,
  live_at: at,
  closed: false,
  total_votes: 4,
  voted: false,
  options: [option("o1", "Tea", 0, 3), option("o2", "Coffee", 1, 1)],
  ...o,
});

it("Poll: a ballot until the vote, then results scaled to the leader with the caller's choice marked", async () => {
  const s = server({
    "GET /polls": ({ url }) => (expect(url.searchParams.get("language")).toBe("en"), [poll()]),
    "POST /polls/p1/vote": ({ body }) => (expect(body).toEqual({ option_id: "o2" }), poll({ voted: true, my_option: "o2", total_votes: 5, options: [option("o1", "Tea", 0, 3), option("o2", "Coffee", 1, 2)] })),
  });
  const user = userEvent.setup();
  render(<Poll language="en" />, { wrapper: wrap(s.client, { viewer: "alice" }) });
  expect(await screen.findByRole("heading", { name: "Tea or coffee?" })).toBeInTheDocument();
  expect(screen.queryByText("75%")).toBeNull();
  await user.click(screen.getByRole("button", { name: /Coffee/ }));
  // At once: the vote counts before the server answers.
  const coffee = await screen.findByRole("meter", { name: "Coffee" });
  await waitFor(() => expect(coffee).toHaveAttribute("aria-valuenow", "40"));
  expect(within(coffee).getByLabelText("Your vote")).toBeInTheDocument();
  expect(screen.getByText("5 votes")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /Tea/ })).toBeNull();
});

it("Poll signed out: votes where the server takes anonymous votes, asks to sign in elsewhere", async () => {
  const user = userEvent.setup();
  const signIn = vi.fn();
  const voted = poll({ voted: true, my_option: "o1", total_votes: 5, options: [option("o1", "Tea", 0, 4), option("o2", "Coffee", 1, 1)] });
  const off = server({ "GET /polls/p1": () => poll(), "GET /config": config(), "POST /polls/p1/vote": () => voted });
  let view = render(<Poll poll="p1" />, { wrapper: wrap(off.client, { viewer: null, onSignIn: signIn }) });
  expect(await screen.findByText("Sign in to vote")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: /Tea/ }));
  expect(signIn).toHaveBeenCalledTimes(1);
  expect(off.calls).not.toContain("POST /polls/p1/vote");
  view.unmount();

  const on = server({ "GET /polls/p1": () => poll(), "GET /config": config({ votes: true }), "POST /polls/p1/vote": () => voted });
  view = render(<Poll poll="p1" />, { wrapper: wrap(on.client, { viewer: null, onSignIn: signIn }) });
  await waitFor(() => expect(on.calls).toContain("GET /config"));
  await user.click(await screen.findByRole("button", { name: /Tea/ }));
  expect(await screen.findByRole("meter", { name: "Tea" })).toBeInTheDocument();
  expect(screen.queryByText("Sign in to vote")).toBeNull();
  expect(signIn).toHaveBeenCalledTimes(1);
  view.unmount();
});

it("Poll: a closed poll shows results; a free-text poll takes and edits an answer", async () => {
  const user = userEvent.setup();
  const closed = server({ "GET /polls/p1": () => poll({ closed: true }) });
  const { unmount } = render(<Poll poll="p1" />, { wrapper: wrap(closed.client) });
  expect(await screen.findByText("This poll is closed.")).toBeInTheDocument();
  expect(screen.getByRole("meter", { name: "Tea" })).toHaveAttribute("aria-valuenow", "75");
  unmount();

  const free = poll({ kind: "free_text", options: [], total_votes: 0 });
  const s = server({
    "GET /polls/p1": () => free,
    "POST /polls/p1/answer": ({ body }) => ({ ...free, my_answer: { id: "a1", text: (body as { text: string }).text, classified: true, updated_at: at }, answer_count: 3, groups: [{ id: "cats", label: "Cats", count: 2 }, { id: "dogs", label: "Dogs", count: 1 }] }),
  });
  render(<Poll poll="p1" />, { wrapper: wrap(s.client, { viewer: "alice" }) });
  await user.type(await screen.findByRole("textbox", { name: "Your answer" }), "Cats are great");
  await user.click(screen.getByRole("button", { name: "Send answer" }));
  expect(await screen.findByText("Cats are great")).toBeInTheDocument();
  expect(screen.getByText("Cats")).toBeInTheDocument();
  expect(screen.getByText("3 answers")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Edit answer" }));
  expect(screen.getByRole("textbox", { name: "Your answer" })).toHaveValue("Cats are great");
});

it("PollEditor: a new poll needs a question and two options; an existing one moves options", async () => {
  const created = poll({ total_votes: 0, options: [option("o1", "Tea", 0), option("o2", "Coffee", 1), option("o3", "Water", 2)] });
  let current = created;
  const s = server({
    "POST /polls": ({ body }) => {
      expect(body).toMatchObject({ kind: "multiple_choice", question: "Tea or coffee?", language: "en", options: [{ label: "Tea", position: 0 }, { label: "Coffee", position: 1 }, { label: "Water", position: 2 }] });
      return json(201, created);
    },
    "GET /polls/p1": () => current,
    "PATCH /polls/p1/options/o3": ({ body }) => ((current = { ...current, options: current.options.map((o) => (o.id === "o3" ? { ...o, ...(body as object) } : o)) }), current.options[2]),
    "PATCH /polls/p1/options/o1": ({ body }) => ((current = { ...current, options: current.options.map((o) => (o.id === "o1" ? { ...o, ...(body as object) } : o)) }), current.options[0]),
    "PATCH /polls/p1/options/o2": ({ body }) => ((current = { ...current, options: current.options.map((o) => (o.id === "o2" ? { ...o, ...(body as object) } : o)) }), current.options[1]),
  });
  const user = userEvent.setup();
  const onCreated = vi.fn();
  render(<PollEditor languages={["en", "ja"]} onCreated={onCreated} />, { wrapper: wrap(s.client, { viewer: "editor" }) });
  await user.click(screen.getByRole("button", { name: "Create poll" }));
  expect(screen.getByRole("alert")).toHaveTextContent("Write a question.");
  await user.type(screen.getByLabelText("Question"), "Tea or coffee?");
  await user.type(screen.getByRole("textbox", { name: "Option 1" }), "Tea");
  await user.click(screen.getByRole("button", { name: "Create poll" }));
  expect(screen.getByRole("alert")).toHaveTextContent("A poll needs at least two options.");
  await user.type(screen.getByRole("textbox", { name: "Option 2" }), "Coffee");
  await user.click(screen.getByRole("button", { name: "Add option" }));
  await user.type(screen.getByRole("textbox", { name: "Option 3" }), "Water");
  await user.click(screen.getByRole("button", { name: "Create poll" }));
  await waitFor(() => expect(onCreated).toHaveBeenCalledWith(created));
  expect(await screen.findByRole("heading", { name: "Edit poll" })).toBeInTheDocument();

  // Keyboard reorder through the drag handle: lift Water, move it up one, drop.
  const rect = vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
    // The drag overlay's row stands where the dragged row is.
    const li = this.closest("[data-overlay]") ? document.querySelector("li[data-dragging]") : this.closest("li");
    const i = li?.parentElement ? [...li.parentElement.children].indexOf(li) : 0;
    return { x: 0, y: i * 48, top: i * 48, left: 0, bottom: i * 48 + 44, right: 400, width: 400, height: 44, toJSON: () => ({}) } as DOMRect;
  });
  const tick = () => act(() => new Promise((r) => setTimeout(r, 20)));
  const handle = screen.getByRole("button", { name: "Reorder Water" });
  handle.focus();
  fireEvent.keyDown(handle, { key: " ", code: "Space" });
  await tick();
  fireEvent.keyDown(document.activeElement!, { key: "ArrowUp", code: "ArrowUp" });
  await tick();
  fireEvent.keyDown(document.activeElement!, { key: " ", code: "Space" });
  rect.mockRestore();
  await waitFor(() => expect(screen.getAllByRole("textbox", { name: /Option \d/ }).map((e) => (e as HTMLInputElement).value)).toEqual(["Tea", "Water", "Coffee"]));
  await waitFor(() => expect(s.calls.filter((c) => c.startsWith("PATCH"))).toEqual(["PATCH /polls/p1/options/o3", "PATCH /polls/p1/options/o2"]));
});

it("PollEditor: emptying the closing time clears it (closes_at null)", async () => {
  let current = poll({ closes_at: "2031-05-01T10:00:00Z" });
  const s = server({
    "GET /polls/p1": () => current,
    "PATCH /polls/p1": ({ body }) => (expect(body).toEqual({ closes_at: null }), (current = { ...current, closes_at: undefined })),
  });
  const user = userEvent.setup();
  render(<PollEditor poll="p1" />, { wrapper: wrap(s.client, { viewer: "editor" }) });
  const closes = await screen.findByLabelText("Closes (optional)");
  expect(closes).not.toHaveValue("");
  await user.clear(closes);
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(s.calls).toContain("PATCH /polls/p1"));
  expect(await screen.findByText("Saved.")).toBeInTheDocument();
});

it("CommentModeration: deletes and restores; the queue approves, and says when an item changed since it was listed", async () => {
  const admin: AdminComment = { ...comment("c1"), tenant_id: "t", content_kind: "video", content_id: "v1", deleted: false };
  let deleted = false;
  const held: HeldItem = { kind: "comment", revision: 2, id: "h1", ref: { tenant_id: "t", content_kind: "video", content_id: "v1" }, author_id: "erin", author: { id: "erin", username: "erin" }, body: "iffy text", reason: "needs a look", held_at: at, created_at: at };
  let stale = true;
  const s = server({
    "GET /comments/admin": () => [{ ...admin, deleted }],
    "DELETE /comments/c1": () => ((deleted = true), undefined),
    "POST /comments/c1/restore": () => ((deleted = false), { restored: true }),
    "GET /moderation/held": () => ({ items: [held] }),
    "POST /moderation/comment/h1/resolve": () => (stale ? ((stale = false), refusal(404, "not_found")) : { decision: "approve" }),
  });
  const user = userEvent.setup();
  render(<CommentModeration itemHref={(i) => `/watch/${i.id}`} />, { wrapper: wrap(s.client, { viewer: "moderator" }) });
  const row = (await screen.findByText("body c1")).closest("li")!;
  expect(within(row).getByRole("link", { name: "video/v1" })).toHaveAttribute("href", "/watch/v1");
  await user.click(within(row).getByRole("button", { name: "Delete" }));
  expect(await within(row).findByText("Deleted")).toBeInTheDocument();
  await user.click(within(row).getByRole("button", { name: "Restore" }));
  await waitFor(() => expect(within(row).getByText("Published")).toBeInTheDocument());

  await user.click(screen.getByRole("tab", { name: "Review queue" }));
  const h = (await screen.findByText("iffy text")).closest("li")!;
  expect(within(h).getByText("Held because: needs a look")).toBeInTheDocument();
  await user.click(within(h).getByRole("button", { name: "Approve" }));
  expect(await within(h).findByText("It was edited since it was listed; review it again.")).toBeInTheDocument();
  await act(async () => {
    await user.click(within(h).getByRole("button", { name: "Approve" }));
  });
  await waitFor(() => expect(screen.getByText("Nothing is awaiting review.")).toBeInTheDocument());
});
