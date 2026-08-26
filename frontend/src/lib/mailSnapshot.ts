// Bounded, user-scoped persistence for recently visited mail list pages. A
// snapshot paints only after the embedded bootstrap confirms the same user.

import type { Conversation, MailListResponse, Message } from "../types";
import { defaultMailSortOrder, mailSortOrder } from "./mailSort";
import type { MailSortOrder } from "./mailSort";

const snapshotVersion = 3;
const maxSnapshotBytes = 900_000;
const maxStoredSnapshotBytes = 2_400_000;
const maxStoredSnapshots = 6;
const maxSnapshotConversations = 200;
const snapshotPrefix = `rolltop.mail.list.v${snapshotVersion}.`;
// Snapshot formats this build can no longer read. A row's `message_account_ids`
// changed meaning in v3: it now names the account of each of the row's messages
// rather than the distinct set of accounts they are spread over. A v2 page
// cannot be told apart from a v3 one field by field, and read as v3 it answers
// "which account is this message in" wrongly - so the version is what rejects
// it, and these keys are dropped rather than left sitting in storage.
const supersededSnapshotPrefixes = ["rolltop.mail.list.v2."];
const legacyAllMailPrefix = "rolltop.mail.all.v1.";

type MailSnapshot = {
  version: number;
  user_id: number;
  mailbox_id: number | null;
  page_number: number;
  // Snapshots written before date sorting existed carry no order and are read
  // back as the newest-first default they were captured in.
  order?: MailSortOrder;
  saved_at: number;
  page: MailListResponse;
};

export function mailSnapshotStorageKey(userID: number, mailboxID: string | null, page: number, order: MailSortOrder = defaultMailSortOrder): string {
  const mailbox = normalizedMailboxID(mailboxID);
  const suffix = order === defaultMailSortOrder ? "" : `.${order}`;
  return `${snapshotPrefix}${userID}.${mailbox === null ? "all" : `box-${mailbox}`}.p${page}${suffix}`;
}

export function loadMailSnapshot(userID: number, mailboxID: string | null, page: number, order: MailSortOrder = defaultMailSortOrder): MailListResponse | null {
  const mailbox = normalizedMailboxID(mailboxID);
  if (!positiveInteger(userID) || !positiveInteger(page) || (mailboxID !== null && mailbox === null)) return null;
  try {
    const parsed = JSON.parse(localStorage.getItem(mailSnapshotStorageKey(userID, mailboxID, page, order)) || "null") as unknown;
    if (validSnapshot(parsed, userID, mailbox, page, order)) return parsed.page;
  } catch {
    // A corrupt current entry should not prevent migration of a valid v1 page.
  }
  if (mailbox === null && page === 1 && order === defaultMailSortOrder) {
    try {
      const legacy = JSON.parse(localStorage.getItem(`${legacyAllMailPrefix}${userID}`) || "null") as unknown;
      if (validLegacyAllMailSnapshot(legacy, userID)) {
        saveMailSnapshot(userID, null, 1, legacy.page);
        localStorage.removeItem(`${legacyAllMailPrefix}${userID}`);
        return legacy.page;
      }
    } catch {
      return null;
    }
  }
  return null;
}

function validLegacyAllMailSnapshot(value: unknown, userID: number): value is { version: 1; user_id: number; saved_at: number; page: MailListResponse } {
  if (!record(value)) return false;
  return value.version === 1 && value.user_id === userID &&
    typeof value.saved_at === "number" && Number.isFinite(value.saved_at) && value.saved_at > 0 &&
    validMailPage(value.page) && value.page.page === 1 && value.page.has_prev === false;
}

export function saveMailSnapshot(userID: number, mailboxID: string | null, pageNumber: number, page: MailListResponse, order: MailSortOrder = defaultMailSortOrder): boolean {
  const mailbox = normalizedMailboxID(mailboxID);
  if (!positiveInteger(userID) || !positiveInteger(pageNumber) || (mailboxID !== null && mailbox === null) ||
    page.page !== pageNumber || !validMailPage(page)) return false;
  const snapshot: MailSnapshot = {
    version: snapshotVersion,
    user_id: userID,
    mailbox_id: mailbox,
    page_number: pageNumber,
    order,
    saved_at: Date.now(),
    page
  };
  try {
    const serialized = JSON.stringify(snapshot);
    if (new TextEncoder().encode(serialized).byteLength > maxSnapshotBytes) return false;
    const key = mailSnapshotStorageKey(userID, mailboxID, pageNumber, order);
    // Before the write, not after it: the quota recovery below evicts this
    // user's own pages under the current prefix and cannot reach a superseded
    // one, so pages left by an older format would hold the quota against every
    // write that follows - the save fails, the prune that would have cleared
    // them never runs, and nothing is ever written again.
    dropSupersededSnapshots();
    if (!storeWithQuotaRecovery(key, serialized, userID, order)) return false;
    pruneMailSnapshots(userID, order);
    return true;
  } catch {
    return false;
  }
}

export function clearMailSnapshots(userID: number) {
  if (!positiveInteger(userID)) return;
  dropSupersededSnapshots();
  try {
    const currentPrefix = `${snapshotPrefix}${userID}.`;
    const legacyKey = `${legacyAllMailPrefix}${userID}`;
    storageKeys().filter((key) => key.startsWith(currentPrefix) || key === legacyKey)
      .forEach((key) => localStorage.removeItem(key));
  } catch {
    // Storage may be unavailable in private or locked-down browser contexts.
  }
}

export function clearOtherMailSnapshots(keepUserID: number) {
  dropSupersededSnapshots();
  try {
    const keepPrefix = `${snapshotPrefix}${keepUserID}.`;
    const keepLegacy = `${legacyAllMailPrefix}${keepUserID}`;
    storageKeys().filter((key) =>
      (key.startsWith(snapshotPrefix) && !key.startsWith(keepPrefix)) ||
      (key.startsWith(legacyAllMailPrefix) && key !== keepLegacy)
    ).forEach((key) => localStorage.removeItem(key));
  } catch {
    // Best-effort privacy cleanup for storage-restricted browsers.
  }
}

function validSnapshot(value: unknown, userID: number, mailboxID: number | null, page: number, order: MailSortOrder): value is MailSnapshot {
  if (!record(value)) return false;
  return value.version === snapshotVersion && value.user_id === userID &&
    value.mailbox_id === mailboxID && value.page_number === page &&
    mailSortOrder(value.order) === order &&
    typeof value.saved_at === "number" && Number.isFinite(value.saved_at) && value.saved_at > 0 &&
    validMailPage(value.page) && value.page.page === page;
}

function validMailPage(value: unknown): value is MailListResponse {
  if (!record(value) || !positiveInteger(value.page) || typeof value.has_prev !== "boolean" || typeof value.has_next !== "boolean") return false;
  if (!Array.isArray(value.conversations) || value.conversations.length > maxSnapshotConversations) return false;
  return value.conversations.every(validConversation);
}

// Every write comes from the list the reader is looking at, so its order names
// the All Mail page 1 that will paint on the next start. That is the one page
// worth keeping; the same page in the direction they turned off is not.
function storeWithQuotaRecovery(key: string, serialized: string, userID: number, order: MailSortOrder): boolean {
  try {
    localStorage.setItem(key, serialized);
    return true;
  } catch {
    const pinned = mailSnapshotStorageKey(userID, null, 1, order);
    const candidates = snapshotEntries(userID).filter((entry) => entry.key !== key).sort((left, right) => {
      if (left.key === pinned) return 1;
      if (right.key === pinned) return -1;
      return left.savedAt - right.savedAt;
    });
    for (const candidate of candidates) {
      localStorage.removeItem(candidate.key);
      try {
        localStorage.setItem(key, serialized);
        return true;
      } catch {
        // Continue evicting this user's oldest snapshots until the write fits.
      }
    }
    return false;
  }
}

// dropSupersededSnapshots clears pages written in a format this build cannot
// read. They are dead weight against the storage budget every write and prune
// spends, and no user's copy is worth keeping, so every one of them goes.
function dropSupersededSnapshots() {
  try {
    storageKeys().filter((key) => supersededSnapshotPrefixes.some((prefix) => key.startsWith(prefix)))
      .forEach((key) => localStorage.removeItem(key));
  } catch {
    // Storage may be unavailable in private or locked-down browser contexts.
  }
}

function pruneMailSnapshots(userID: number, order: MailSortOrder) {
  const pinned = mailSnapshotStorageKey(userID, null, 1, order);
  const entries = snapshotEntries(userID).sort((left, right) => {
    if (left.key === pinned) return -1;
    if (right.key === pinned) return 1;
    return right.savedAt - left.savedAt;
  });
  let retainedBytes = 0;
  entries.forEach((entry, index) => {
    retainedBytes += entry.bytes;
    if (index >= maxStoredSnapshots || retainedBytes > maxStoredSnapshotBytes) localStorage.removeItem(entry.key);
  });
}

function snapshotEntries(userID: number): Array<{ key: string; savedAt: number; bytes: number }> {
  const prefix = `${snapshotPrefix}${userID}.`;
  return storageKeys().filter((key) => key.startsWith(prefix)).map((key) => {
    const serialized = localStorage.getItem(key) || "";
    let savedAt = 0;
    try {
      const value = JSON.parse(serialized) as { saved_at?: unknown };
      if (typeof value.saved_at === "number" && Number.isFinite(value.saved_at)) savedAt = value.saved_at;
    } catch {
      // Invalid entries sort oldest and are evicted first.
    }
    return { key, savedAt, bytes: new TextEncoder().encode(serialized).byteLength };
  });
}

function storageKeys(): string[] {
  const keys: string[] = [];
  for (let index = 0; index < localStorage.length; index += 1) {
    const key = localStorage.key(index);
    if (key) keys.push(key);
  }
  return keys;
}

function normalizedMailboxID(value: string | null): number | null {
  if (value === null) return null;
  const parsed = Number(value);
  return positiveInteger(parsed) ? parsed : null;
}

function validConversation(value: unknown): value is Conversation {
  if (!record(value) || !validMessage(value.message)) return false;
  return positiveInteger(value.starred_message_id) && typeof value.participants === "string" &&
    typeof value.recipient_participants === "string" && positiveInteger(value.count) &&
    typeof value.is_read === "boolean" && typeof value.has_attachments === "boolean" &&
    typeof value.snippet === "string" && optionalPositiveIntegerArray(value.message_ids) &&
    optionalPositiveIntegerArray(value.message_account_ids) && optionalStringArray(value.attachment_names) &&
    optionalStringArray(value.attachment_matches) && optionalBoolean(value.attachment_content_matched) &&
    optionalStringArray(value.match_terms) && optionalStringArray(value.match_query_terms) &&
    optionalString(value.snoozed_until);
}

function validMessage(value: unknown): value is Message {
  if (!record(value)) return false;
  return positiveInteger(value.id) && positiveInteger(value.account_id) && positiveInteger(value.mailbox_id) &&
    typeof value.subject === "string" && typeof value.from_addr === "string" &&
    typeof value.to_addr === "string" && typeof value.cc_addr === "string" &&
    typeof value.date === "string" && typeof value.date_short === "string" &&
    typeof value.is_read === "boolean" && typeof value.is_starred === "boolean" &&
    typeof value.has_attachments === "boolean" && typeof value.is_encrypted === "boolean" &&
    typeof value.is_signed === "boolean" && typeof value.snippet === "string";
}

function record(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === "object" && !Array.isArray(value);
}

function positiveInteger(value: unknown): boolean {
  return typeof value === "number" && Number.isInteger(value) && value > 0;
}

function optionalPositiveIntegerArray(value: unknown): boolean {
  return value === undefined || (Array.isArray(value) && value.every(positiveInteger));
}

function optionalStringArray(value: unknown): boolean {
  return value === undefined || (Array.isArray(value) && value.every((item) => typeof item === "string"));
}

function optionalBoolean(value: unknown): boolean {
  return value === undefined || typeof value === "boolean";
}

function optionalString(value: unknown): boolean {
  return value === undefined || typeof value === "string";
}

/**
 * forgetMessagesInSnapshots rewrites this user's stored pages without the rows
 * the given messages seed. A snapshot paints before any request answers, so a
 * page still carrying mail the reader deleted is the copy that puts it back on
 * screen - and, once the message is really gone, the copy whose row opens to a
 * message the server cannot find.
 */
export function forgetMessagesInSnapshots(userID: number, messageIDs: number[]) {
  if (!positiveInteger(userID) || messageIDs.length === 0) return;
  const gone = new Set(messageIDs);
  try {
    const prefix = `${snapshotPrefix}${userID}.`;
    storageKeys().filter((key) => key.startsWith(prefix)).forEach((key) => {
      const serialized = localStorage.getItem(key);
      if (!serialized) return;
      let snapshot: MailSnapshot;
      try {
        snapshot = JSON.parse(serialized) as MailSnapshot;
      } catch {
        localStorage.removeItem(key);
        return;
      }
      if (!record(snapshot) || !validMailPage(snapshot.page)) return;
      const kept = snapshot.page.conversations.filter((conversation) => !gone.has(conversation.message.id));
      if (kept.length === snapshot.page.conversations.length) return;
      localStorage.setItem(key, JSON.stringify({ ...snapshot, page: { ...snapshot.page, conversations: kept } }));
    });
  } catch {
    // Storage may be unavailable in private or locked-down browser contexts.
  }
}
