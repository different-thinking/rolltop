// File overview: Typed browser API client. It centralizes JSON parsing, CSRF-bearing writes,
// ETag-aware GET caching, multipart compose uploads, and endpoint shapes used by views.

import type {
  Account,
  AccountPurgeEstimate,
  Activity,
  Bootstrap,
  CalendarEvent,
  CalendarEventInput,
  CalendarSummary,
  Contact,
  ContactAutocomplete,
  DatabaseOverview,
  DuplicateCopyReport,
  DuplicateScanResult,
  ComposeAttachmentUpload,
  ComposeForm,
  ComposeIdentity,
  Conversation,
  FolderProgress,
  MailListResponse,
  Mailbox,
  MailIdentity,
  MessageSnooze,
  MessageOriginalSource,
  PluginSetting,
  SMTPAccount,
  SMTPLogSession,
  SMTPTestResult,
  ScopeMoveResponse,
  SearchExplanation,
  SearchIndexReport,
  SearchRebuildBlock,
  ServerLogLine,
  StorageStats,
  SwipePreferences,
  SyncFolder,
  SyncRun,
  SyncRunLiveDetail,
  ThreadMessage,
  User
} from "./types";
import { clearMailSnapshots, clearOtherMailSnapshots, forgetMessagesInSnapshots, loadMailSnapshot, saveMailSnapshot } from "./lib/mailSnapshot";
import { clearFiledMessages, clearOtherFiledMessages, filedMessageIDs, filterFiledConversations, setFiledMessagesUser } from "./lib/filedMessages";
import { clearOtherMailSortOrders, defaultMailSortOrder } from "./lib/mailSort";
import type { MailSortOrder } from "./lib/mailSort";
import type { MailView } from "./lib/routes";
import { clearOtherSidebarState } from "./lib/sidebarLocal";

/** Error thrown for non-2xx API responses after the JSON error payload is decoded. */
export class ApiError extends Error {
  status: number;

  /** payload is the decoded response body. A few failures carry data the caller
   * needs -- a rejected contact edit answers with the version that won -- and
   * without this the only way to get it would be to re-fetch. */
  payload: Record<string, unknown>;

  constructor(status: number, message: string, payload: Record<string, unknown> = {}) {
    super(message);
    this.status = status;
    this.payload = payload;
  }
}

/**
 * nonAPIErrorMessage names a failure whose body did not parse as the JSON every
 * Rolltop route answers with. Something in front of the app answered instead --
 * a proxy, the hosting platform, a gateway that timed out -- or the answer was
 * cut off on the way. The body is then a whole HTML document or a fragment of a
 * payload, which a toast used to print verbatim: a screenful of markup instead
 * of a reason. The status the response really carried is the one thing every
 * such failure has, so it is always said; what is added to it is whatever
 * sentence the body actually holds, and nothing when it holds none.
 */
function nonAPIErrorMessage(res: Response, body: string): string {
  // The reason phrase is read once and defended once. It is a string per the
  // Fetch specification, but not in every stand-in for a Response, and a helper
  // that only runs when something has already failed must not be what throws.
  const statusText = (res.statusText || "").trim();
  const status = statusText ? `${res.status} ${statusText}` : String(res.status);
  const trimmed = body.trim();
  if (!trimmed) return status;
  if (trimmed.startsWith("<")) {
    const title = /<title[^>]*>([\s\S]*?)<\/title>/i.exec(trimmed)?.[1]?.trim();
    return `${status}: ${title || "the server did not answer with a Rolltop response."}`;
  }
  // A body that begins as JSON and does not parse is a payload the connection
  // cut short, so there is no sentence in it to quote -- only the fragment that
  // got through, which is what the raw dump used to be.
  if (trimmed.startsWith("{") || trimmed.startsWith("[")) {
    return `${status}: the server's answer was cut short.`;
  }
  const firstLine = trimmed.split("\n", 1)[0].trim();
  // A plain-text body that only repeats the status ("Bad Gateway") would
  // otherwise be said twice in one sentence.
  if (statusText && firstLine.toLowerCase() === statusText.toLowerCase()) return status;
  const excerpt = firstLine.length > 200 ? `${firstLine.slice(0, 200)}…` : firstLine;
  return `${status}: ${excerpt}`;
}

// All API helpers flow through parse so callers see typed payloads on success
// and a consistent ApiError on backend validation/session failures.
async function parse<T>(res: Response): Promise<T> {
  const text = await res.text();
  let data: Record<string, unknown> = {};
  if (text) {
    try {
      data = JSON.parse(text);
    } catch (err) {
      if (!res.ok) {
        throw new ApiError(res.status, nonAPIErrorMessage(res, text));
      }
      throw err;
    }
  }
  if (!res.ok) {
    throw new ApiError(res.status, typeof data.error === "string" ? data.error : res.statusText, data);
  }
  return data as T;
}

type SnoozeListResponse = MailListResponse & { snoozes: MessageSnooze[] };

// A send that SMTP accepted but that could not be filed locally answers ok with
// a warning rather than an error: the mail is gone and retrying would duplicate
// it. The field is optional and must stay read by the caller -- dropping it is
// how a missing Sent copy becomes silent.
export type ComposeSendResult = {
  ok: boolean;
  message_id?: number;
  sent?: boolean;
  warning?: string;
  archived_mailbox?: string;
};

const getCache = new Map<string, { etag: string; data: unknown }>();
const getInflight = new Map<string, Promise<unknown>>();
const mailCacheEpochs = new Map<number, number>();
// The signed-in user, so callers that only hold message ids - a list row, an
// open message - can still reach this user's cached pages and filings.
let activeMailUserID = 0;
type MutationRequestOptions = { keepalive?: boolean };

async function fetchGET(url: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(url, init);
  } catch (err) {
    await new Promise((resolve) => window.setTimeout(resolve, 250));
    try {
      return await fetch(url, init);
    } catch {
      throw err;
    }
  }
}

/**
 * GET JSON with lightweight ETag revalidation. Callers can supply an explicit
 * scope key when the same URL may return data for different authenticated users.
 */
export async function getJSON<T>(url: string, cacheKey = url): Promise<T> {
  const inflight = getInflight.get(cacheKey);
  if (inflight) return inflight as Promise<T>;

  const request = (async () => {
    const headers: Record<string, string> = { Accept: "application/json" };
    const cached = getCache.get(cacheKey);
    if (cached?.etag) headers["If-None-Match"] = cached.etag;
    const res = await fetchGET(url, { headers });
    if (res.status === 304 && cached) return cached.data as T;
    const data = await parse<T>(res);
    const etag = res.headers.get("ETag") || "";
    if (etag) getCache.set(cacheKey, { etag, data });
    else getCache.delete(cacheKey);
    return data;
  })();

  getInflight.set(cacheKey, request);
  try {
    return await request;
  } finally {
    if (getInflight.get(cacheKey) === request) getInflight.delete(cacheKey);
  }
}

/** POST JSON to a mutating endpoint with the current CSRF token. */
export async function postJSON<T>(url: string, csrf: string, body: unknown = {}, options: MutationRequestOptions = {}): Promise<T> {
  return parse<T>(
    await fetch(url, {
      method: "POST",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        "X-CSRF-Token": csrf
      },
      body: JSON.stringify(body),
      ...(options.keepalive ? { keepalive: true } : {})
    })
  );
}

/** PUT JSON to a mutating endpoint with the current CSRF token. */
export async function putJSON<T>(url: string, csrf: string, body: unknown = {}, options: MutationRequestOptions = {}): Promise<T> {
  return parse<T>(
    await fetch(url, {
      method: "PUT",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        "X-CSRF-Token": csrf
      },
      body: JSON.stringify(body),
      ...(options.keepalive ? { keepalive: true } : {})
    })
  );
}

/** DELETE JSON from a mutating endpoint with the current CSRF token. */
export async function deleteJSON<T>(url: string, csrf: string, options: MutationRequestOptions = {}): Promise<T> {
  return parse<T>(
    await fetch(url, {
      method: "DELETE",
      headers: {
        Accept: "application/json",
        "X-CSRF-Token": csrf
      },
      ...(options.keepalive ? { keepalive: true } : {})
    })
  );
}

// The backend caps bulk message endpoints at 1000 IDs per request. Larger
// batches are split here and dispatched together so every caller inherits the
// limit, and so background (keepalive) commits hand all requests to the
// browser before the page can unload.
export const bulkMessageIDLimit = 1000;

function chunkMessageIDs(ids: number[]): number[][] {
  if (ids.length <= bulkMessageIDLimit) return [ids];
  const chunks: number[][] = [];
  for (let start = 0; start < ids.length; start += bulkMessageIDLimit) {
    chunks.push(ids.slice(start, start + bulkMessageIDLimit));
  }
  return chunks;
}

/** DELETE JSON with a request body for endpoints keyed by payload rather than path. */
export async function deleteJSONBody<T>(url: string, csrf: string, body: unknown = {}): Promise<T> {
  return parse<T>(
    await fetch(url, {
      method: "DELETE",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        "X-CSRF-Token": csrf
      },
      body: JSON.stringify(body)
    })
  );
}

/** POST multipart form data without forcing a Content-Type boundary. */
export async function postForm<T>(url: string, csrf: string, body: FormData): Promise<T> {
  return parse<T>(
    await fetch(url, {
      method: "POST",
      headers: {
        Accept: "application/json",
        "X-CSRF-Token": csrf
      },
      body
    })
  );
}

function composeSendPayload(form: ComposeForm): ComposeForm {
  const { available_attachments: _availableAttachments, forward_attachment: _forwardAttachment, ...payload } = form;
  return payload;
}

// The default order stays off the URL so warmed first pages, service caches, and
// clients that never touch sorting all keep asking for the same address.
function mailListURL(mailboxID: string | null, page: number, order: MailSortOrder = defaultMailSortOrder, view: MailView = "") {
  const q = new URLSearchParams({ page: String(page) });
  if (mailboxID) q.set("mailbox", mailboxID);
  else if (view) q.set("view", view);
  if (order !== defaultMailSortOrder) q.set("sort", order);
  return `/api/mail?${q}`;
}

function mailCacheEpoch(userID: number) {
  const current = mailCacheEpochs.get(userID);
  if (current !== undefined) return current;
  mailCacheEpochs.set(userID, 0);
  return 0;
}

function mailListCacheKey(userID: number, mailboxID: string | null, page: number, order: MailSortOrder, view: MailView, epoch = mailCacheEpoch(userID)) {
  return `user:${userID}:mail-epoch:${epoch}:${mailListURL(mailboxID, page, order, view)}`;
}

function searchListURL(query: string, page: number) {
  const q = new URLSearchParams({ q: query, page: String(page) });
  return `/api/search?${q}`;
}

function messageURL(id: string | number, showImages: boolean, highlightQuery = "") {
  const params = new URLSearchParams();
  if (showImages) params.set("images", "1");
  if (highlightQuery.trim()) params.set("q", highlightQuery.trim());
  const q = params.toString() ? `?${params}` : "";
  return `/api/messages/${id}${q}`;
}

function prefetchJSON<T>(url: string, cacheKey = url) {
  void getJSON<T>(url, cacheKey).catch(() => undefined);
}

function cachedJSON<T>(cacheKey: string): T | null {
  const cached = getCache.get(cacheKey);
  return cached ? cached.data as T : null;
}

function cachedMail(userID: number, mailboxID: string | null, page: number, order: MailSortOrder = defaultMailSortOrder, view: MailView = ""): MailListResponse | null {
  const key = mailListCacheKey(userID, mailboxID, page, order, view);
  const cached = cachedJSON<MailListResponse>(key);
  // Named views keep no localStorage snapshot: snapshot keys only name
  // mailbox/all pages, and an All Mail page painting into one of them would
  // show exactly the rows that view exists to leave out.
  const stored = cached || (view ? null : loadMailSnapshot(userID, mailboxID, page, order));
  return stored && withoutFiledConversations(stored);
}

// withoutFiledConversations is applied to every list page this module hands
// back, from the network or from either cache. A page the reader filed mail out
// of is stale in exactly one way, and it is the way that puts deleted mail back
// on screen; the filing itself decides when the row is due again.
function withoutFiledConversations<T extends MailListResponse>(page: T): T {
  const conversations = filterFiledConversations(page.conversations);
  return conversations.length === page.conversations.length ? page : { ...page, conversations };
}

async function loadMail(userID: number, mailboxID: string | null, page: number, order: MailSortOrder = defaultMailSortOrder, view: MailView = ""): Promise<MailListResponse> {
  const url = mailListURL(mailboxID, page, order, view);
  const epoch = mailCacheEpoch(userID);
  const key = mailListCacheKey(userID, mailboxID, page, order, view, epoch);
  // Filed rows come off the page before it is stored, not only before it is
  // handed back: a snapshot written with them outlives the filing that hides
  // them, and would paint deleted mail again once the filing expired.
  const data = withoutFiledConversations(await getJSON<MailListResponse>(url, key));
  if (mailCacheEpoch(userID) !== epoch) {
    getCache.delete(key);
    return data;
  }
  if (!view) saveMailSnapshot(userID, mailboxID, page, data, order);
  return data;
}

function prefetchMail(userID: number, mailboxID: string | null, page: number, order: MailSortOrder = defaultMailSortOrder, view: MailView = "") {
  void loadMail(userID, mailboxID, page, order, view).catch(() => undefined);
}

function clearMailCache(userID: number) {
  mailCacheEpochs.set(userID, mailCacheEpoch(userID) + 1);
  const prefix = `user:${userID}:mail-epoch:`;
  for (const key of getCache.keys()) {
    if (key.startsWith(prefix)) getCache.delete(key);
  }
  clearMailSnapshots(userID);
}

/**
 * forgetUserMail drops everything this browser holds about one user's mail, for
 * a sign-out. Filings go with it - they are the reader's own pending decisions
 * and belong to their session - which is why `clearMailCache` leaves them
 * alone: a dropped cache is a page this browser can no longer trust, and mail
 * the reader deleted a moment ago is not made undeleted by that.
 */
function forgetUserMail(userID: number) {
  clearMailCache(userID);
  clearFiledMessages(userID);
}

function retainMailCacheForUser(userID: number) {
  activeMailUserID = userID;
  setFiledMessagesUser(userID);
  clearOtherFiledMessages(userID);
  // The stored pages are rewritten once per start rather than only filtered on
  // the way out, so mail filed away in an earlier session is gone from them
  // before its filing expires - a snapshot outlives the record that hides it.
  forgetFiledMessages();
  for (const cachedUserID of mailCacheEpochs.keys()) {
    if (cachedUserID !== userID) clearMailCache(cachedUserID);
  }
  for (const key of getCache.keys()) {
    const match = key.match(/^user:(\d+):mail-epoch:/);
    if (match && Number(match[1]) !== userID) getCache.delete(key);
  }
  clearOtherMailSnapshots(userID);
  clearOtherMailSortOrders(userID);
  clearOtherSidebarState(userID);
}

/**
 * forgetMessages takes messages out of every list page this browser is holding -
 * the in-memory pages, the prefetched neighbours, the localStorage snapshots -
 * for mail the reader filed away or that the server says it no longer has.
 * Filtering the pages on the way out (`withoutFiledConversations`) is what keeps
 * a filed row off screen; this is what stops the stale copy outliving the
 * filing, so a page whose ETag never changes again cannot bring it back.
 *
 * The rewritten page keeps its data and loses its ETag: a revalidation that
 * answered 304 would otherwise be answered from the copy edited here, which is
 * an answer about mail rather than about the page the server holds.
 */
function forgetMessages(messageIDs: number[]) {
  const gone = new Set(messageIDs.filter((id) => Number.isInteger(id) && id > 0));
  if (gone.size === 0) return;
  for (const [key, entry] of getCache) {
    const data = entry.data as { conversations?: unknown } | null;
    if (!data || !Array.isArray(data.conversations)) continue;
    const conversations = (data.conversations as Conversation[]).filter((conversation) => !gone.has(conversation.message.id));
    if (conversations.length === data.conversations.length) continue;
    getCache.set(key, { etag: "", data: { ...data, conversations } });
  }
  if (activeMailUserID > 0) forgetMessagesInSnapshots(activeMailUserID, Array.from(gone));
}

/** forgetFiledMessages drops what is currently filed from the cached pages too. */
function forgetFiledMessages() {
  forgetMessages(filedMessageIDs());
}

// The api object is deliberately explicit rather than generated: it documents
// the route surface used by the current frontend and keeps response shapes close
// to the call sites that depend on them.
export const api = {
  bootstrap: () => getJSON<Bootstrap>("/api/bootstrap"),
  setup: (csrf: string, body: { email: string; name: string; password: string }) =>
    postJSON<{ ok: boolean }>("/api/setup", csrf, body),
  login: (csrf: string, body: { email: string; password: string }) => postJSON<{ ok: boolean }>("/api/login", csrf, body),
  logout: (csrf: string) => postJSON<{ ok: boolean }>("/api/logout", csrf),
  pushVAPIDPublicKey: () => getJSON<{ public_key: string }>("/api/push/vapid-public-key"),
  savePushSubscription: (csrf: string, subscription: unknown) =>
    postJSON<{ ok: boolean; subscription_id: number }>("/api/push/subscription", csrf, subscription),
  deletePushSubscription: (csrf: string, endpoint: string) =>
    deleteJSONBody<{ ok: boolean }>("/api/push/subscription", csrf, { endpoint }),
  mail: loadMail,
  cachedMail,
  prefetchMail,
  clearMailCache,
  forgetUserMail,
  retainMailCacheForUser,
  forgetMessages,
  snoozes: (page: number) => getJSON<SnoozeListResponse>(`/api/snoozes?${new URLSearchParams({ page: String(page) })}`)
    .then(withoutFiledConversations),
  snoozeMessage: (csrf: string, id: number, until: Date, options?: MutationRequestOptions) =>
    putJSON<{ ok: boolean; snoozed: boolean; snooze: MessageSnooze }>(`/api/messages/${id}/snooze`, csrf, { until: until.toISOString() }, options),
  unsnoozeMessage: (csrf: string, id: number, options?: MutationRequestOptions) =>
    deleteJSON<{ ok: boolean; snoozed: boolean }>(`/api/messages/${id}/snooze`, csrf, options),
  search: (query: string, page: number) =>
    getJSON<{ conversations: Conversation[]; page: number; has_prev: boolean; has_next: boolean }>(searchListURL(query, page))
      .then(withoutFiledConversations),
  prefetchSearch: (query: string, page: number) =>
    prefetchJSON<{ conversations: Conversation[]; page: number; has_prev: boolean; has_next: boolean }>(searchListURL(query, page)),
  brandIcons: (domains: string[]) => {
    const q = new URLSearchParams();
    domains.slice(0, 40).forEach((domain) => q.append("domain", domain));
    return getJSON<{ icons: Record<string, string> }>(`/api/brand-icons?${q}`);
  },
  message: (id: string, showImages: boolean, highlightQuery = "") =>
    getJSON<{
      message: { id: number; account_id: number; subject: string; mailbox_id: number };
      thread: ThreadMessage[];
      compose_from: string;
      from_identities: ComposeIdentity[];
      mailbox_id: number;
        conversation: number;
        snoozed_until?: string;
      }>(messageURL(id, showImages, highlightQuery)),
  prewarmMessage: (id: string | number) =>
    prefetchJSON(`/api/messages/${id}/prefetch`),
  messageLoadStatus: (id: string) =>
    getJSON<{
      conversation: number;
      imap_fetch_count: number;
      local_blob_count: number;
      indexed_count: number;
      unavailable_count: number;
      source: "imap" | "local_blob" | "local" | "indexed" | "preview" | string;
    }>(`/api/messages/${id}/load-status`),
  messageOriginal: (id: number) => getJSON<MessageOriginalSource>(`/api/messages/${id}/original`),
  searchExplanation: (id: number, query: string, hitID = 0) => {
    const params = new URLSearchParams();
    params.set("q", query.trim());
    if (hitID > 0) params.set("hit", String(hitID));
    return getJSON<SearchExplanation>(`/api/messages/${id}/search-explanation?${params}`);
  },
  trustImages: (csrf: string, id: number) => postJSON<{ ok: boolean }>(`/api/messages/${id}/images/trust`, csrf),
  unsubscribe: (csrf: string, id: number) =>
    postJSON<{ ok: boolean; already_sent: boolean; sent_at: string }>(`/api/messages/${id}/unsubscribe`, csrf),
  setStarred: (csrf: string, id: number, starred: boolean) =>
    postJSON<{ ok: boolean; message: { id: number; is_starred: boolean } }>(`/api/messages/${id}/star`, csrf, { starred }),
  moveMessage: (csrf: string, id: number, mailboxID: number, options?: MutationRequestOptions) =>
    postJSON<{ ok: boolean; mailbox: string }>(`/api/messages/${id}/move`, csrf, { mailbox_id: mailboxID }, options),
  bulkMoveMessages: async (csrf: string, ids: number[], mailboxID: number, options?: MutationRequestOptions) => {
    const results = await Promise.all(chunkMessageIDs(ids).map((chunk) =>
      postJSON<{ ok: boolean; queued: boolean; moved?: number; run_id?: number; mailbox: string }>("/api/messages/bulk-move", csrf, {
        message_ids: chunk,
        mailbox_id: mailboxID
      }, options)));
    // run_ids collects every background run this move started, so a caller that
    // hides the moved rows can wait for all of them, not just the last answer.
    return results.reduce<{ ok: boolean; queued: boolean; moved?: number; run_id?: number; run_ids: number[]; mailbox: string }>((merged, data) => ({
      ok: merged.ok && data.ok,
      queued: merged.queued || data.queued,
      moved: (merged.moved || 0) + (data.moved || 0),
      run_id: data.run_id ?? merged.run_id,
      run_ids: data.run_id ? [...merged.run_ids, data.run_id] : merged.run_ids,
      mailbox: data.mailbox || merged.mailbox
    }), { ok: true, queued: false, moved: 0, run_ids: [], mailbox: "" });
  },
  // The scope endpoint takes the filter rather than message IDs, so a delete is
  // not limited to the IDs one page happens to have loaded. The server resolves
  // the matches, groups them per account Trash, and answers with the runs it
  // started; progress arrives through the normal sync-run events.
  scopeTrashMessages: (csrf: string, scope: { mailboxID: number; query: string; view?: MailView }) =>
    postJSON<ScopeMoveResponse>("/api/messages/scope-trash", csrf, {
      scope_mailbox_id: scope.mailboxID,
      scope_query: scope.query,
      scope_view: scope.view || ""
    }),
  // Archiving by date uses the same scope description plus the cutoff. The
  // cutoff is the exact instant the chosen day begins in the reader's own
  // timezone, so the day they name is kept whole wherever they are.
  scopeArchiveMessages: (csrf: string, scope: { mailboxID: number; query: string; view?: MailView; before: string }) =>
    postJSON<ScopeMoveResponse>("/api/messages/scope-archive", csrf, {
      scope_mailbox_id: scope.mailboxID,
      scope_query: scope.query,
      scope_view: scope.view || "",
      before: scope.before
    }),
  // Emptying the Trash is the one action that deletes mail on the IMAP server
  // instead of moving it, so it names a single folder and nothing else.
  emptyTrash: (csrf: string, mailboxID: number) =>
    postJSON<{ ok: boolean; run_id: number; mailbox: string }>("/api/messages/empty-trash", csrf, {
      mailbox_id: mailboxID
    }),
  bulkCopyMessages: async (csrf: string, ids: number[], mailboxID: number) => {
    const results = await Promise.all(chunkMessageIDs(ids).map((chunk) =>
      postJSON<{ ok: boolean; queued: boolean; copied?: number; run_id?: number; mailbox: string }>("/api/messages/bulk-copy", csrf, {
        message_ids: chunk,
        mailbox_id: mailboxID
      })));
    return results.reduce((merged, data) => ({
      ok: merged.ok && data.ok,
      queued: merged.queued || data.queued,
      copied: (merged.copied || 0) + (data.copied || 0),
      run_id: data.run_id ?? merged.run_id,
      mailbox: data.mailbox || merged.mailbox
    }));
  },
  bulkRead: async (csrf: string, ids: number[], read: boolean, options?: MutationRequestOptions) => {
    const results = await Promise.all(chunkMessageIDs(ids).map((chunk) =>
      postJSON<{ ok: boolean; updated: number }>("/api/messages/bulk-read", csrf, { ids: chunk, read }, options)));
    return results.reduce((merged, data) => ({ ok: merged.ok && data.ok, updated: merged.updated + data.updated }));
  },
  compose: (query: string) =>
    getJSON<{ compose: ComposeForm; compose_from: string; from_identities: ComposeIdentity[] }>(`/api/compose${query ? `?${query}` : ""}`),
  // Compose sends pure JSON when possible, then switches to multipart only when
  // there are file bodies. Inline files are represented in the JSON payload by
  // stable form field names and Content-ID metadata.
  send: (csrf: string, form: ComposeForm, attachments: ComposeAttachmentUpload[] = []) => {
    const payload = composeSendPayload(form);
    if (attachments.length === 0) {
      return postJSON<ComposeSendResult>("/api/compose", csrf, payload);
    }
    const body = new FormData();
    body.append("payload", JSON.stringify({
      ...payload,
      attachments: attachments.map((attachment) => ({
        field: attachment.field,
        filename: attachment.filename,
        content_type: attachment.content_type,
        content_id: attachment.content_id,
        inline: attachment.inline,
        size: attachment.size
      }))
    }));
    attachments.forEach((attachment) => body.append(attachment.field, attachment.file, attachment.filename));
    return postForm<ComposeSendResult>("/api/compose", csrf, body);
  },
  saveDraft: (csrf: string, form: ComposeForm, attachments: ComposeAttachmentUpload[] = []) => {
    const payload = composeSendPayload(form);
    if (attachments.length === 0) {
      return postJSON<{ ok: boolean; message_id: number }>("/api/compose/draft", csrf, payload);
    }
    const body = new FormData();
    body.append("payload", JSON.stringify({
      ...payload,
      attachments: attachments.map((attachment) => ({
        field: attachment.field,
        filename: attachment.filename,
        content_type: attachment.content_type,
        content_id: attachment.content_id,
        inline: attachment.inline,
        size: attachment.size
      }))
    }));
    attachments.forEach((attachment) => body.append(attachment.field, attachment.file, attachment.filename));
    return postForm<{ ok: boolean; message_id: number }>("/api/compose/draft", csrf, body);
  },
  // source is "", "all", "local", or "google:<connection id>". It is a server
  // parameter rather than a filter over the answer because the listing is
  // capped, and filtering afterwards would hide contacts the cap already cut.
  contacts: (query = "", source = "") => {
    const params = new URLSearchParams();
    if (query.trim()) params.set("q", query.trim());
    if (source && source !== "all") params.set("source", source);
    const suffix = params.size > 0 ? `?${params}` : "";
    return getJSON<{ contacts: Contact[] }>(`/api/contacts${suffix}`);
  },
  contactAutocomplete: (query: string) =>
    getJSON<{ contacts: ContactAutocomplete[] }>(`/api/contacts/autocomplete?${new URLSearchParams({ q: query })}`),
  createContact: (csrf: string, contact: Contact) => postJSON<{ contact: Contact }>("/api/contacts", csrf, contact),
  updateContact: (csrf: string, contact: Contact) => putJSON<{ contact: Contact }>(`/api/contacts/${contact.id}`, csrf, contact),
  deleteContact: (csrf: string, id: number) => deleteJSON<{ ok: boolean }>(`/api/contacts/${id}`, csrf),
  uploadContactIcon: (csrf: string, id: number, file: File) => {
    const form = new FormData();
    form.append("icon", file);
    return postForm<{ contact: Contact }>(`/api/contacts/${id}/icon`, csrf, form);
  },
  deleteContactIcon: (csrf: string, id: number) => deleteJSON<{ contact: Contact }>(`/api/contacts/${id}/icon`, csrf),
  importContacts: (csrf: string, file: File) => {
    const form = new FormData();
    form.append("file", file);
    return postForm<{ ok: boolean; imported: number; updated: number; failed: number }>("/api/contacts/import", csrf, form);
  },
  /**
   * setMessageCategory files every message from one sender into a category and
   * keeps future mail from that sender there. It is a correction about the
   * sender, so it deliberately reaches beyond the messages that prompted it.
   * Several messages may be named at once — dropping a multi-row selection onto
   * a category does — and the server files each distinct sender behind them.
   */
  setMessageCategory: (csrf: string, messageIDs: number | number[], category: string) =>
    postJSON<{ category: string; sender: string; senders: string[]; moved: number }>("/api/mail/category", csrf, {
      message_ids: Array.isArray(messageIDs) ? messageIDs : [messageIDs],
      category
    }),
  calendars: () => getJSON<{ calendars: CalendarSummary[] }>("/api/calendar/calendars"),
  setCalendarSelected: (csrf: string, id: number, selected: boolean) =>
    putJSON<{ calendar: CalendarSummary }>(`/api/calendar/calendars/${id}`, csrf, { selected }),
  // The range is sent as absolute instants; which calendars are drawn is stored
  // server-side, so the client never has to keep a second copy of that choice.
  calendarEvents: (from: Date, to: Date) =>
    getJSON<{ events: CalendarEvent[] }>(
      `/api/calendar/events?${new URLSearchParams({ from: from.toISOString(), to: to.toISOString() })}`
    ),
  createCalendarEvent: (csrf: string, input: CalendarEventInput) =>
    postJSON<{ event: CalendarEvent }>("/api/calendar/events", csrf, input),
  updateCalendarEvent: (csrf: string, id: number, input: CalendarEventInput) =>
    putJSON<{ event: CalendarEvent }>(`/api/calendar/events/${id}`, csrf, input),
  deleteCalendarEvent: (csrf: string, id: number) =>
    deleteJSON<{ ok: boolean }>(`/api/calendar/events/${id}`, csrf),
  // Sync-now for one connected account. The calendar view offers it because a
  // week that looks empty is the moment a user wants to force a refresh, and
  // the settings page is two navigations away.
  syncGoogleCalendar: (csrf: string, connectionID: number) =>
    postJSON<{ calendars: number; created: number; updated: number; deleted: number }>(
      `/api/google/connections/${connectionID}/calendar/sync`,
      csrf
    ),
  respondToCalendarEvent: (csrf: string, id: number, response: string) =>
    postJSON<{ event: CalendarEvent }>(`/api/calendar/events/${id}/respond`, csrf, { response }),
  addSenderContact: (csrf: string, id: number) =>
    postJSON<{ contact: Contact; created: boolean }>(`/api/messages/${id}/contacts/add-sender`, csrf),
  syncStatus: () => getJSON<{ running: boolean; latest: SyncRun | null }>("/api/sync/status"),
  account: () =>
    getJSON<{
      imap_accounts: Account[];
      smtp_accounts: SMTPAccount[];
      identities: MailIdentity[];
      me_contacts: Contact[];
      sync_runs: SyncRun[];
      sync_folders: SyncFolder[];
      storage?: StorageStats;
      notice: string;
      account_needs_password?: boolean;
    }>("/api/account"),
  folderProgress: () => getJSON<{ folders: FolderProgress[] }>("/api/account/folders/progress"),
  storage: () => getJSON<StorageStats>("/api/storage"),
  // Rebuilds the signed-in user's own search index. It takes no user id: this
  // is the reader acting on their own index, and acting on somebody else's is
  // the admin route.
  rebuildOwnSearchIndex: (csrf: string) =>
    postJSON<{
      ok: boolean;
      started_runs: number;
      busy_accounts: number;
      blocked?: SearchRebuildBlock[];
      storage: StorageStats;
    }>("/api/storage/search-index/rebuild", csrf),
  plugins: () => getJSON<{ enabled: string[] }>("/api/plugins"),
  saveProfile: (csrf: string, profile: {
    backup_email: string;
    date_locale: string;
    date_format: string;
    theme: string;
    search_preset: string;
    search_recency_bias: string;
    search_fuzzy: string;
    search_sender_boost: boolean;
    search_sender_history: string;
    search_contact_boost: string;
    search_attachment_weight: string;
    search_compact_splitting: boolean;
  }) =>
    postJSON<{ user: User }>("/api/profile", csrf, profile),
  saveSwipePreferences: (csrf: string, preferences: SwipePreferences) =>
    postJSON<{ swipe_preferences: SwipePreferences }>("/api/profile/swipes", csrf, preferences),
  requestPasswordReset: (csrf: string, email: string) =>
    postJSON<{ ok: boolean }>("/api/password-reset/request", csrf, { email }),
  completePasswordReset: (csrf: string, token: string, password: string) =>
    postJSON<{ ok: boolean }>("/api/password-reset/complete", csrf, { token, password }),
  saveIMAPAccount: (csrf: string, account: Record<string, unknown>) =>
    postJSON<{ ok: boolean; account: Account }>("/api/account/imap", csrf, account),
  imapAccountPurgeEstimate: (id: number) =>
    getJSON<AccountPurgeEstimate>(`/api/account/imap/${id}/purge-estimate`),
  deleteIMAPAccount: (csrf: string, id: number, confirm: string) =>
    postJSON<{ ok: boolean; queued: boolean; run_id: number; estimate: AccountPurgeEstimate }>(`/api/account/imap/${id}/delete`, csrf, { confirm }),
  createIMAPFolder: (csrf: string, accountID: number, name: string) =>
    postJSON<{ ok: boolean; mailbox: Mailbox }>(`/api/account/imap/${accountID}/folders`, csrf, { name }),
  saveSMTPAccount: (csrf: string, account: Record<string, unknown>) =>
    postJSON<{ ok: boolean; smtp_account: SMTPAccount }>("/api/account/smtp", csrf, account),
  deleteSMTPAccount: (csrf: string, id: number) =>
    deleteJSON<{ ok: boolean }>(`/api/account/smtp/${id}`, csrf),
  saveMailIdentity: (csrf: string, identity: Record<string, unknown>) =>
    postJSON<{ ok: boolean; identity: MailIdentity; identities: MailIdentity[] }>("/api/account/identities", csrf, identity),
  deleteMailIdentity: (csrf: string, id: number) =>
    deleteJSON<{ ok: boolean; identities: MailIdentity[] }>(`/api/account/identities/${id}`, csrf),
  syncAccount: (csrf: string) => postJSON<{ ok: boolean }>("/api/account/sync", csrf),
  rebuildIMAPAccountSearchIndex: (csrf: string, id: number) =>
    postJSON<{ ok: boolean; queued: boolean; run_id: number }>(`/api/account/imap/${id}/rebuild-search-index`, csrf),
  setFolderMode: (csrf: string, id: number, syncMode: string) =>
    postJSON<{ ok: boolean }>(`/api/account/folders/${id}/mode`, csrf, { sync_mode: syncMode }),
  saveFolderSettings: (csrf: string, id: number, settings: Record<string, unknown>) =>
    postJSON<{ ok: boolean; queued?: boolean; run_id?: number }>(`/api/account/folders/${id}/settings`, csrf, settings),
  syncFolder: (csrf: string, id: number) => postJSON<{ ok: boolean }>(`/api/account/folders/${id}/sync`, csrf),
  rebuildFolderSearchIndex: (csrf: string, id: number) =>
    postJSON<{ ok: boolean; queued: boolean; run_id: number }>(`/api/account/folders/${id}/search-index/rebuild`, csrf),
  purgeFolderSearchIndex: (csrf: string, id: number) =>
    postJSON<{ ok: boolean; queued: boolean; run_id: number }>(`/api/account/folders/${id}/search-index/purge`, csrf),
  purgeFolderLocalReferences: (csrf: string, id: number) =>
    postJSON<{ ok: boolean; queued: boolean; run_id: number }>(`/api/account/folders/${id}/local-references/purge`, csrf),
  // The SMTP tail is read while a send is failing, so it answers without an
  // ETag and every call reaches the server: a revalidated answer would show
  // the attempt before the one the reader just made.
  smtpLog: (accountID = 0, limit = 10) =>
    getJSON<{ sessions: SMTPLogSession[] }>(
      `/api/smtp-log?limit=${limit}${accountID ? `&account_id=${accountID}` : ""}`),
  testSMTPAccount: (csrf: string, id: number) =>
    postJSON<SMTPTestResult>(`/api/account/smtp/${id}/test`, csrf),
  duplicateCopies: () => getJSON<DuplicateCopyReport>("/api/account/duplicates"),
  rescanDuplicateCopies: (csrf: string, after = "") =>
    postJSON<DuplicateScanResult>("/api/account/duplicates/rescan", csrf, { after }),
  trashDuplicateCopies: (csrf: string) =>
    postJSON<{ ok: boolean; queued: boolean; matched: number; skipped: number; queued_messages?: number; truncated: boolean; partial_error?: string }>(
      "/api/account/duplicates/trash", csrf),
  users: () => getJSON<{ users: User[]; password_reset_from_address?: string }>("/api/admin/users"),
  createUser: (csrf: string, body: { email: string; name: string; password: string; is_admin: boolean }) =>
    postJSON<{ ok: boolean }>("/api/admin/users", csrf, body),
  setUserPassword: (csrf: string, id: number, password: string) =>
    postJSON<{ ok: boolean }>(`/api/admin/users/${id}/password`, csrf, { password }),
  deleteUser: (csrf: string, id: number) =>
    deleteJSON<{ ok: boolean }>(`/api/admin/users/${id}`, csrf),
  database: () => getJSON<DatabaseOverview>("/api/admin/database"),
  searchIndex: () => getJSON<SearchIndexReport>("/api/admin/search-index"),
  rebuildSearchIndex: (csrf: string, userID: number) =>
    postJSON<SearchIndexReport>("/api/admin/search-index", csrf, { user_id: userID }),
  // The tail answers without an ETag, so every call reaches the server: a
  // revalidated answer is the one thing this endpoint must never give.
  serverLog: (limit = 200) => getJSON<{ lines: ServerLogLine[] }>(`/api/admin/log?limit=${limit}`),
  savePasswordResetSettings: (csrf: string, fromAddress: string) =>
    postJSON<{ ok: boolean; from_address: string }>("/api/admin/password-reset", csrf, { from_address: fromAddress }),
  adminPlugins: () => getJSON<{ plugins: PluginSetting[] }>("/api/admin/plugins"),
  setAdminPlugin: (csrf: string, id: string, enabled: boolean) =>
    postJSON<{ ok: boolean; plugins: PluginSetting[] }>(`/api/admin/plugins/${encodeURIComponent(id)}`, csrf, { enabled }),
  remoteImageBlocklist: () => getJSON<{ patterns: string[] }>("/api/admin/remote-image-blocklist"),
  saveRemoteImageBlocklist: (csrf: string, patterns: string[]) =>
    postJSON<{ ok: boolean; patterns: string[] }>("/api/admin/remote-image-blocklist", csrf, { patterns }),
  syncRun: (id: string) => getJSON<{ sync_run: SyncRun; live: SyncRunLiveDetail }>(`/api/sync-runs/${id}`),
  cancelSyncRun: (csrf: string, id: number) => postJSON<{ ok: boolean }>(`/api/sync-runs/${id}/cancel`, csrf),
  deleteSyncRun: (csrf: string, id: number) => deleteJSON<{ ok: boolean }>(`/api/sync-runs/${id}`, csrf),
  activity: () => getJSON<Activity>("/api/activity"),
  // The reservation key embeds a raw IMAP mailbox name, which may contain any
  // separator a URL scheme could pick, so it travels in the body.
  cancelWorker: (csrf: string, key: string) =>
    postJSON<{ ok: boolean }>("/api/activity/workers/cancel", csrf, { key }),
  clearSyncHistory: (csrf: string) => deleteJSON<{ ok: boolean; removed: number }>("/api/activity/history", csrf)
};
