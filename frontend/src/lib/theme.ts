// File overview: Applying the reader's theme to the document. The server already
// stamps data-theme into index.html so the first paint is correct; this keeps the
// document in step afterwards, when the reader changes the setting or the
// operating system flips its own light/dark preference.

/** systemThemeID follows the operating system instead of pinning a palette. */
export const systemThemeID = "system";

const themeColorMetaName = "theme-color";
const darkPreferenceQuery = "(prefers-color-scheme: dark)";

/**
 * applyDocumentTheme pins the document to a theme, or removes the marker for the
 * System theme. An absent marker is what lets the stylesheet fall through to
 * prefers-color-scheme, so it must be removed rather than set to "system".
 */
export function applyDocumentTheme(themeID: string | undefined): void {
  const root = document.documentElement;
  if (!themeID || themeID === systemThemeID) {
    delete root.dataset.theme;
  } else {
    root.dataset.theme = themeID;
  }
  syncBrowserChromeColor();
}

/**
 * syncBrowserChromeColor republishes theme-color from the active theme's
 * --chrome token. Reading it back from the stylesheet is what makes this work
 * for plugin themes too: the server cannot know their palette, the browser can.
 */
export function syncBrowserChromeColor(): void {
  const chrome = getComputedStyle(document.documentElement).getPropertyValue("--chrome").trim();
  if (!chrome) return;
  const existing = Array.from(document.head.querySelectorAll(`meta[name="${themeColorMetaName}"]`));
  // The shell ships a light/dark pair for the pre-hydration frame. One meta with
  // the resolved colour replaces it, because only one of the two can be right.
  for (const meta of existing.slice(1)) meta.remove();
  const meta = (existing[0] as HTMLMetaElement | undefined) || document.createElement("meta");
  meta.name = themeColorMetaName;
  meta.removeAttribute("media");
  meta.content = chrome;
  if (!meta.parentNode) document.head.appendChild(meta);
}

/**
 * watchSystemThemePreference re-publishes the chrome colour when the operating
 * system switches between light and dark. Returns a cleanup function.
 */
export function watchSystemThemePreference(): () => void {
  if (typeof window.matchMedia !== "function") return () => {};
  const query = window.matchMedia(darkPreferenceQuery);
  const onChange = () => syncBrowserChromeColor();
  query.addEventListener("change", onChange);
  return () => query.removeEventListener("change", onChange);
}
