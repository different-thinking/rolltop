// File overview: Theme styling for the sandboxed message-body iframes. The app
// shell and the PGP plugin both render message documents, so the rules live
// here once and are woven into the document string before the iframe loads it:
// the body is themed on its first paint rather than repainted afterwards.
//
// Only plain-text messages are themed. An HTML message carries the sender's own
// colours, and overriding them is how a readable newsletter turns into light
// text on the mail's own white table. Those messages keep their design and
// render as a light sheet inside the dark app, the way desktop mail clients do.

/** EmailDocumentTheme is the theme marker written onto a message document. */
export type EmailDocumentTheme = "classic" | "classic_dark" | "matrix";

const themeMarkerAttribute = "data-rolltop-theme";

/**
 * plainTextDocumentClass marks a message document whose body Rolltop rendered
 * itself from plain text. It is the only case where there are no author colours
 * to conflict with, so it is the only case that gets a dark ground.
 */
export const plainTextDocumentClass = "plaintext-doc";

const darkGrounds: Record<"classic_dark" | "matrix", { bg: string; text: string; link: string; linkBorder: string }> = {
  // The same surface the .email-frame sits on, so message and frame belong to
  // one colour family.
  classic_dark: { bg: "#1a2230", text: "#e2dfd9", link: "#9cc9ea", linkBorder: "rgba(156,201,234,.5)" },
  matrix: { bg: "#06130d", text: "#dcffe9", link: "#7dffbf", linkBorder: "rgba(125,255,191,.5)" }
};

function darkRules(root: string, ground: { bg: string; text: string; link: string; linkBorder: string }): string {
  return `${root},${root} body{background:${ground.bg}!important;color:${ground.text}!important;color-scheme:dark}`
    + `${root} a{color:${ground.link}!important;border-bottom-color:${ground.linkBorder}!important}`;
}

const plainText = `html.${plainTextDocumentClass}`;

const emailDocumentThemeCSS = [
  darkRules(`${plainText}[${themeMarkerAttribute}="classic_dark"]`, darkGrounds.classic_dark),
  darkRules(`${plainText}[${themeMarkerAttribute}="matrix"]`, darkGrounds.matrix),
  // No marker means the reader follows the system, so the document does too.
  `@media (prefers-color-scheme:dark){`,
  darkRules(`${plainText}:not([${themeMarkerAttribute}])`, darkGrounds.classic_dark),
  `}`
].join("");

const themeStyleTag = `<style>${emailDocumentThemeCSS}</style>`;

/**
 * currentEmailDocumentTheme reports the theme a message body should render in,
 * or null when the reader follows the system and the document should decide
 * for itself via prefers-color-scheme.
 */
export function currentEmailDocumentTheme(): EmailDocumentTheme | null {
  const theme = document.documentElement.dataset.theme;
  if (!theme) return null;
  if (theme === "classic_dark" || theme === "matrix") return theme;
  return "classic";
}

/**
 * themedEmailDocument returns the message document with the theme stylesheet
 * and the active theme marker in place. It is applied to the srcDoc string, so
 * the iframe never paints an unthemed frame first.
 */
export function themedEmailDocument(srcDoc: string): string {
  const theme = currentEmailDocumentTheme();
  const doc = theme ? srcDoc.replace(/<html\b/i, `<html ${themeMarkerAttribute}="${theme}"`) : srcDoc;
  if (/<\/head>/i.test(doc)) return doc.replace(/<\/head>/i, `${themeStyleTag}</head>`);
  // No head element: the stylesheet goes after the complete start tag, never
  // inside it, because the html element carries attributes of its own.
  if (/<html\b[^>]*>/i.test(doc)) return doc.replace(/<html\b[^>]*>/i, (tag) => `${tag}${themeStyleTag}`);
  return themeStyleTag + doc;
}

/**
 * applyEmailDocumentTheme re-marks an already loaded message document, which
 * keeps open message bodies in step when the reader switches themes.
 */
export function applyEmailDocumentTheme(doc: Document | null | undefined): void {
  if (!doc) return;
  const theme = currentEmailDocumentTheme();
  if (!theme) {
    doc.documentElement.removeAttribute(themeMarkerAttribute);
    return;
  }
  doc.documentElement.setAttribute(themeMarkerAttribute, theme);
}
