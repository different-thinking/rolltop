// File overview: Bounded, user-scoped persistence for what the reader may open
// while the server cannot be reached - opened and warmed message threads, and
// the calendar weeks around the one they were looking at.
//
// The mail list pages already have a home in localStorage (`mailSnapshot.ts`);
// message bodies and calendar windows are too large for that quota, so they
// live in IndexedDB. Four properties keep the store honest and must survive
// any change to it.
//
//   - Every entry is keyed by the user it belongs to, read back only for that
//     user, and the whole user's set is dropped on sign-out and when another
//     user signs in (`clearOfflineUser`, `clearOtherOfflineUsers`). IDs are
//     only unique per installation, so a message id alone would hand the next
//     reader on a shared browser the previous one's mail.
//   - The set is bounded per user and per kind, in entries and in bytes, and
//     the oldest entries go first. Mail sizes span four orders of magnitude, so
//     a count alone would let one newsletter decide how much of the quota a
//     user holds.
//   - Metadata and payloads are two object stores. Eviction reads only the
//     metadata; reading every stored thread to find out which one is oldest
//     would pull the whole store through memory on every write.
//   - A browser without IndexedDB, or one that refuses to open it, falls back to
//     a memory map for the tab's lifetime, so nothing that writes here has to
//     care whether the copy outlives the tab. It is a convenience store: a miss
//     is answered by the network, never by an error.

type OfflineKind = "message" | "calendars" | "calendar-events" | "calendar-outbox";

export type OfflineEntryMeta = {
  key: string;
  user_id: number;
  kind: OfflineKind;
  name: string;
  saved_at: number;
  bytes: number;
};

type OfflineEntry<T = unknown> = { meta: OfflineEntryMeta; data: T };

type OfflineBackend = {
  get(key: string): Promise<OfflineEntry | null>;
  put(entry: OfflineEntry): Promise<void>;
  delete(keys: string[]): Promise<void>;
  list(userID: number): Promise<OfflineEntryMeta[]>;
  listAll(): Promise<OfflineEntryMeta[]>;
};

const databaseName = "rolltop-offline";
const databaseVersion = 1;
const metaStore = "meta";
const dataStore = "data";

/** Per-user, per-kind ceilings. A message thread with inline parts is a few
 * hundred kilobytes; the message budget holds a week or two of ordinary mail. */
const limits: Record<OfflineKind, { entries: number; bytes: number }> = {
  message: { entries: 200, bytes: 32 * 1024 * 1024 },
  calendars: { entries: 1, bytes: 1024 * 1024 },
  "calendar-events": { entries: 12, bytes: 8 * 1024 * 1024 },
  "calendar-outbox": { entries: 1, bytes: 2 * 1024 * 1024 }
};

/** Kinds a sign-out keeps. The calendar outbox is what the reader typed and
 * has not reached the server yet; signing out must not forfeit it, and only
 * another user signing in on this browser drops it (`clearOtherOfflineUsers`). */
const keptThroughSignOut = new Set<OfflineKind>(["calendar-outbox"]);

/** A single entry larger than this is not stored at all: it would evict most
 * of the set to make room for one thread. */
const maxEntryBytes = 6 * 1024 * 1024;

function entryKey(userID: number, kind: OfflineKind, name: string): string {
  return `${userID}:${kind}:${name}`;
}

function positiveInteger(value: unknown): value is number {
  return typeof value === "number" && Number.isInteger(value) && value > 0;
}

// --- Memory backend -------------------------------------------------------

function memoryBackend(): OfflineBackend {
  const entries = new Map<string, OfflineEntry>();
  return {
    async get(key) {
      return entries.get(key) || null;
    },
    async put(entry) {
      entries.set(entry.meta.key, entry);
    },
    async delete(keys) {
      keys.forEach((key) => entries.delete(key));
    },
    async list(userID) {
      return Array.from(entries.values()).map((entry) => entry.meta).filter((meta) => meta.user_id === userID);
    },
    async listAll() {
      return Array.from(entries.values()).map((entry) => entry.meta);
    }
  };
}

// --- IndexedDB backend ----------------------------------------------------

function requestToPromise<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error || new Error("IndexedDB request failed"));
  });
}

function transactionDone(transaction: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    transaction.oncomplete = () => resolve();
    transaction.onabort = () => reject(transaction.error || new Error("IndexedDB transaction aborted"));
    transaction.onerror = () => reject(transaction.error || new Error("IndexedDB transaction failed"));
  });
}

/** OfflineStoreUnavailable is IndexedDB refusing to open at all, as opposed to
 * one read or write failing inside a database that works. */
class OfflineStoreUnavailable extends Error {}

function openDatabase(onClose: () => void): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(databaseName, databaseVersion);
    request.onupgradeneeded = () => {
      const db = request.result;
      if (!db.objectStoreNames.contains(metaStore)) {
        const meta = db.createObjectStore(metaStore, { keyPath: "key" });
        meta.createIndex("user_id", "user_id", { unique: false });
      }
      if (!db.objectStoreNames.contains(dataStore)) db.createObjectStore(dataStore);
    };
    request.onsuccess = () => {
      const db = request.result;
      // Another tab upgrading the schema asks this connection to step aside,
      // and the browser may close it on its own (storage evicted, the profile
      // locked). Either way the cached handle goes with it, so the next call
      // opens a fresh one instead of failing on a closed connection.
      db.onversionchange = () => {
        db.close();
        onClose();
      };
      db.onclose = onClose;
      resolve(db);
    };
    request.onerror = () => reject(new OfflineStoreUnavailable(request.error?.message || "IndexedDB open failed"));
    request.onblocked = () => reject(new OfflineStoreUnavailable("IndexedDB open blocked"));
  });
}

function validMeta(value: unknown): value is OfflineEntryMeta {
  if (!value || typeof value !== "object") return false;
  const meta = value as Record<string, unknown>;
  return typeof meta.key === "string" && positiveInteger(meta.user_id) && typeof meta.kind === "string" &&
    typeof meta.name === "string" && typeof meta.saved_at === "number" && typeof meta.bytes === "number";
}

function indexedDBBackend(): OfflineBackend {
  let database: Promise<IDBDatabase> | null = null;
  function db(): Promise<IDBDatabase> {
    if (!database) {
      const opened: Promise<IDBDatabase> = openDatabase(() => {
        if (database === opened) database = null;
      }).catch((err) => {
        database = null;
        throw err;
      });
      database = opened;
    }
    return database;
  }
  return {
    async get(key) {
      const connection = await db();
      const transaction = connection.transaction([metaStore, dataStore], "readonly");
      const [meta, data] = await Promise.all([
        requestToPromise(transaction.objectStore(metaStore).get(key)),
        requestToPromise(transaction.objectStore(dataStore).get(key))
      ]);
      if (!validMeta(meta) || data === undefined) return null;
      return { meta, data };
    },
    async put(entry) {
      const connection = await db();
      const transaction = connection.transaction([metaStore, dataStore], "readwrite");
      transaction.objectStore(metaStore).put(entry.meta);
      transaction.objectStore(dataStore).put(entry.data, entry.meta.key);
      await transactionDone(transaction);
    },
    async delete(keys) {
      if (keys.length === 0) return;
      const connection = await db();
      const transaction = connection.transaction([metaStore, dataStore], "readwrite");
      keys.forEach((key) => {
        transaction.objectStore(metaStore).delete(key);
        transaction.objectStore(dataStore).delete(key);
      });
      await transactionDone(transaction);
    },
    async list(userID) {
      const connection = await db();
      const transaction = connection.transaction(metaStore, "readonly");
      const rows = await requestToPromise(transaction.objectStore(metaStore).index("user_id").getAll(userID));
      return (rows as unknown[]).filter(validMeta);
    },
    async listAll() {
      const connection = await db();
      const transaction = connection.transaction(metaStore, "readonly");
      const rows = await requestToPromise(transaction.objectStore(metaStore).getAll());
      return (rows as unknown[]).filter(validMeta);
    }
  };
}

// --- Backend selection ----------------------------------------------------

let backend: OfflineBackend | null = null;
let backendOverride: OfflineBackend | null = null;

function currentBackend(): OfflineBackend {
  if (backendOverride) return backendOverride;
  if (!backend) {
    backend = typeof indexedDB !== "undefined" ? indexedDBBackend() : memoryBackend();
  }
  return backend;
}

// An IndexedDB that will not open - private mode in some browsers, a
// locked-down profile - degrades to memory for this tab rather than failing
// every save and read. The switch happens once and is one-way for the tab; the
// next tab tries IndexedDB again. Only a refused open switches. A single read
// or write failing inside a database that works - a quota hit on one large
// thread is the ordinary case - answers with the fallback for that call alone:
// switching on it would make every copy already stored unreadable for the
// tab, and would let the calendar outbox in memory drift from the one on disk,
// which the next page load would read back and send again.
async function withBackend<T>(run: (store: OfflineBackend) => Promise<T>, fallback: T): Promise<T> {
  const store = currentBackend();
  try {
    return await run(store);
  } catch (err) {
    if (err instanceof OfflineStoreUnavailable && !backendOverride && store === backend) {
      backend = memoryBackend();
      try {
        return await run(backend);
      } catch {
        return fallback;
      }
    }
    return fallback;
  }
}

/** useMemoryOfflineStoreForTests swaps the backend for a fresh in-memory one
 * and returns a function that restores the default. Tests only. */
export function useMemoryOfflineStoreForTests(): () => void {
  backendOverride = memoryBackend();
  return () => {
    backendOverride = null;
  };
}

// --- Public API -----------------------------------------------------------

function sizeOf(data: unknown): number {
  try {
    return JSON.stringify(data).length;
  } catch {
    return maxEntryBytes + 1;
  }
}

/**
 * saveOfflineEntry stores one payload for a user and trims that user's set of
 * the same kind back under its limits, oldest first. It never throws: a copy
 * that could not be kept is a copy the network answers for next time.
 */
export async function saveOfflineEntry(userID: number, kind: OfflineKind, name: string, data: unknown): Promise<boolean> {
  if (!positiveInteger(userID) || !name) return false;
  const bytes = sizeOf(data);
  if (bytes > maxEntryBytes) return false;
  const key = entryKey(userID, kind, name);
  const meta: OfflineEntryMeta = { key, user_id: userID, kind, name, saved_at: Date.now(), bytes };
  return withBackend(async (store) => {
    await store.put({ meta, data });
    const siblings = (await store.list(userID)).filter((entry) => entry.kind === kind);
    await store.delete(evictionKeys(siblings, limits[kind], key));
    return true;
  }, false);
}

/**
 * evictionKeys names the entries to drop so the set fits its limits. The entry
 * just written is kept whatever its age; after it, the newest entries are kept
 * until either limit is reached and everything older goes.
 */
export function evictionKeys(entries: OfflineEntryMeta[], limit: { entries: number; bytes: number }, keep: string): string[] {
  const sorted = [...entries].sort((left, right) => {
    if (left.key === keep) return -1;
    if (right.key === keep) return 1;
    return right.saved_at - left.saved_at;
  });
  const evicted: string[] = [];
  let retainedBytes = 0;
  sorted.forEach((entry, index) => {
    retainedBytes += entry.bytes;
    if (entry.key === keep) return;
    if (index >= limit.entries || retainedBytes > limit.bytes) evicted.push(entry.key);
  });
  return evicted;
}

export type OfflineHit<T> = { data: T; saved_at: number };

/** loadOfflineEntry reads one payload back, or null when the user never stored
 * it, or it has since been evicted. */
export async function loadOfflineEntry<T>(userID: number, kind: OfflineKind, name: string): Promise<OfflineHit<T> | null> {
  if (!positiveInteger(userID) || !name) return null;
  return withBackend(async (store) => {
    const entry = await store.get(entryKey(userID, kind, name));
    if (!entry || entry.meta.user_id !== userID || entry.meta.kind !== kind) return null;
    return { data: entry.data as T, saved_at: entry.meta.saved_at };
  }, null);
}

/** listOfflineEntries reports what one user holds of one kind, without the
 * payloads. */
export async function listOfflineEntries(userID: number, kind: OfflineKind): Promise<OfflineEntryMeta[]> {
  if (!positiveInteger(userID)) return [];
  return withBackend(async (store) => (await store.list(userID)).filter((entry) => entry.kind === kind), []);
}

/** deleteOfflineEntries drops named entries of one kind for one user. */
export async function deleteOfflineEntries(userID: number, kind: OfflineKind, names: string[]): Promise<void> {
  if (!positiveInteger(userID) || names.length === 0) return;
  await withBackend(async (store) => store.delete(names.map((name) => entryKey(userID, kind, name))), undefined);
}

/** clearOfflineUser drops what one user holds - a sign-out - except what is
 * theirs to send later (`keptThroughSignOut`). */
export async function clearOfflineUser(userID: number): Promise<void> {
  if (!positiveInteger(userID)) return;
  await withBackend(async (store) => {
    const entries = await store.list(userID);
    await store.delete(entries.filter((entry) => !keptThroughSignOut.has(entry.kind)).map((entry) => entry.key));
  }, undefined);
}

/** clearOtherOfflineUsers drops every other user's entries - a sign-in on a
 * browser somebody else used. */
export async function clearOtherOfflineUsers(keepUserID: number): Promise<void> {
  await withBackend(async (store) => {
    const entries = await store.listAll();
    await store.delete(entries.filter((entry) => entry.user_id !== keepUserID).map((entry) => entry.key));
  }, undefined);
}
