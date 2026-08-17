// File overview: Theme styling for the sandboxed message-body iframes. The app
// shell and the PGP plugin both render message documents, so the rules live
// here once and are woven into the document string before the iframe loads it:
// the body is themed on its first paint rather than repainted afterwards.

/** EmailDocumentTheme is the theme marker written onto a message document. */
export type EmailDocumentTheme = "classic" | "classic_dark" | "matrix";

const themeMarkerAttribute = "data-rolltop-theme";

// Dark message bodies use the same surface the .email-frame sits on, so the
// mail content and its frame belong to one colour family. Mail markup carries
// its own inline colours, which is why these need !important.
const emailDocumentThemeCSS = [
  `html[${themeMarkerAttribute}="classic_dark"],html[${themeMarkerAttribute}="classic_dark"] body{background:#1a2230!important;color:#e2dfd9!important;color-scheme:dark}`,
  `html[${themeMarkerAttribute}="classic_dark"] body :where(div,p,span,blockquote,pre,td,th,li){background:transparent!important;color:inherit!important;border-color:rgba(226,223,217,.24)!important}`,
  `html[${themeMarkerAttribute}="classic_dark"] a{color:#9cc9ea!important;border-bottom-color:rgba(156,201,234,.5)!important}`,
  `html[${themeMarkerAttribute}="matrix"],html[${themeMarkerAttribute}="matrix"] body{background:#06130d!important;color:#dcffe9!important;color-scheme:dark}`,
  `html[${themeMarkerAttribute}="matrix"] body :where(div,p,span,blockquote,pre,td,th,li){background:transparent!important;color:inherit!important;border-color:rgba(74,222,128,.24)!important}`,
  `html[${themeMarkerAttribute}="matrix"] a{color:#7dffbf!important;border-bottom-color:rgba(125,255,191,.5)!important}`,
  // No marker means the reader follows the system, so the iframe does too.
  `@media (prefers-color-scheme:dark){`,
  `html:not([${themeMarkerAttribute}]),html:not([${themeMarkerAttribute}]) body{background:#1a2230!important;color:#e2dfd9!important;color-scheme:dark}`,
  `html:not([${themeMarkerAttribute}]) body :where(div,p,span,blockquote,pre,td,th,li){background:transparent!important;color:inherit!important;border-color:rgba(226,223,217,.24)!important}`,
  `html:not([${themeMarkerAttribute}]) a{color:#9cc9ea!important;border-bottom-color:rgba(156,201,234,.5)!important}`,
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
