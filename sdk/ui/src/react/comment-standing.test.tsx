// @vitest-environment jsdom
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it } from "vitest";
import { createContentKitClient, type ContentKitClient } from "../client/client.js";
import type { Comment, Content } from "../client/generated/wire.js";
import type { ContentKitChange } from "../client/http.js";
import { ContentKitProvider } from "./provider.js";
import { useCanComment, useComments } from "./comments.js";
import { ResourceStore } from "./resources.js";

afterEach(cleanup);
const target = { type: "contents" as const, id: "c1", version: "edition-a" };
const comment = (reply?: string): Comment => ({
  id: "new", body: "new", language: "en", reply_to_id: reply, deleted: false,
  likes: 0, dislikes: 0, mine: 0, reply_count: 0, created_at: "2026-10-16T00:00:00Z", updated_at: "2026-10-16T00:00:00Z",
});

it("keeps standing keys/counts by version and thread language, updating only the posted thread", async () => {
  const counts: Record<string, number> = { "edition-a/en": 2, "edition-a/ja": 5, "edition-b/en": 8 };
  const reads: string[] = [];
  const client = createContentKitClient({ baseUrl: "https://ck.test", fetch: async (input, init) => {
    const url = new URL(String(input));
    const key = `${url.searchParams.get("version")}/${url.searchParams.get("language")}`;
    if (url.pathname.endsWith("/can-comment")) {
      reads.push(key);
      return Response.json({ can_comment: true, comments: "open", locked: false, count: counts[key], anonymous: false, max_length: 2000, moderate: false, ban_scopes: [] });
    }
    if (init?.method === "POST") {
      const body = JSON.parse(String(init.body));
      if (!body.reply_to_id) {
        const thread = `${url.searchParams.get("version")}/${body.language}`;
        counts[thread] = (counts[thread] ?? 0) + 1;
      }
      return Response.json(comment(body.reply_to_id), { status: 201 });
    }
    return Response.json([]);
  } });
  const wrapper = ({ children }: { children: ReactNode }) => <ContentKitProvider client={client} viewer="alice">{children}</ContentKitProvider>;
  const { result, rerender } = renderHook(() => ({
    en: useCanComment(target, { language: "en" }), ja: useCanComment(target, { language: "ja" }),
    other: useCanComment({ ...target, version: "edition-b" }, { language: "en" }),
    enList: useComments(target, { language: "en" }), jaList: useComments(target, { language: "ja" }),
  }), { wrapper });
  await waitFor(() => expect([result.current.en.standing?.count, result.current.ja.standing?.count, result.current.other.standing?.count]).toEqual([2, 5, 8]));
  rerender();
  await act(async () => { await Promise.resolve(); });
  expect(reads).toHaveLength(3);
  await act(async () => { await result.current.enList.post("new"); });
  await waitFor(() => expect(result.current.en.standing?.count).toBe(3));
  expect(result.current.enList.items).toHaveLength(1);
  expect(result.current.jaList.items).toHaveLength(0);
  expect(result.current.ja.standing?.count).toBe(5);
  expect(result.current.other.standing?.count).toBe(8);
  expect(reads).toEqual(["edition-a/en", "edition-a/ja", "edition-b/en", "edition-a/en"]);
  await act(async () => { await result.current.enList.post("reply", { replyTo: "root" }); });
  expect(reads).toHaveLength(4); // replies do not change the top-level count
});

const changes: ContentKitChange[] = [
  { type: "comment.updated", comment: comment() },
  { type: "comment.deleted", id: "outside-loaded-page" },
  { type: "comment.restored", id: "outside-loaded-page" },
  { type: "moderation.resolved", kind: "comment", id: "outside-loaded-page", decision: "approve" },
  { type: "access.changed" },
  { type: "content.changed", id: target.id },
  { type: "content.saved", content: { id: target.id } as Content },
  { type: "content.deleted", id: target.id },
];

it.each(changes)("refreshes authoritative standing after $type without needing the comment in a loaded page", async (change) => {
  let emit: (change: ContentKitChange, scope: string | null) => void = () => {};
  const client = { subscribe: (listener: typeof emit) => { emit = listener; return () => {}; } } as ContentKitClient;
  const store = new ResourceStore(client);
  const unsubscribe = store.subscribe("standing", { type: "standing", target, language: "en" }, () => {});
  let count = 23;
  let calls = 0;
  const unbind = store.bind("standing", async () => { calls++; return { count }; });
  store.load("standing");
  await waitFor(() => expect(store.snapshot<{ count: number }>("standing").data?.count).toBe(23));
  count = 24;
  emit(change, "{}");
  await waitFor(() => expect(store.snapshot<{ count: number }>("standing").data?.count).toBe(24));
  expect(calls).toBe(2);
  unbind();
  unsubscribe();
});
