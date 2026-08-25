// File overview: what the reader filed out of a folder - deleted, reported as
// spam, archived - remembered beyond the list they clicked in.
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
// every list before it draws. Three properties keep that from hiding real mail.
//
//   - A record names the folder the message was filed *into*, and hides the row
//     only while it still claims some other folder. Deleted mail is therefore
//     hidden everywhere except the Trash it was moved to, and stops being hidden
//     the moment any list reports it has arrived - no confirmation needed, and
//     nothing to clear.
//   - Records expire (`filedMessageTTLMS`). A filing whose move never happened
//     is a row the reader has to get back eventually, whatever went wrong in the
//     background and whatever this browser was told about it.
//   - The set is bounded (`filedMessageLimit`); the oldest records go first.
//
// A destination of 0 means the message is gone rather than moved - the server
// answered for it with "no such message" - and hides it in every list.

import type { Conversation } from "../types";

const storageVersion = 1;
const storagePrefix = `rolltop.mail.filed.v${storageVersion}.`;
/** How long a filing hides a row that never reports arriving anywhere. */
export const filedMessageTTLMS = 24 * 60 * 60 * 1000;
const filedMessageLimit = 2000;

/** FiledMessage is one message and the folder it was filed into (0: it is gone). */
export type FiledMessage = { id: number; toMailboxID: number };

type StoredRecord = { id: number; to: number; at: number };

let activeUserID = 0;
let records = new Map<number, StoredRecord>();
const listeners = new Set<() => void>();

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
    typeof record.to === "number" && Number.isInteger(record.to) && record.to >= 0 &&
    typeof record.at === "number" && Number.isFinite(record.at) && record.at > 0;
}

function load(userID: number): Map<number, StoredRecord> {
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
  return prune(loaded);
}

function persist() {
  if (activeUserID <= 0) return;
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

/**
 * setFiledMessagesUser points this module at the signed-in user. Called from the
 * one place that already decides which user's mail caches are the live ones, so
 * a second account never reads the first one's filings.
 */
export function setFiledMessagesUser(userID: number) {
  const next = Number.isInteger(userID) && userID > 0 ? userID : 0;
  if (next === activeUserID) return;
  activeUserID = next;
  records = next > 0 ? load(next) : new Map();
  notify();
}

/** clearFiledMessages forgets one user's filings, on logout or a user switch. */
export function clearFiledMessages(userID: number) {
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

/** clearOtherFiledMessages drops every user's filings but the signed-in one's. */
export function clearOtherFiledMessages(keepUserID: number) {
  try {
    const keep = storageKey(keepUserID);
    for (let index = localStorage.length - 1; index >= 0; index -= 1) {
      const key = localStorage.key(index);
      if (key && key.startsWith(storagePrefix) && key !== keep) localStorage.removeItem(key);
    }
  } catch {
    // Best-effort cleanup for storage-restricted browsers.
  }
}

/**
 * recordFiledMessages files messages away and returns the ids it took. A
 * destination of 0 records a message the server no longer has.
 */
export function recordFiledMessages(entries: FiledMessage[]): number[] {
  const at = Date.now();
  const taken: number[] = [];
  entries.forEach((entry) => {
    if (!Number.isInteger(entry.id) || entry.id <= 0) return;
    const to = Number.isInteger(entry.toMailboxID) && entry.toMailboxID > 0 ? entry.toMailboxID : 0;
    records.set(entry.id, { id: entry.id, to, at });
    taken.push(entry.id);
  });
  if (taken.length === 0) return taken;
  records = prune(records);
  persist();
  notify();
  return taken;
}

/** releaseFiledMessages puts messages back on screen: an undo, or a move that was never attempted. */
export function releaseFiledMessages(ids: number[]) {
  let changed = false;
  ids.forEach((id) => {
    if (records.delete(id)) changed = true;
  });
  if (!changed) return;
  persist();
  notify();
}

/** filedMessageIDs lists what is currently filed, for callers purging cached pages. */
export function filedMessageIDs(): number[] {
  records = prune(records);
  return Array.from(records.keys());
}

/**
 * messageIsFiled reports whether a row should stay off screen: it was filed, and
 * it still claims a folder other than the one it was filed into.
 */
export function messageIsFiled(messageID: number, mailboxID: number): boolean {
  const record = records.get(messageID);
  if (!record) return false;
  if (record.at <= Date.now() - filedMessageTTLMS) return false;
  return record.to !== mailboxID;
}

/**
 * filterFiledConversations drops the rows a filing is still hiding. A row stands
 * for its own seed message, which is the message the filing named, so that is
 * what decides it - the same message the list's own dismissal keys on.
 */
export function filterFiledConversations<T extends Conversation>(conversations: T[]): T[] {
  if (records.size === 0) return conversations;
  return conversations.filter((conversation) => !messageIsFiled(conversation.message.id, conversation.message.mailbox_id));
}
