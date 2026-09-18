// File overview: Small HTML conversion helpers used by compose when plain text needs a safe editable
// HTML representation.

export function textToHTML(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll("\n", "<br>");
}

// The compose editor renders with `white-space: pre-wrap`, so text pasted into
// it keeps its newlines as plain characters: the browser has no reason to turn
// them into markup, and the writer sees every line break and blank line they
// pasted. A mail client renders the HTML part of the sent message with the CSS
// default instead, where a newline is ordinary whitespace, so the same body
// arrives as one run-on paragraph -- breaks gone, blank lines gone, a signature
// folded onto the end of the last sentence. Converting those newlines to <br>
// before the message leaves makes the recipient's copy read like the editor
// looked.
//
// Quoted and forwarded blocks are deliberately skipped. They carry markup
// copied out of the message being answered, and the newlines between its tags
// were never line breaks; converting them would add a blank line for every
// newline in the original's source.
const quotedOrPreformattedSelector = "pre, textarea, .rolltop-reply-body, .rolltop-forwarded-body";

export function convertTextNewlinesToBreaks(root: ParentNode): void {
  for (const node of Array.from(root.childNodes)) {
    if (node.nodeType === Node.TEXT_NODE) {
      replaceNewlinesWithBreaks(node as Text);
      continue;
    }
    if (node.nodeType !== Node.ELEMENT_NODE) continue;
    const element = node as Element;
    if (element.matches(quotedOrPreformattedSelector)) continue;
    convertTextNewlinesToBreaks(element);
  }
}

function replaceNewlinesWithBreaks(node: Text): void {
  const segments = node.data.split(/\r\n|\r|\n/);
  if (segments.length < 2) return;
  const doc = node.ownerDocument;
  const replacement = doc.createDocumentFragment();
  segments.forEach((segment, index) => {
    if (index > 0) replacement.append(doc.createElement("br"));
    if (segment !== "") replacement.append(doc.createTextNode(segment));
  });
  node.replaceWith(replacement);
}
