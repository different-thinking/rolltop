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

// The text part of a message is built from the same markup as its HTML part
// rather than from the editor's innerText, which counts a line twice wherever
// the browser put a <br> at the end of a block. An empty line between two typed
// paragraphs is one `<div><br></div>` and renders as one blank line; innerText
// reports two, so the recipient reading the text part saw the message spaced
// out twice as far as the writer spaced it -- and a plain-text reader is
// exactly who cannot fall back on the HTML part.
//
// What matches the rendering is that a block element ends its line, and a <br>
// adds one only when something follows it inside that block: a <br> at the end
// of a block is what makes an empty line editable, not a line of its own.
const blockSelector = "address, blockquote, dd, div, dl, dt, h1, h2, h3, h4, h5, h6, li, ol, p, pre, table, tr, ul";

export function composeTextFromHTML(html: string): string {
  const template = document.createElement("template");
  template.innerHTML = html;
  return textFromChildNodes(template.content).replace(/\n+$/, "");
}

function textFromChildNodes(parent: ParentNode): string {
  const children = Array.from(parent.childNodes);
  let text = "";
  children.forEach((node, index) => {
    if (node.nodeType === Node.TEXT_NODE) {
      text += (node as Text).data;
      return;
    }
    if (node.nodeType !== Node.ELEMENT_NODE) return;
    const element = node as Element;
    if (element.tagName === "BR") {
      if (index < children.length - 1) text += "\n";
      return;
    }
    const raw = textFromChildNodes(element);
    if (!element.matches(blockSelector)) {
      text += raw;
      return;
    }
    const inner = element.tagName === "BLOCKQUOTE" ? quoted(raw) : raw;
    // A block starts on a line of its own and ends the one it is on. An empty
    // block still ends a line, which is what keeps a blank paragraph blank.
    if (text !== "" && !text.endsWith("\n")) text += "\n";
    text += inner;
    if (inner === "" || !inner.endsWith("\n")) text += "\n";
  });
  return text;
}

// The message being answered is a <blockquote> in the editor, and the text part
// is where that has to be said in characters: without the quote markers a reply
// reads to a plain-text reader as if the writer had written the original's
// sentences themselves. A line already quoted gains a second marker rather than
// a second "> ", which is how a quote of a quote has always been written.
function quoted(text: string): string {
  return text
    .replace(/\n+$/, "")
    .split("\n")
    .map((line) => (line === "" ? ">" : line.startsWith(">") ? `>${line}` : `> ${line}`))
    .join("\n");
}
