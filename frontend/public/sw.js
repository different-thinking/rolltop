const STATIC_CACHE = "rolltop-static-v11";
const STATIC_ASSETS = ["/offline.html", "/icon.svg", "/icon.svg?v=transparent-logo-v2"];
// Assets under /assets/ are content-hashed, so a new release never overwrites
// the previous one's entries -- it simply stops requesting them. Unbounded,
// this cache therefore accumulated every asset of every release an install had
// ever seen, a 4.6 MB PDFium among them, until the browser began rejecting
// writes. Those rejections are silent, so from the outside the cache had merely
// stopped working, with no way to tell that from it never having worked.
//
// A count is a crude proxy for the bytes that actually run out, but it is the
// only measure a cache can take cheaply, and what it drops is close enough: the
// list is in insertion order, so the entries at the front are the ones this
// install cached first, which under content hashing means the releases it saw
// earliest. Eviction is first-in rather than least-recently-used -- re-fetching
// an asset was measured not to move it back, put replaces an entry in place --
// so an asset that outlives several releases under one hash can be dropped
// while still in use. That costs one network fetch, served from the HTTP cache
// these assets are marked immutable for. The cap is several builds' worth of a
// build that ships about a dozen.
const MAX_CACHED_ASSETS = 48;
// A failed write halves the cache rather than trimming to a second fixed
// number. A fixed one was measured to do nothing at all: a cache full of large
// assets reaches the quota well under the cap, so the trim it asked for had
// already happened and every later write failed too, leaving the install frozen
// on whichever release filled it first. Halving always makes room. The floor
// stops a quota small enough to reject even one asset from emptying the cache
// on every fetch.
const MIN_CACHED_ASSETS = 4;
let securityUnlockUserID = 0;
let securityUnlockState = { unlockedUntil: 0, keys: [] };
let securityUnlockTimer = 0;

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(STATIC_CACHE).then((cache) => cache.addAll(STATIC_ASSETS)));
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((key) => key !== STATIC_CACHE).map((key) => caches.delete(key)))
    )
  );
  self.clients.claim();
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  const url = new URL(req.url);
  if (req.method !== "GET" || url.origin !== self.location.origin) return;

  if (url.pathname.startsWith("/rolltop-native-share/")) {
    event.respondWith(fetch(req));
    return;
  }

  if (url.pathname.startsWith("/brand-icons/") || url.pathname.startsWith("/plugins/")) return;

  if (url.pathname.startsWith("/api/")) return;

  const acceptsHTML = req.headers.get("Accept")?.includes("text/html");
  if (req.mode === "navigate" || acceptsHTML) {
    const networkRequest = new Request(req, { cache: "no-cache" });
    event.respondWith(
      fetch(networkRequest).catch(() => req.mode === "navigate" ? caches.match("/offline.html") : Promise.reject(new Error("offline")))
    );
    return;
  }

  // Never put authenticated mail, attachment, blob, or image responses in a
  // URL-only cache; user-local IDs can overlap after a session change.
  const cacheablePublicAsset = url.pathname.startsWith("/assets/") || isStaticAsset(url);
  if (!cacheablePublicAsset) return;

  event.respondWith(
    fetch(req)
      .then((res) => {
        // The copy has to be taken before the response is handed to the page:
        // caches.open() resolves a turn later, by which point the page has
        // already started reading the body and clone() throws.
        if (res.ok) {
          const copy = res.clone();
          // waitUntil keeps the worker alive until the write lands; without it
          // the browser may terminate the worker first and the asset silently
          // never reaches the cache.
          event.waitUntil(cacheAsset(req, copy));
        }
        return res;
      })
      .catch(() => caches.match(req))
  );
});

function isStaticAsset(url) {
  return STATIC_ASSETS.includes(`${url.pathname}${url.search}`) || STATIC_ASSETS.includes(url.pathname);
}

async function cacheAsset(req, res) {
  let cache;
  try {
    cache = await caches.open(STATIC_CACHE);
    await cache.put(req, res);
  } catch {
    // Almost always the storage quota. There is no retry here: put has already
    // read the body, and a second clone would have had to be taken on the hot
    // path for a case that resolves itself anyway -- the trim below leaves room,
    // and this asset is fetched network-first, so the next request for it lands
    // in a cache that now has space.
    if (cache) await trimAssetCache(cache, null);
    return;
  }
  await trimAssetCache(cache, MAX_CACHED_ASSETS);
}

// trimAssetCache drops the entries cached longest ago, down to limit -- or, for
// a null limit, down to half of what is there, which is what a caller reacting
// to a failed write asks for: it cannot know how many entries the quota has
// room for, only that it is fewer than this.
//
// The precached static assets are skipped however old they are: they are
// written once at install and never requested again on a healthy install, so by
// age alone they are always first in line -- and they are the offline fallback,
// which is the one thing this cache exists to still have.
async function trimAssetCache(cache, limit) {
  try {
    const keys = await cache.keys();
    const target = limit === null ? Math.max(MIN_CACHED_ASSETS, Math.floor(keys.length / 2)) : limit;
    let removable = keys.length - target;
    if (removable <= 0) return;
    // Issued together rather than awaited one at a time: the keys are distinct,
    // so the deletes are independent, and the pressure path can be dropping a
    // dozen at once while waitUntil holds the worker open for all of them.
    const deletions = [];
    for (const key of keys) {
      if (removable <= 0) break;
      if (isStaticAsset(new URL(key.url))) continue;
      deletions.push(cache.delete(key));
      removable--;
    }
    await Promise.all(deletions);
  } catch {
    // A cache that cannot be read or pruned still serves the page from the
    // network; there is nothing to recover here and nobody to report it to.
  }
}

self.addEventListener("push", (event) => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch {
    data = {};
  }
  const title = typeof data.title === "string" && data.title ? data.title : "rolltop";
  const icon = typeof data.icon === "string" && data.icon ? data.icon : "/icon.svg?v=transparent-logo-v2";
  const options = {
    body: typeof data.body === "string" ? data.body : "New mail synced.",
    tag: typeof data.tag === "string" && data.tag ? data.tag : "rolltop-new-mail",
    icon,
    badge: typeof data.badge === "string" && data.badge ? data.badge : icon,
    data: {
      url: sameOriginPath(data.url, "/mail"),
      apiURL: sameOriginPath(data.api_url, ""),
      messageID: Number(data.message_id || 0)
    }
  };
  event.waitUntil(Promise.all([
    warmNotificationMessage(options.data),
    self.registration.showNotification(title, options)
  ]));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const data = event.notification.data || {};
  const targetURL = new URL(sameOriginPath(data.url, "/mail"), self.location.origin).href;
  event.waitUntil(
    Promise.all([
      warmNotificationMessage(data),
      self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(async (clients) => {
        for (const client of clients) {
          if (!client.url.startsWith(self.location.origin)) continue;
          if ("navigate" in client && client.url !== targetURL) {
            const navigated = await client.navigate(targetURL);
            return navigated?.focus();
          }
          return client.focus();
        }
        return self.clients.openWindow(targetURL);
      })
    ])
  );
});

function sameOriginPath(value, fallback) {
  if (typeof value !== "string" || !value) return fallback;
  try {
    const url = new URL(value, self.location.origin);
    if (url.origin !== self.location.origin) return fallback;
    return `${url.pathname}${url.search}${url.hash}`;
  } catch {
    return fallback;
  }
}

async function warmNotificationMessage(data) {
  const apiURL = sameOriginPath(data?.apiURL, "");
  if (!apiURL) return;
  const url = new URL(apiURL, self.location.origin);
  if (!isNotificationWarmAPIURL(url)) return;
  try {
    await fetch(apiURL, {
      method: "GET",
      headers: { Accept: "application/json" },
      credentials: "include",
      cache: "reload"
    });
  } catch {
    // Navigation should still work if the background warm-up misses.
  }
}

function isNotificationWarmAPIURL(url) {
  if (/^\/api\/messages\/\d+\/prefetch$/.test(url.pathname) && !url.search) return true;
  return url.pathname === "/api/mail" && url.searchParams.get("page") === "1" && Array.from(url.searchParams.keys()).length === 1;
}

self.addEventListener("message", (event) => {
  const data = event.data || {};
  if (data.type === "rolltop:security-unlock-get") {
    const userID = Number(data.userID || 0);
    const state = currentSecurityUnlockState(userID);
    if (!state.unlockedUntil) requestSecurityUnlockStateFromClients(userID);
    event.source?.postMessage({ type: "rolltop:security-unlock-state", userID, state });
    return;
  }
  if (data.type !== "rolltop:security-unlock-set") return;
  const userID = Number(data.userID || 0);
  const state = normalizeSecurityUnlockState(data.state);
  securityUnlockUserID = userID;
  securityUnlockState = state;
  scheduleSecurityLock();
  broadcastSecurityUnlockState(userID);
});

function normalizeSecurityUnlockState(state) {
  const unlockedUntil = Number(state?.unlockedUntil || 0);
  if (!unlockedUntil || unlockedUntil <= Date.now()) return { unlockedUntil: 0, keys: [] };
  const keys = Array.isArray(state?.keys) ? state.keys.filter((key) => key && key.private_key_armored) : [];
  return keys.length > 0 ? { unlockedUntil, keys } : { unlockedUntil: 0, keys: [] };
}

function currentSecurityUnlockState(userID) {
  if (userID !== securityUnlockUserID) return { unlockedUntil: 0, keys: [] };
  securityUnlockState = normalizeSecurityUnlockState(securityUnlockState);
  if (!securityUnlockState.unlockedUntil) securityUnlockUserID = 0;
  return securityUnlockState;
}

function scheduleSecurityLock() {
  if (securityUnlockTimer) clearTimeout(securityUnlockTimer);
  securityUnlockTimer = 0;
  if (!securityUnlockState.unlockedUntil) return;
  securityUnlockTimer = setTimeout(() => {
    const userID = securityUnlockUserID;
    securityUnlockUserID = 0;
    securityUnlockState = { unlockedUntil: 0, keys: [] };
    broadcastSecurityUnlockState(userID);
  }, Math.max(0, securityUnlockState.unlockedUntil - Date.now()));
}

async function broadcastSecurityUnlockState(userID) {
  const clients = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  const state = currentSecurityUnlockState(userID);
  clients.forEach((client) => client.postMessage({ type: "rolltop:security-unlock-state", userID, state }));
}

async function requestSecurityUnlockStateFromClients(userID) {
  const clients = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  clients.forEach((client) => client.postMessage({ type: "rolltop:security-unlock-request", userID }));
}
