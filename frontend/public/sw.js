const STATIC_CACHE = "rolltop-static-v12";
// The app shell the reader last started online, kept so the app can start
// again offline. It is the HTML the server personalises per navigation - the
// signed-in user's startup data is embedded in it - so three rules keep it
// from outliving the session it belongs to. It is stored only when its startup
// data names a user, and a shell for nobody (the login page after a sign-out)
// deletes it; the page asks for it to be dropped on sign-out
// (rolltop:offline-clear), before the sign-out request can even fail; and it is
// served only when the network could not answer a navigation, never in front of
// it. The server marks it Vary: * so Cache.put would refuse the response as
// received; what is stored is a fresh Response built from its text.
const SHELL_CACHE = "rolltop-shell-v1";
const SHELL_KEY = "/__rolltop/app-shell";
// The routes a shell is never remembered from or served for: they are the
// sign-in pages, which are no use without the server.
const SHELL_PUBLIC_PATHS = ["/login", "/setup", "/reset-password"];
// The hashed assets the stored shell references. They are kept out of the
// asset cache's eviction, because a shell whose script was evicted starts to a
// blank page rather than to offline.html, which is the worse of the two.
let shellAssets = null;
// Bumped by every forgetShell. A rememberShell that began before a sign-out
// cleared the shell must not finish after it: the navigation's body is read
// and the cache opened across several turns, and a put landing after the
// delete would put the signed-out user's shell back.
let shellGeneration = 0;
const STATIC_ASSETS = [
  "/offline.html",
  "/icon.svg",
  "/icon.svg?v=transparent-logo-v2",
  "/icons/mail.svg",
  "/icons/calendar.svg",
  "/icons/contacts.svg"
];
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
    caches.keys().then((keys) => {
      const stale = keys.filter((key) => key !== STATIC_CACHE && key !== SHELL_CACHE);
      // A new asset cache leaves the old one behind, and the stored shell's
      // scripts with it; a shell whose scripts are gone starts to a blank page.
      // The shell goes too, and the next online navigation stores the new one
      // together with the assets it names.
      return Promise.all([...stale.map((key) => caches.delete(key)), stale.length > 0 ? forgetShell() : undefined]);
    })
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
      fetch(networkRequest)
        .then((res) => {
          // The copy is taken before the response reaches the browser, for the
          // same reason as below: the body is read as soon as it is handed
          // over. Only a page is copied: a navigation can also be a download,
          // and a clone of one nobody reads holds the whole file in memory
          // until the browser has finished saving it.
          if (req.mode === "navigate" && isHTMLPage(res)) event.waitUntil(rememberShell(url, res.clone()));
          return res;
        })
        .catch(() => {
          if (req.mode !== "navigate") return Promise.reject(new Error("offline"));
          return offlineShell(url).then((shell) => shell || caches.match("/offline.html"));
        })
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

function isHTMLPage(res) {
  return res.ok && (res.headers.get("Content-Type") || "").toLowerCase().includes("text/html");
}

async function rememberShell(url, res) {
  const generation = shellGeneration;
  try {
    const text = await res.text();
    // The startup data is a JSON object; a signed-in shell carries its user as
    // an object under that key, and json encoding cannot produce this sequence
    // inside a string value (the quotes there are escaped). A shell without it
    // is the app signed out - the login page, or an app route answered for a
    // session that has ended - and the shell kept from before that goes. This
    // is checked ahead of the route, so a visit to the login page cannot leave
    // the previous session's shell behind.
    if (!text.includes('<script id="rolltop-startup"') || !text.includes('"user":{"')) {
      await forgetShell();
      return;
    }
    if (SHELL_PUBLIC_PATHS.includes(url.pathname)) return;
    // The assets first, the shell second: a shell stored while one of its
    // scripts is missing from the cache starts offline to a blank page, where
    // the shell kept from the previous navigation would have started the app.
    // So the previous shell stays until every asset the new one names is
    // cached, and a navigation whose assets could not all be fetched leaves
    // the stored shell as it was.
    const assets = assetPathsIn(text);
    if (!(await precacheShellAssets(assets))) return;
    if (generation !== shellGeneration) return;
    const cache = await caches.open(SHELL_CACHE);
    await cache.put(SHELL_KEY, new Response(text, { headers: { "Content-Type": "text/html; charset=utf-8" } }));
    if (generation !== shellGeneration) {
      // A sign-out arrived between the check and the write; the write is
      // taken back rather than left as the signed-out user's shell.
      await forgetShell();
      return;
    }
    shellAssets = assets;
  } catch {
    // Starting offline is a convenience; the navigation itself was answered.
  }
}

async function offlineShell(url) {
  if (SHELL_PUBLIC_PATHS.includes(url.pathname)) return undefined;
  try {
    const cache = await caches.open(SHELL_CACHE);
    return await cache.match(SHELL_KEY);
  } catch {
    return undefined;
  }
}

async function forgetShell() {
  shellGeneration += 1;
  shellAssets = new Set();
  try {
    const cache = await caches.open(SHELL_CACHE);
    await cache.delete(SHELL_KEY);
  } catch {
    // Nothing stored, nothing to forget.
  }
}

function assetPathsIn(html) {
  const paths = new Set();
  const pattern = /(?:src|href)="(\/assets\/[^"]+)"/g;
  let match;
  while ((match = pattern.exec(html)) !== null) paths.add(match[1]);
  return paths;
}

// The shell's assets are fetched network-first like every other asset, so on a
// healthy install they are cached by the time the page has loaded them. This
// covers the ones the page did not ask for - a modulepreload the browser
// skipped, a stylesheet a theme change will want - so a start offline finds
// every file the shell names. It reports whether every asset is now cached;
// the shell is stored only on a yes.
async function precacheShellAssets(paths) {
  const cache = await caches.open(STATIC_CACHE);
  let complete = true;
  for (const path of paths) {
    if (await cache.match(path)) continue;
    try {
      await cache.add(path);
    } catch {
      // The next online navigation tries again; until then the previous
      // shell, whose assets are all there, is the one served offline.
      complete = false;
    }
  }
  return complete;
}

async function currentShellAssets() {
  if (shellAssets) return shellAssets;
  try {
    const cache = await caches.open(SHELL_CACHE);
    const shell = await cache.match(SHELL_KEY);
    shellAssets = shell ? assetPathsIn(await shell.text()) : new Set();
  } catch {
    shellAssets = new Set();
  }
  return shellAssets;
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
    const pinned = await currentShellAssets();
    const target = limit === null ? Math.max(MIN_CACHED_ASSETS, Math.floor(keys.length / 2)) : limit;
    let removable = keys.length - target;
    if (removable <= 0) return;
    // Issued together rather than awaited one at a time: the keys are distinct,
    // so the deletes are independent, and the pressure path can be dropping a
    // dozen at once while waitUntil holds the worker open for all of them.
    const deletions = [];
    for (const key of keys) {
      if (removable <= 0) break;
      const keyURL = new URL(key.url);
      if (isStaticAsset(keyURL) || pinned.has(keyURL.pathname)) continue;
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
  if (data.type === "rolltop:offline-clear") {
    event.waitUntil(forgetShell());
    return;
  }
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
