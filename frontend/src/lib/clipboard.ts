// File overview: Putting a string on the clipboard from a click. The async
// clipboard API is the first choice; the hidden-textarea route stays because an
// Android WebView or a page served without a secure context has no
// navigator.clipboard at all, and a copy button that silently does nothing is
// worse than no button.

/** copyText copies a string and reports whether anything took it. */
export async function copyText(text: string): Promise<boolean> {
  if (!text) return false;
  try {
    if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // Permission denied or no secure context; fall through to the selection route.
  }
  if (typeof document === "undefined") return false;
  const area = document.createElement("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.style.position = "fixed";
  area.style.top = "0";
  area.style.left = "0";
  area.style.opacity = "0";
  document.body.appendChild(area);
  try {
    area.focus();
    area.select();
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    area.remove();
  }
}
