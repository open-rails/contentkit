import { useCallback, useState } from "react";
import type { ContentKitClient } from "../client/client.js";
import { toContentKitError, type ContentKitError } from "../client/errors.js";
import type { Poll, PollInput, PollOption, PollUpdate } from "../client/generated/wire.js";
import { keyOf, offsetPages, useContentScope, useList, useResource, type UseList } from "./use-resource.js";

/** The poll after the caller votes for option; a vote is final. */
export function withVote(p: Poll, option: string): Poll {
  if (p.voted) return p;
  return { ...p, voted: true, my_option: option, total_votes: p.total_votes + 1, options: p.options.map((o) => (o.id === option ? { ...o, vote_count: o.vote_count + 1 } : o)) };
}

export interface UsePoll {
  /** null: no such poll (or no live one). */
  poll: Poll | null;
  loading: boolean;
  error?: ContentKitError;
  reload: () => void;
  /** A vote or answer is in flight. */
  pending: boolean;
  /** Votes at once (a vote is final); rolled back if the server refuses it. */
  vote: (option: string) => Promise<void>;
  /** Stores or replaces the signed-in caller's free-text answer. */
  answer: (text: string) => Promise<void>;
}

/**
 * A poll with the caller's vote or answer. Without an id it is the newest
 * live poll (in language): the one a home page shows.
 */
export function usePoll(id?: string | null, o: { initial?: Poll; language?: string; client?: ContentKitClient } = {}): UsePoll {
  const { client, store, scope } = useContentScope(o.client);
  const key = id ? keyOf("poll", scope, id) : keyOf("poll", scope, "latest", o.language ?? "");
  const r = useResource<Poll | null>(
    store,
    key,
    { type: "poll", id: id ?? undefined },
    async (s) => (id ? client.polls.get(id, s) : ((await client.polls.list({ language: o.language, limit: 1 }, s))[0] ?? null)),
    { initial: o.initial },
  );
  const [pending, setPending] = useState(false);
  const poll = r.data ?? null;
  const vote = useCallback(
    async (option: string) => {
      const prev = store.snapshot<Poll | null>(key).data ?? poll;
      if (!prev) return;
      const token = store.mark(key);
      store.set(key, withVote(prev, option));
      setPending(true);
      try {
        await client.polls.vote(prev.id, option);
      } catch (e) {
        store.rollback(key, token, prev);
        throw toContentKitError(e);
      } finally {
        setPending(false);
      }
    },
    [client, store, key, poll],
  );
  const answer = useCallback(
    async (text: string) => {
      if (!poll) return;
      setPending(true);
      try {
        await client.polls.answer(poll.id, text);
      } finally {
        setPending(false);
      }
    },
    [client, poll],
  );
  return { poll, loading: r.loading || (!r.loaded && o.initial === undefined), error: r.error, reload: r.reload, pending, vote, answer };
}

export interface PollFilter {
  language?: string;
  /** YYYY-MM */
  month?: string;
  /** YYYY-MM-DD */
  date?: string;
  /** Staff: every poll, scheduled and inactive ones included (PollWrite). */
  admin?: boolean;
  pageSize?: number;
}

/** Polls, newest first, a page at a time. */
export function usePolls(f: PollFilter = {}, o: { client?: ContentKitClient } = {}): UseList<Poll> {
  const { client, store, scope } = useContentScope(o.client);
  const size = f.pageSize ?? 20;
  const q = { language: f.language, month: f.month, date: f.date };
  return useList(
    store,
    keyOf("polls", scope, !!f.admin, q.language, q.month, q.date, size),
    { type: "polls" },
    offsetPages(size, (page, signal) => (f.admin ? client.polls.adminList({ ...q, ...page }, signal) : client.polls.list({ ...q, ...page }, signal))),
  );
}

/** Images for a new poll, uploaded once it exists: the question's, and each option's by index. */
export interface PollImages {
  question?: Blob | null;
  options?: readonly (Blob | null | undefined)[];
}

export interface UsePollEditor {
  /** null before create (or while loading). */
  poll: Poll | null;
  loading: boolean;
  error?: ContentKitError;
  /** Writes in flight. */
  saving: boolean;
  /** Creates the poll, then uploads its images; the editor then edits it. Image failures are returned, not thrown. */
  create: (input: PollInput, images?: PollImages) => Promise<{ poll: Poll; imageErrors: ContentKitError[] }>;
  update: (patch: PollUpdate) => Promise<Poll>;
  remove: () => Promise<void>;
  addOption: (label: string, image?: Blob | null) => Promise<PollOption>;
  renameOption: (option: string, label: string) => Promise<void>;
  /** Removes an option and its votes; a poll keeps at least two. */
  removeOption: (option: string) => Promise<void>;
  /** Moves an option to index to, at once; refetched if a write fails. */
  moveOption: (option: string, to: number) => Promise<void>;
  setImage: (file: Blob | null) => Promise<void>;
  setOptionImage: (option: string, file: Blob | null) => Promise<void>;
}

/** Staff: one poll's editor (PollWrite); null creates a new poll. */
export function usePollEditor(id: string | null | undefined, o: { client?: ContentKitClient } = {}): UsePollEditor {
  const { client, store, scope } = useContentScope(o.client);
  const [created, setCreated] = useState<string | null>(null);
  const pid = id ?? created;
  const key = pid ? keyOf("poll", scope, pid) : null;
  const r = useResource<Poll | null>(store, key, { type: "poll", id: pid ?? undefined }, (s) => client.polls.get(pid!, s));
  const poll = r.data ?? null;
  const [inflight, setInflight] = useState(0);
  const run = useCallback(async <T>(fn: () => Promise<T>): Promise<T> => {
    setInflight((n) => n + 1);
    try {
      return await fn();
    } finally {
      setInflight((n) => n - 1);
    }
  }, []);
  const need = useCallback(() => {
    if (!pid) throw new Error("contentkit-ui: create the poll first");
    return pid;
  }, [pid]);

  const create = useCallback<UsePollEditor["create"]>(
    (input, images = {}) =>
      run(async () => {
        const made = await client.polls.create(input);
        const imageErrors: ContentKitError[] = [];
        const tries: Promise<unknown>[] = [];
        if (images.question) tries.push(client.polls.uploadImage(made.id, images.question));
        const byPosition = [...made.options].sort((a, b) => a.position - b.position);
        (input.options ?? []).forEach((opt, i) => {
          const file = images.options?.[i];
          const target = byPosition.find((x) => x.position === opt.position) ?? byPosition[i];
          if (file && target) tries.push(client.polls.uploadOptionImage(made.id, target.id, file));
        });
        for (const res of await Promise.allSettled(tries)) if (res.status === "rejected") imageErrors.push(toContentKitError(res.reason));
        setCreated(made.id);
        return { poll: tries.length ? await client.polls.get(made.id) : made, imageErrors };
      }),
    [client, run],
  );
  const update = useCallback((patch: PollUpdate) => run(() => client.polls.update(need(), patch)), [client, run, need]);
  const remove = useCallback(() => run(() => client.polls.delete(need())), [client, run, need]);
  const addOption = useCallback(
    (label: string, image?: Blob | null) =>
      run(async () => {
        const opt = await client.polls.addOption(need(), { label });
        if (image) opt.image_url = (await client.polls.uploadOptionImage(need(), opt.id, image)) ?? undefined;
        return opt;
      }),
    [client, run, need],
  );
  const renameOption = useCallback((option: string, label: string) => run(async () => void (await client.polls.updateOption(need(), option, { label }))), [client, run, need]);
  const removeOption = useCallback((option: string) => run(() => client.polls.deleteOption(need(), option)), [client, run, need]);
  const moveOption = useCallback(
    (option: string, to: number) =>
      run(async () => {
        const current = store.snapshot<Poll | null>(key!).data;
        if (!current) return;
        const order = [...current.options].sort((a, b) => a.position - b.position);
        const from = order.findIndex((x) => x.id === option);
        if (from < 0 || to < 0 || to >= order.length || from === to) return;
        order.splice(to, 0, ...order.splice(from, 1));
        store.set<Poll | null>(key!, { ...current, options: order.map((x, position) => ({ ...x, position })) });
        try {
          await client.polls.reorderOptions(current.id, order);
        } catch (e) {
          store.load(key!, true);
          throw e;
        }
      }),
    [client, store, key, run],
  );
  const setImage = useCallback((file: Blob | null) => run(async () => void (await client.polls.uploadImage(need(), file))), [client, run, need]);
  const setOptionImage = useCallback(
    (option: string, file: Blob | null) => run(async () => void (await client.polls.uploadOptionImage(need(), option, file))),
    [client, run, need],
  );
  return {
    poll,
    loading: !!key && (r.loading || !r.loaded),
    error: r.error,
    saving: inflight > 0,
    create,
    update,
    remove,
    addOption,
    renameOption,
    removeOption,
    moveOption,
    setImage,
    setOptionImage,
  };
}
