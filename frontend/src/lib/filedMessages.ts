// File overview: what the reader filed out of a folder - deleted, reported as
// spam, archived, dragged somewhere - remembered beyond the list they clicked
// in.
//
// A list's own dismissal (`MessageList`'s `dismissedIDs`) lives exactly as long
// as the component holding it, and it only ever knew about the rows the server
// had already handed that component. Everything else that can put a filed row
// back on screen went straight past it: a cached page painted from
// `localStorage` on the way back from a message, a page prefetched before the
// delete, a remount after a route change, a reload finishing after the move.
// The reader deleted mail and saw it again seconds later, and pressing Delete on
// such a row asked the server to move a message it no longer had - the "could
// not be deleted" that followed.
//
// So a filing is recorded here as well: user-scoped, persisted, and read by
// every list before it draws. Four properties keep that from hiding real mail.
//
//   - A record is **self-releasing**. It names the folder the message left and
//     the folder it is going to, and hides the row only while the row still
//     claims the folder it left. Any list that reports the row anywhere else -
//     arrived in Trash, or put back by the reader, another client, or a filter -
//     drops the record on sight. Without that a record went on hiding a message
//     that had since come back: it was filed out of the Inbox, reached Trash,
//     was dragged back, and the rule "not in Trash means hidden" re-armed.
//   - Records **expire** (`filedMessageTTLMS`), so a filing whose move never
//     happened is a row the reader gets back eventually, whatever went wrong in
//     the background and whatever this browser was told about it.
//   - The set is **bounded** (`filedMessageLimit`); the oldest records go first.
//   - Writes **merge with what is stored**. Two tabs are two copies of this map
//     and a plain overwrite meant the second tab to file anything silently
//     dropped the first tab's filings, so mail one tab deleted was drawn again
//     after a reload. Tabs also follow each other's writes through the browser's
//     `storage` event.
//
// A destination of 0 with no source folder means the message is gone rather
// than moved - the server answered for it with "no such message" - and hides it
// in every list. That is the row that could be neither opened nor deleted.

import type { Conversation } from "../types";

const storageVersion = 1;
const storagePrefix = `rolltop.mail.filed.v${storageVersion}.`;
/** How long a filing hides a row that never reports arriving anywhere. */
export const filedMessageTTLMS = 24 * 60 * 60 * 1000;
const filedMessageLimit = 2000;

/**
 * FiledMessage is one message, the folder it is leaving, and the folder it is
 * going to. `fromMailboxID` is optional because a caller may only know the
 * destination - a drag holds the folder it dropped on, not the row's own -
 * and a record without it falls back to "hidden until it arrives".
 * `toMailboxID: 0` with no source means the message is gone from the server.
 */
export type FiledMessage = { id: number; fromMailboxID?: number; toMailboxID: number };

type StoredRecord = { id: number; from: number; to: number; at: number };

let activeUserID = 0;
let records = new Map<number, StoredRecord>();
const listeners = new Set<() => void>();
let storageWatcher: ((event: StorageEvent) => void) | null = null;

function storageKey(userID: number) {
  return `${storagePrefix}${userID}`;
}

function notify() {
  listeners.forEach((listener) => listener());
}

/** subscribeFiledMessages re-renders a list when what is filed changes. */
export function subscribeFiledMessages(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function validRecord(value: unknown): value is StoredRecord {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const record = value as Record<string, unknown>;
  return typeof record.id === "number" && Number.isInteger(record.id) && record.id > 0 &&
    typeof record.from === "number" && Number.isInteger(record.from) && record.from >= 0 &&
    typeof record.to === "number" && Number.isInteger(record.to) && record.to >= 0 &&
    typeof record.at === "number" && Number.isFinite(record.at) && record.at > 0;
}

function readStored(userID: number): Map<number, StoredRecord> {
  const loaded = new Map<number, StoredRecord>();
  if (!Number.isInteger(userID) || userID <= 0) return loaded;
  try {
    const parsed = JSON.parse(localStorage.getItem(storageKey(userID)) || "null") as unknown;
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return loaded;
    const entries = (parsed as { entries?: unknown }).entries;
    if (!Array.isArray(entries)) return loaded;
    entries.filter(validRecord).forEach((record) => loaded.set(record.id, record));
  } catch {
    // Storage may be unavailable or corrupt; nothing filed is nothing hidden.
  }
  return loaded;
}

/**
 * commit writes this tab's map back, merged with whatever another tab has
 * stored since it was last read. What this tab just filed is already in
 * `records` and wins the merge by being copied over the stored copy; what it
 * just released has to be named, because it is gone from `records` and the
 * stored copy would otherwise put it straight back.
 */
function commit(released: Set<number>) {
  if (activeUserID <= 0) return;
  const merged = readStored(activeUserID);
  records.forEach((record, id) => merged.set(id, record));
  released.forEach((id) => merged.delete(id));
  records = prune(merged);
  try {
    localStorage.setItem(storageKey(activeUserID), JSON.stringify({
      version: storageVersion,
      user_id: activeUserID,
      entries: Array.from(records.values())
    }));
  } catch {
    // A browser that refuses the write still gets the in-memory set for this
    // page, which is what the list the reader is looking at reads.
  }
}

// prune drops what has expired and, past the cap, the oldest of what is left.
function prune(source: Map<number, StoredRecord>): Map<number, StoredRecord> {
  const cutoff = Date.now() - filedMessageTTLMS;
  const live = Array.from(source.values()).filter((record) => record.at > cutoff);
  live.sort((left, right) => right.at - left.at);
  return new Map(live.slice(0, filedMessageLimit).map((record) => [record.id, record]));
}

// watchStorage follows the other tabs of this browser. A filing made in one tab
// is a decision about the same mailbox the others are showing, so they hide the
// row too rather than each holding their own answer until a reload.
function watchStorage() {
  if (storageWatcher || typeof window === "undefined") return;
  storageWatcher = (event: StorageEvent) => {
    if (activeUserID <= 0 || event.key !== storageKey(activeUserID)) return;
    records = prune(readStored(activeUserID));
    notify();
  };
  window.addEventListener("storage", storageWatcher);
}

/**
 * setFiledMessagesUser points this module at the signed-in user. Called from the
 * one place that already decides which user's mail caches are the live ones, so
 * a second account never reads the first one's filings.
 */
export function setFiledMessagesUser(userID: number) {
  const next = Number.isInteger(userID) && userID > 0 ? userID : 0;
  if (next === activeUserID) return;
  activeUserID = next;
  records = next > 0 ? prune(readStored(next)) : new Map();
  if (next > 0) watchStorage();
  notify();
}

/** clearFiledMessages forgets one user's filings, on sign-out. */
export function clearFiledMessages(userID: number) {
  if (!Number.isInteger(userID) || userID <= 0) return;
  try {
    localStorage.removeItem(storageKey(userID));
  } catch {
    // Nothing to do: the in-memory set below is what this page reads.
  }
  if (userID === activeUserID) {
    records = new Map();
    notify();
  }
}

/**
 * clearOtherFiledMessages drops every user's filings but the signed-in one's.
 * A bootstrap that answers with no user at all - the login page, a session that
 * expired - names nobody to keep, and must not be read as "keep nobody": that
 * deleted the filings of every real account on this browser, so mail deleted
 * before the session lapsed came back after signing in again.
 */
export function clearOtherFiledMessages(keepUserID: number) {
  if (!Number.isInteger(keepUserID) || keepUserID <= 0) return;
  try {
    // Collected before anything is removed: the order `localStorage` reports
    // keys in is the browser's business and may shift as entries go, so
    // deleting while walking it by index can skip one.
    const keep = storageKey(keepUserID);
    const dropped: string[] = [];
    for (let index = 0; index < localStorage.length; index += 1) {
      const key = localStorage.key(index);
      if (key && key.startsWith(storagePrefix) && key !== keep) dropped.push(key);
    }
    dropped.forEach((key) => localStorage.removeItem(key));
  } catch {
    // Best-effort cleanup for storage-restricted browsers.
  }
}

/**
 * recordFiledMessages files messages away and returns the ids it took. A
 * destination of 0 with no source folder records a message the server no longer
 * has. Filing a message again replaces its record, so mail moved back out of
 * Trash and then filed somewhere else is judged by the newer decision.
 */
export function recordFiledMessages(entries: FiledMessage[]): number[] {
  const at = Date.now();
  const taken = new Set<number>();
  entries.forEach((entry) => {
    if (!Number.isInteger(entry.id) || entry.id <= 0) return;
    const to = positiveMailboxID(entry.toMailboxID);
    const from = positiveMailboxID(entry.fromMailboxID);
    // A move to the folder the message is already in files nothing: the record
    // would be released by the first list that drew the row anyway.
    if (from > 0 && from === to) return;
    records.set(entry.id, { id: entry.id, from, to, at });
    taken.add(entry.id);
  });
  if (taken.size === 0) return [];
  commit(new Set());
  notify();
  return Array.from(taken);
}

/** releaseFiledMessages puts messages back on screen: an undo, or the reader asking. */
export function releaseFiledMessages(ids: number[]) {
  const released = new Set<number>();
  ids.forEach((id) => {
    if (records.delete(id)) released.add(id);
  });
  if (released.size === 0) return;
  commit(released);
  notify();
}

/** filedMessageIDs lists what is currently filed, for callers purging cached pages. */
export function filedMessageIDs(): number[] {
  records = prune(records);
  return Array.from(records.keys());
}

/**
 * messageIsFiled reports whether a row should stay off screen. A record with no
 * destination is a message the server no longer has, and hides it everywhere;
 * otherwise the row is hidden only while it still claims the folder it was
 * filed out of, and a row seen anywhere else has answered the question the
 * record was asking.
 */
export function messageIsFiled(messageID: number, mailboxID: number): boolean {
  const record = records.get(messageID);
  if (!record) return false;
  if (record.at <= Date.now() - filedMessageTTLMS) return false;
  if (record.to === 0 && record.from === 0) return true;
  if (record.from > 0) return mailboxID === record.from;
  return mailboxID !== record.to;
}

/**
 * filterFiledConversations drops the rows a filing is still hiding. A row stands
 * for its own seed message, which is the message the filing named, so that is
 * what decides it - the same message the list's own dismissal keys on.
 *
 * Pure, and it has to stay pure: a list calls it while rendering, and a render
 * that writes to `localStorage` blocks the main thread and happens again on
 * every render React discards. Letting go of the answered records is the other
 * half and lives in `releaseAnsweredFilings`, which callers reach outside the
 * render.
 */
export function filterFiledConversations<T extends Conversation>(conversations: T[]): T[] {
  if (records.size === 0) return conversations;
  return conversations.filter((conversation) => !messageIsFiled(conversation.message.id, conversation.message.mailbox_id));
}

/**
 * releaseAnsweredFilings lets go of the records these rows have answered: the
 * message arriving where it was filed to, or turning up anywhere other than the
 * folder it was filed out of. That is what makes a record self-clearing, and
 * why nothing has to be cleared when a move lands.
 *
 * It is called where a page of rows is settled rather than drawn - a list
 * response coming back, an effect after the rows are on screen - never during a
 * render. Reading the same rows twice is free: a record is released once and is
 * not there to release again.
 */
export function releaseAnsweredFilings(conversations: readonly Conversation[]) {
  if (records.size === 0) return;
  const answered = new Set<number>();
  conversations.forEach((conversation) => {
    const { id, mailbox_id: mailboxID } = conversation.message;
    if (records.has(id) && !messageIsFiled(id, mailboxID)) answered.add(id);
  });
  if (answered.size === 0) return;
  answered.forEach((id) => records.delete(id));
  commit(answered);
}

function positiveMailboxID(value: number | undefined): number {
  return typeof value === "number" && Number.isInteger(value) && value > 0 ? value : 0;
}
