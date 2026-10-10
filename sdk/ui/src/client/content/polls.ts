import type { Poll, PollImage, PollInput, PollOption, PollOptionPatch, PollUpdate } from "../generated/wire.js";
import type { Http } from "../http.js";
import type { MediaClient, NamedOptions } from "../media/client.js";
import { call, type PageQuery } from "../route.js";

export interface PollQuery extends PageQuery {
  language?: string;
  /** Polls live in this month, YYYY-MM. */
  month?: string;
  /** Polls live on this day, YYYY-MM-DD. */
  date?: string;
}

/** Site-wide polls: multiple choice or free text; staff write them (PollWrite). */
export class PollsClient {
  constructor(
    private readonly http: Http,
    private readonly media: MediaClient,
  ) {}

  /** Live polls, newest first. */
  list(q: PollQuery = {}, signal?: AbortSignal): Promise<Poll[]> {
    return call(this.http, "GET", "/polls", { query: q, signal });
  }
  /** Every poll, scheduled and inactive ones included (PollWrite). */
  adminList(q: PollQuery = {}, signal?: AbortSignal): Promise<Poll[]> {
    return call(this.http, "GET", "/polls/admin", { query: q, signal });
  }
  /** A poll with the caller's vote or answer. */
  get(id: string, signal?: AbortSignal): Promise<Poll> {
    return call(this.http, "GET", "/polls/{id}", { params: { id }, signal });
  }
  /** Creates a poll with its options (images are set once it exists). */
  async create(input: PollInput): Promise<Poll> {
    const poll: Poll = await call(this.http, "POST", "/polls", { body: input });
    this.http.emit({ type: "poll.created", poll });
    return poll;
  }
  /** Updates the given fields. */
  async update(id: string, patch: PollUpdate): Promise<Poll> {
    const poll: Poll = await call(this.http, "PATCH", "/polls/{id}", { params: { id }, body: patch });
    this.http.emit({ type: "poll.updated", poll });
    return poll;
  }
  /** Deletes the poll and its images. */
  async delete(id: string): Promise<void> {
    await call(this.http, "DELETE", "/polls/{id}", { params: { id } });
    this.http.emit({ type: "poll.deleted", id });
  }
  /** Votes for an option of an open multiple-choice poll; a vote is final, and voting again changes nothing. */
  async vote(id: string, option: string): Promise<Poll> {
    const poll: Poll = await call(this.http, "POST", "/polls/{id}/vote", { params: { id }, body: { option_id: option } });
    this.http.emit({ type: "poll.updated", poll });
    return poll;
  }
  /** Stores or replaces the signed-in caller's answer to an open free-text poll. */
  async answer(id: string, text: string): Promise<Poll> {
    const poll: Poll = await call(this.http, "POST", "/polls/{id}/answer", { params: { id }, body: { text } });
    this.http.emit({ type: "poll.updated", poll });
    return poll;
  }
  /** Sets the question image to an inline image name of the poll's folder, or clears it; resolves with its URL. */
  async setImage(id: string, image: string | null): Promise<string | null> {
    const r: PollImage = await call(this.http, "PUT", "/polls/{id}/image", { params: { id }, body: { image: image ?? "" } });
    this.http.emit({ type: "poll.changed", id });
    return r.image_url;
  }
  /** Uploads file to the poll's folder as the question image (null clears it). */
  async uploadImage(id: string, file: Blob | null, o?: NamedOptions): Promise<string | null> {
    return this.setImage(id, file ? (await this.media.uploadNamed(this.media.pollRef(id), file, o)).name : null);
  }
  /** Adds an option, at the end unless position is given. */
  async addOption(id: string, option: PollOptionPatch): Promise<PollOption> {
    const o: PollOption = await call(this.http, "POST", "/polls/{id}/options", { params: { id }, body: option });
    this.http.emit({ type: "poll.changed", id });
    return o;
  }
  async updateOption(id: string, option: string, patch: PollOptionPatch): Promise<PollOption> {
    const o: PollOption = await call(this.http, "PATCH", "/polls/{id}/options/{oid}", { params: { id, oid: option }, body: patch });
    this.http.emit({ type: "poll.changed", id });
    return o;
  }
  /** Removes an option and its votes; a poll keeps at least two. */
  async deleteOption(id: string, option: string): Promise<void> {
    await call(this.http, "DELETE", "/polls/{id}/options/{oid}", { params: { id, oid: option } });
    this.http.emit({ type: "poll.changed", id });
  }
  /** Gives the options the positions of their order, patching those that moved. */
  async reorderOptions(id: string, options: readonly Pick<PollOption, "id" | "position">[]): Promise<void> {
    for (const [position, o] of options.entries()) {
      if (o.position !== position) await call(this.http, "PATCH", "/polls/{id}/options/{oid}", { params: { id, oid: o.id }, body: { position } });
    }
    this.http.emit({ type: "poll.changed", id });
  }
  async setOptionImage(id: string, option: string, image: string | null): Promise<string | null> {
    const r: PollImage = await call(this.http, "PUT", "/polls/{id}/options/{oid}/image", { params: { id, oid: option }, body: { image: image ?? "" } });
    this.http.emit({ type: "poll.changed", id });
    return r.image_url;
  }
  /** Uploads file to the poll's folder as the option's image (null clears it). */
  async uploadOptionImage(id: string, option: string, file: Blob | null, o?: NamedOptions): Promise<string | null> {
    return this.setOptionImage(id, option, file ? (await this.media.uploadNamed(this.media.pollRef(id), file, o)).name : null);
  }
}
