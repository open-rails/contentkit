import assert from "node:assert/strict";
import { runMediaContracts } from "./media.mjs";
import {
  createContentKitClient,
  ContentKitError,
} from "../../../sdk/ui/dist/client.js";

const fixture = JSON.parse(process.env.CONTENTKIT_TYPESCRIPT_FIXTURE);
const group = process.argv[2];
const owner = createContentKitClient({
  baseUrl: fixture.base,
  token: () => fixture.owner,
});
const reader = createContentKitClient({
  baseUrl: fixture.base,
  token: () => fixture.reader,
});
const anonymous = createContentKitClient({ baseUrl: fixture.base });
const bindings = [];
function proved(...names) {
  for (const name of names) {
    assert(!bindings.includes(name), `duplicate semantic receipt ${name}`);
    bindings.push(name);
  }
}
async function refused(action, status, code) {
  await assert.rejects(
    action,
    (error) =>
      error instanceof ContentKitError &&
      error.status === status &&
      error.code === code,
  );
}

async function released(id) {
  const deadline = Date.now() + 60_000;
  do {
    const item = await owner.contents.get(id);
    if (item.live) return item;
    await new Promise((resolve) => setTimeout(resolve, 550));
  } while (Date.now() < deadline);
  assert.fail(`durable publication did not complete for ${id}`);
}

if (group === "catalog_publication") {
  const config = await anonymous.config();
  assert.equal(config.comment_max_length, 2200);
  assert.deepEqual(config.anonymous, {
    comments: false,
    reactions: false,
    votes: false,
  });
  proved("ContentKitClient.config");
  const kinds = await anonymous.kinds();
  assert.deepEqual(Object.keys(kinds.kinds).sort(), ["article", "gallery"]);
  assert.deepEqual(kinds.languages, ["en", "de"]);
  proved("ContentKitClient.kinds");
  assert.equal(owner.url("contents"), fixture.base + "/v1");
  assert.equal(owner.url("codes"), fixture.base + "/v1/codes");
  proved("ContentKitClient.url");
  const before = await owner.catalog.listEvents({
    types: ["content.published"],
    limit: 1,
  });
  assert.equal(typeof before.next_cursor, "string");
  assert(before.next_cursor.length > 0);
  const draft = await owner.contents.create({
    kind: "article",
    text: {
      en: {
        title: "TS Unicode Straße / ?",
        body: "Exact TypeScript source body",
      },
    },
  });
  await refused(() => anonymous.contents.get(draft.id), 404, "not_found");
  await refused(() => reader.contents.publish(draft.id), 403, "forbidden");
  assert.equal((await owner.contents.get(draft.id)).state, "draft");
  const published = await owner.contents.publish(draft.id);
  assert.equal(published.id, draft.id);
  assert.equal((await released(draft.id)).live, true);
  assert.equal(
    (await anonymous.contents.get(draft.id)).body,
    "Exact TypeScript source body",
  );
  proved("ContentsClient.publish");
  const second = await owner.contents.create({
    kind: "article",
    text: {
      en: { title: "Second TypeScript article", body: "Ready without media" },
    },
  });
  const ready = await owner.contents.publishWhenReady(second.id, undefined, 60_000);
  assert.equal(ready.id, second.id);
  assert.equal(ready.live, true);
  await refused(
    () => reader.contents.publishWhenReady(second.id, undefined, 0),
    403,
    "forbidden",
  );
  assert.equal(
    (await anonymous.contents.get(second.id)).body,
    "Ready without media",
  );
  proved("ContentsClient.publishWhenReady");
  const facts = [];
  let cursor = before.next_cursor;
  for (let i = 0; i < 4; i++) {
    const page = await owner.catalog.listEvents({
      after: cursor,
      types: ["content.published"],
      limit: 1,
    });
    assert(page.events.length <= 1);
    for (const fact of page.events) {
      assert.equal(fact.type, "content.published");
      assert.equal(fact.scope, fixture.app);
      assert.match(fact.id, /^\d+$/);
      facts.push(fact);
    }
    cursor = page.next_cursor;
    if (!page.has_more) break;
  }
  assert.deepEqual(
    facts.map((x) => x.content_id),
    [draft.id, second.id],
  );
  assert.equal(new Set(facts.map((x) => x.id)).size, 2);
  assert.equal(
    (
      await owner.catalog.listEvents({
        after: cursor,
        types: ["content.published"],
      })
    ).events.length,
    0,
  );
  await refused(() => anonymous.catalog.listEvents(), 401, "unauthorized");
  proved("CatalogClient.listEvents");
  await refused(() => reader.contents.unpublish(draft.id), 403, "forbidden");
  const preservedPublication = await owner.contents.get(draft.id);
  assert.equal(preservedPublication.live, true);
  assert.equal(preservedPublication.body, "Exact TypeScript source body");
  const hidden = await owner.contents.unpublish(draft.id);
  assert.equal(hidden.id, draft.id);
  assert.equal(hidden.live, false);
  await refused(() => anonymous.contents.get(draft.id), 404, "not_found");
  await owner.contents.publish(draft.id);
  await released(draft.id);
  assert.equal(
    (await anonymous.contents.get(draft.id)).body,
    "Exact TypeScript source body",
  );
  proved("ContentsClient.unpublish");
  await refused(() => anonymous.contents.get(fixture.legacy), 404, "not_found");
  await refused(
    () => reader.contents.restore(fixture.legacy),
    403,
    "forbidden",
  );
  const preservedDeletion = await owner.contents.manage({
    ids: [fixture.legacy],
    state: "deleted",
  });
  assert.equal(preservedDeletion.length, 1);
  assert.equal(preservedDeletion[0].id, fixture.legacy);
  assert.equal(preservedDeletion[0].state, "deleted");
  const restored = await owner.contents.restore(fixture.legacy);
  assert.equal(restored.id, fixture.legacy);
  assert.equal((await released(fixture.legacy)).live, true);
  assert.equal(
    (await anonymous.contents.get(fixture.legacy)).body,
    "Retained historical body",
  );
  proved("ContentsClient.restore");
  const totals = await anonymous.catalog.totals({
    kinds: ["article"],
    languages: ["en"],
  });
  assert.deepEqual(Object.keys(totals.kinds), ["article"]);
  assert.equal(totals.kinds.article.public, 3);
  assert.equal(totals.kinds.article.languages.en.public, 3);
  proved("CatalogClient.totals");
  await refused(
    () => reader.contents.removeVersion(draft.id, draft.versions[0].id),
    403,
    "forbidden",
  );
  const preservedVersion = await owner.contents.version(
    draft.id,
    draft.versions[0].id,
  );
  assert.equal(preservedVersion.id, draft.versions[0].id);
  assert.equal(preservedVersion.state, "published");
  assert.equal((await owner.contents.get(draft.id)).live, true);
  await owner.contents.removeVersion(draft.id, draft.versions[0].id);
  await refused(() => anonymous.contents.get(draft.id), 404, "not_found");
  const retired = await owner.contents.manage({
    ids: [draft.id],
    state: "deleted",
  });
  assert.equal(retired.length, 1);
  assert.equal(retired[0].id, draft.id);
  assert.equal(retired[0].state, "deleted");
  await refused(() => owner.contents.restore(draft.id), 409, "conflict");
  assert.equal((await anonymous.contents.get(second.id)).id, second.id);
  proved("ContentsClient.removeVersion");
} else if (group === "moderation_bans") {
  const item = await owner.contents.create({
    kind: "article",
    text: {
      en: { title: "TypeScript owned thread", body: "Owned public body" },
    },
  });
  await owner.contents.publish(item.id);
  await released(item.id);
  const target = { type: "contents", id: item.id },
    other = { type: "contents", id: fixture.other };
  const prior = await reader.comments.create(target, {
    body: "Original retained comment",
  });
  assert.equal(prior.body, "Original retained comment");
  assert.equal(await owner.bans.get("global", fixture.readerID), null);
  const ownerBan = await owner.bans.ban("owner", fixture.readerID, {
    reason: "Owner-only control",
  });
  assert.equal(ownerBan.scope, "owner:" + fixture.ownerID);
  assert.equal(ownerBan.user_id, fixture.readerID);
  assert.equal(ownerBan.banned_by, fixture.ownerID);
  assert.deepEqual(
    (await owner.bans.list("owner")).map((x) => x.user_id),
    [fixture.readerID],
  );
  await refused(
    () => reader.comments.create(target, { body: "Must not be accepted" }),
    403,
    "comment_banned",
  );
  assert.equal((await anonymous.comments.list(target)).length, 1);
  const otherComment = await reader.comments.create(other, {
    body: "Other owner is unaffected",
  });
  assert.equal(otherComment.body, "Other owner is unaffected");
  await owner.bans.lift("owner", fixture.readerID);
  assert.deepEqual(await owner.bans.list("owner"), []);
  const afterLift = await reader.comments.create(target, {
    body: "Owner lift permits writing",
  });
  assert.equal(afterLift.body, "Owner lift permits writing");
  const global = await owner.bans.ban("global", fixture.readerID, {
    reason: "App-wide control",
  });
  assert.equal(global.scope, "global");
  assert.equal(
    (await owner.bans.get("global", fixture.readerID)).reason,
    "App-wide control",
  );
  assert.deepEqual(
    (await owner.bans.list("global")).map((x) => x.user_id),
    [fixture.readerID],
  );
  await refused(
    () => reader.bans.lift("global", fixture.readerID),
    403,
    "forbidden",
  );
  await refused(
    () => reader.bans.get("global", fixture.readerID),
    403,
    "forbidden",
  );
  assert.equal(
    (await owner.bans.get("global", fixture.readerID)).reason,
    "App-wide control",
  );
  await refused(
    () => reader.comments.create(other, { body: "App ban applies here too" }),
    403,
    "comment_banned",
  );
  await owner.bans.lift("global", fixture.readerID);
  await owner.bans.lift("global", fixture.readerID);
  assert.equal(await owner.bans.get("global", fixture.readerID), null);
  assert.deepEqual(await owner.bans.list("global"), []);
  proved(
    "BansClient.list",
    "BansClient.get",
    "BansClient.ban",
    "BansClient.lift",
  );
  const rules = await owner.moderation.banWords([
    { word: "ＦＯＲＢＩＤＤＥＮ", action: "reject" },
    { word: "Masked", action: "mask" },
  ]);
  assert.equal(rules.length, 2);
  assert.deepEqual(
    (await owner.moderation.bannedWords())
      .map((x) => [x.word, x.action])
      .sort(),
    [
      ["forbidden", "reject"],
      ["masked", "mask"],
    ],
  );
  const count = (await anonymous.comments.list(target)).length;
  await refused(
    () => reader.comments.create(target, { body: "forbidden" }),
    422,
    "moderation_rejected",
  );
  await refused(
    () => reader.moderation.unbanWords(["forbidden"]),
    403,
    "forbidden",
  );
  assert.equal((await anonymous.comments.list(target)).length, count);
  assert.equal(
    (await reader.comments.create(target, { body: "A MASKED word" })).body,
    "A ****** word",
  );
  await owner.moderation.unbanWords(["forbidden", "masked"]);
  assert.deepEqual(await owner.moderation.bannedWords(), []);
  assert.equal(
    (await reader.comments.create(target, { body: "forbidden" })).body,
    "forbidden",
  );
  proved(
    "ModerationClient.bannedWords",
    "ModerationClient.banWords",
    "ModerationClient.unbanWords",
  );
  const networks = await owner.moderation.banIPs([
    { network: "127.0.0.99/24", reason: "Local wire" },
  ]);
  assert.deepEqual(
    networks.map((x) => x.network),
    ["127.0.0.0/24"],
  );
  assert.deepEqual(
    (await owner.moderation.ipBans()).map((x) => x.network),
    ["127.0.0.0/24"],
  );
  await refused(
    () => reader.comments.create(target, { body: "Network must refuse" }),
    403,
    "comment_banned",
  );
  await refused(
    () => reader.moderation.liftIPBans(["127.0.0.0/24"]),
    403,
    "forbidden",
  );
  assert.equal((await owner.moderation.ipBans()).length, 1);
  await owner.moderation.liftIPBans(["127.0.0.45/24"]);
  assert.deepEqual(await owner.moderation.ipBans(), []);
  assert.equal(
    (
      await reader.comments.create(target, {
        body: "Network lift permits writing",
      })
    ).body,
    "Network lift permits writing",
  );
  proved(
    "ModerationClient.ipBans",
    "ModerationClient.banIPs",
    "ModerationClient.liftIPBans",
  );
  const holdReply = await fetch(
    fixture.base + "/v1/comments/" + prior.id + "/hold",
    { method: "POST", headers: { Authorization: "Bearer " + fixture.owner } },
  );
  assert.equal(holdReply.status, 204);
  assert(
    !(await anonymous.comments.list(target)).some((x) => x.id === prior.id),
  );
  const held = await owner.moderation.held({ kind: "comment", limit: 1 });
  assert.equal(held.items.length, 1);
  assert.equal(held.items[0].id, prior.id);
  assert.equal(held.items[0].body, "Original retained comment");
  assert(held.items[0].revision > 0);
  await refused(
    () => reader.moderation.resolve(held.items[0], "approve"),
    403,
    "forbidden",
  );
  await refused(
    () =>
      owner.moderation.resolve(
        { ...held.items[0], revision: held.items[0].revision - 1 },
        "approve",
      ),
    404,
    "not_found",
  );
  assert(
    !(await anonymous.comments.list(target)).some((x) => x.id === prior.id),
  );
  const approved = await owner.moderation.resolve(
    held.items[0],
    "approve",
    "Reviewed original text",
  );
  assert.equal(approved.decision, "approve");
  assert(
    (await anonymous.comments.list(target)).some(
      (x) => x.id === prior.id && x.body === "Original retained comment",
    ),
  );
  assert.equal(
    (await owner.moderation.held({ kind: "comment" })).items.length,
    0,
  );
  proved("ModerationClient.held", "ModerationClient.resolve");
} else if (group === "discovery_entities") {
  const tags = await owner.entities.create([
    {
      kind: "tag",
      slug: "ts-shared-tag",
      names: [
        {
          language: "en",
          kind: "name",
          name: "TypeScript shared classification",
        },
      ],
    },
    {
      kind: "tag",
      slug: "ts-removable-tag",
      names: [
        { language: "en", kind: "name", name: "Temporary classification" },
      ],
    },
  ]);
  assert.equal(tags.length, 2);
  assert.notEqual(tags[0].id, tags[1].id);
  assert(tags.every((x) => x.kind === "tag"));
  await refused(
    () => reader.entities.create([{ kind: "tag", slug: "unauthorized-tag" }]),
    403,
    "forbidden",
  );
  const updated = await owner.entities.update(tags[0].id, {
    names: [
      {
        language: "en",
        kind: "name",
        name: "Renamed TypeScript classification",
      },
    ],
  });
  assert.equal(updated.id, tags[0].id);
  assert.equal(
    (await anonymous.entities.get(tags[0].id)).name,
    "Renamed TypeScript classification",
  );
  await refused(() => reader.entities.remove(tags[0].id), 403, "forbidden");
  assert.equal((await anonymous.entities.get(tags[0].id)).id, tags[0].id);
  const removed = await owner.entities.remove(tags[1].id);
  assert.equal(removed.id, tags[1].id);
  assert.equal(removed.state, "deleted");
  await refused(() => anonymous.entities.get(tags[1].id), 404, "not_found");
  const mine = await owner.channels.mine();
  assert.deepEqual(
    mine.channels.map((x) => x.id),
    [fixture.channel],
  );
  assert.deepEqual(
    (await reader.channels.mine()).channels,
    [],
  );
  proved(
    "EntitiesClient.create",
    "EntitiesClient.update",
    "EntitiesClient.remove",
    "ChannelsClient.mine",
  );
  const secret = (
    await owner.entities.create([
      {
        kind: "tag",
        slug: "private-ts-tag",
        internal: true,
        names: [
          { language: "en", kind: "name", name: "InternalTypeScriptSecret" },
        ],
      },
    ])
  )[0];
  const make = async (name, publish = true, locked = false) => {
    const item = await owner.contents.create({
      kind: "article",
      ...(locked ? { access: { level: "tier", tiers: ["gold"] } } : {}),
      text: { en: { title: "TypeScriptNeedle " + name, body: "Body " + name } },
    });
    await owner.entities.setSharedOfContent(item.id, {
      memberships: [{ group_id: tags[0].id }],
    });
    if (publish) {
      await owner.contents.publish(item.id);
      await released(item.id);
    }
    return item;
  };
  const one = await make("Alpha"),
    two = await make("Beta"),
    draft = await make("Draft", false),
    locked = await make("Locked", true, true);
  const query = {
    kind: "article",
    groups: [tags[0].id],
    mode: "hide",
    language: "en",
  };
  const ids = (p) => p.contents.map((x) => x.id);
  const expected = [one.id, two.id].sort();
  const listing = await anonymous.discovery.list({ ...query, limit: 1 });
  assert.equal(listing.contents.length, 1);
  assert(listing.next_cursor);
  const next = await anonymous.discovery.list({
    ...query,
    limit: 1,
    cursor: listing.next_cursor,
  });
  assert.deepEqual([...ids(listing), ...ids(next)].sort(), expected);
  proved("DiscoveryClient.list");
  const known = await anonymous.discovery.knownCards({
    kind: "article",
    mode: "hide",
    ids: [two.id, fixture.foreign, draft.id, locked.id, one.id],
  });
  assert.deepEqual(ids(known), [two.id, one.id]);
  proved("DiscoveryClient.knownCards");
  const deadline = Date.now() + 30_000;
  let found;
  do {
    found = await anonymous.discovery.search({
      ...query,
      q: "TypeScriptNeedle",
    });
    if (ids(found).length === 2) break;
    await new Promise((resolve) => setTimeout(resolve, 50));
  } while (Date.now() < deadline);
  assert.deepEqual(ids(found).sort(), expected);
  proved("DiscoveryClient.search");
  const ahead = await anonymous.discovery.typeahead({
    q: "TypeScriptNeedle",
    kind: "article",
    mode: "hide",
  });
  assert.deepEqual(ahead.contents.map((x) => x.id).sort(), expected);
  assert.equal(
    (
      await anonymous.discovery.typeahead({
        q: "InternalTypeScriptSecret",
        entity_kinds: ["tag"],
      })
    ).entities.length,
    0,
  );
  proved("DiscoveryClient.typeahead");
  const adminQuery = {
    q: "InternalTypeScriptSecret",
    include_contents: false,
    include_entities: true,
    entity_kinds: ["tag"],
  };
  assert.deepEqual(
    (await owner.discovery.adminSuggestions(adminQuery)).entities.map(
      (x) => x.id,
    ),
    [secret.id],
  );
  await refused(
    () => anonymous.discovery.adminSuggestions(adminQuery),
    401,
    "unauthorized",
  );
  await refused(
    () => reader.discovery.adminSuggestions(adminQuery),
    403,
    "forbidden",
  );
  proved("DiscoveryClient.adminSuggestions");
  assert.deepEqual(ids(await anonymous.discovery.similar(one.id, query)), [
    two.id,
  ]);
  proved("DiscoveryClient.similar");
  assert.deepEqual(
    ids(await reader.discovery.recommendations(query)).sort(),
    expected,
  );
  proved("DiscoveryClient.recommendations");
  const dates = await anonymous.discovery.releaseDates({ ...query, by: "day" });
  assert.equal(dates.dates.length, 1);
  assert.equal(dates.dates[0].count, 2);
  proved("DiscoveryClient.releaseDates");
  const releases = await anonymous.discovery.releases({ ...query, days: 1 });
  assert.deepEqual(
    releases.days.flatMap((day) => day.contents.map((x) => x.id)).sort(),
    expected,
  );
  proved("DiscoveryClient.releases");
  const library = await reader.discovery.library({ language: "en", limit: 1 });
  assert.equal(library.complete, true);
  assert.equal(library.items.length, 1);
  assert.equal(library.items[0].content_id, fixture.paid);
  assert.equal(library.items[0].whole, false);
  assert.deepEqual(library.items[0].version_ids, [fixture.paidVersion]);
  assert.deepEqual((await owner.discovery.library()).items, []);
  await refused(() => anonymous.discovery.library(), 401, "unauthorized");
  proved("DiscoveryClient.library");

  const channel = await owner.channels.get(fixture.channel);
  assert.equal(channel.id, fixture.channel);
  assert(!("kind" in channel), "channel is not a metadata entity");
  const destination = await reader.channels.create({ slug: "typescript-repost-home", names: [{ language: "en", kind: "name", name: "Repost Home" }] });
  assert.equal((await reader.channels.mine()).channels.find((entry) => entry.id === destination.id)?.can_post, true);
  assert.deepEqual((await anonymous.channels.list({ slug: destination.slug })).channels.map((entry) => entry.id), [destination.id]);
  await reader.channels.update(destination.id, { names: [{ language: "en", kind: "name", name: "Updated Repost Home" }] });
  assert.equal((await anonymous.channels.get(destination.id)).name, "Updated Repost Home");
  await reader.channels.setEntities(destination.id, [tags[0].id]);
  assert.deepEqual((await anonymous.channels.entities(destination.id)).map((entry) => entry.id), [tags[0].id]);
  const original = await owner.contents.create({ kind: "article", channel_id: channel.id, text: { en: { title: "A canonical channel original", body: "One original across destinations" } } });
  await owner.contents.publish(original.id);
  await released(original.id);
  await reader.channels.repost(destination.id, original.id);
  await reader.channels.repost(destination.id, original.id);
  const feed = await reader.channels.contents(destination.id, { language: "en" });
  assert.deepEqual(ids(feed), [original.id], "retries must not duplicate a repost");
  assert.equal(feed.contents[0].channel.id, channel.id, "repost retains the original channel");
  assert.equal(feed.contents[0].repost.channel_id, destination.id);
  assert.equal(feed.contents[0].repost.can_remove, true);
  await refused(() => reader.contents.update(original.id, { access: { level: "public" } }), 403, "forbidden");
  await reader.channels.removeRepost(destination.id, original.id);
  assert.deepEqual(ids(await anonymous.channels.contents(destination.id)), []);
  assert.equal((await anonymous.contents.get(original.id)).id, original.id, "removing a repost must keep the original");
  const channelTarget = { type: "channels", id: destination.id };
  assert.equal((await owner.favorites.set(channelTarget, true)).favorited, true);
  assert.equal((await owner.favorites.get(channelTarget)).favorited, true);
  assert.deepEqual((await owner.favorites.mine({ target: "channels" })).items.map((entry) => entry.channel?.id), [destination.id]);
  assert.equal((await owner.reactions.set(channelTarget, 1)).likes, 1);
  assert.equal((await owner.reactions.get(channelTarget)).likes, 1);
  await owner.reactions.set(channelTarget, 0);
  await owner.favorites.set(channelTarget, false);
  assert.deepEqual((await owner.favorites.mine({ target: "channels" })).items, []);
  await reader.channels.remove(destination.id);
  await refused(() => anonymous.channels.get(destination.id), 404, "not_found");
  proved("ChannelsClient.create", "ChannelsClient.get", "ChannelsClient.list", "ChannelsClient.update", "ChannelsClient.entities", "ChannelsClient.setEntities", "ChannelsClient.repost", "ChannelsClient.removeRepost", "ChannelsClient.contents", "ChannelsClient.remove");
} else if (group.startsWith("media_")) {
  bindings.push(...(await runMediaContracts(group, fixture)));
} else throw new Error(`Unknown contract group ${group}`);
console.log(JSON.stringify({ group, bindings, passed: true }));
