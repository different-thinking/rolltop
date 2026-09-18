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
const quotedSourceSelector = ".rolltop-reply-body, .rolltop-forwarded-body";
const quotedOrPreformattedSelector = `pre, textarea, ${quotedSourceSelector}`;

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
// What matches the rendering is that a block element ends the line it is on, a
// cell ends a column, and a <br> ends a line wherever it stands: a <br> at the
// end of a block adds nothing on top of the block's own line ending, while one
// at the end of a bold run still breaks the line the bold run is on.
const blockSelector =
  "address, article, aside, blockquote, center, dd, div, dl, dt, figcaption, figure, footer, "
  + "h1, h2, h3, h4, h5, h6, header, hr, li, main, ol, p, pre, section, table, tr, ul";
const cellSelector = "td, th";

export function composeTextFromHTML(html: string): string {
  const template = document.createElement("template");
  template.innerHTML = html;
  return textFromChildNodes(template.content, false).replace(/\n+$/, "");
}

// `sourceWhitespace` marks the markup whose newlines were never line breaks:
// the quoted and forwarded blocks convertTextNewlinesToBreaks leaves alone for
// exactly that reason, and reading them literally here would undo the same care
// on the other side of the message. A reply to an ordinary HTML mail, whose
// source is indented one tag per line, otherwise arrived as a text part of
// empty quote markers with a word adrift in each of them. Inside those blocks
// the whitespace is read the way the mail client that rendered the original
// read it -- runs of it are one space -- and only <br> and the blocks
// themselves end a line. A <pre> in there means what it says and is exempt.
function textFromChildNodes(parent: ParentNode, sourceWhitespace: boolean): string {
  let text = "";
  for (const node of Array.from(parent.childNodes)) {
    if (node.nodeType === Node.TEXT_NODE) {
      const data = (node as Text).data;
      text += sourceWhitespace ? collapsedWhitespace(data, text) : data;
      continue;
    }
    if (node.nodeType !== Node.ELEMENT_NODE) continue;
    const element = node as Element;
    if (element.tagName === "BR") {
      text += "\n";
      continue;
    }
    const nested = element.tagName === "PRE" ? false : sourceWhitespace || element.matches(quotedSourceSelector);
    const raw = textFromChildNodes(element, nested);
    if (!element.matches(blockSelector)) {
      // Two cells of a row are two columns and not one word: without something
      // between them an item and its price arrive as "Artikel12,50".
      if (element.matches(cellSelector) && text !== "" && !/\s$/.test(text)) text += "\t";
      text += raw;
      continue;
    }
    const cleaned = nested ? trimmedLines(raw) : raw;
    const inner = element.tagName === "BLOCKQUOTE" ? quoted(cleaned) : cleaned;
    // A block starts on a line of its own and ends the one it is on. An empty
    // block still ends a line, which is what keeps a blank paragraph blank.
    if (sourceWhitespace) text = text.replace(/[ \t]+$/, "");
    if (text !== "" && !text.endsWith("\n")) text += "\n";
    text += inner;
    if (inner === "" || !inner.endsWith("\n")) text += "\n";
  }
  return text;
}

// A run of source whitespace is one space, and a space that lands where a line
// has just ended is not a space at all.
function collapsedWhitespace(data: string, text: string): string {
  const collapsed = data.replace(/\s+/g, " ");
  if (!collapsed.startsWith(" ")) return collapsed;
  return text === "" || /\s$/.test(text) ? collapsed.slice(1) : collapsed;
}

function trimmedLines(text: string): string {
  return text.replace(/[ \t]+\n/g, "\n").replace(/\n[ \t]+/g, "\n").replace(/^[ \t]+|[ \t]+$/g, "");
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
