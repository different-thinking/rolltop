// File overview: What happens to the line breaks a writer pastes into the
// compose editor on the way out to a recipient's mail client.

import { describe, expect, it } from "vitest";
import { composeTextFromHTML, convertTextNewlinesToBreaks, textToHTML } from "./html";

function converted(html: string): string {
  const template = document.createElement("template");
  template.innerHTML = html;
  convertTextNewlinesToBreaks(template.content);
  return template.innerHTML;
}

describe("convertTextNewlinesToBreaks", () => {
  it("gives pasted line breaks and blank lines markup a mail client renders", () => {
    // What Chromium leaves in the editor when a plain-text paragraph is pasted
    // into an element that renders with white-space: pre-wrap.
    const pasted = "Guten Morgen,\n\nwir kommen mit 30 Personen.\n\nViele Gruesse\nRobert";
    expect(converted(pasted)).toBe("Guten Morgen,<br><br>wir kommen mit 30 Personen.<br><br>Viele Gruesse<br>Robert");
  });

  it("leaves typed markup and its surrounding text alone", () => {
    const typed = "<div>Guten Morgen,</div><div><br></div><div>wir kommen mit 30 Personen.</div>";
    expect(converted(typed)).toBe(typed);
  });

  it("breaks text inside formatting that was pasted into it", () => {
    expect(converted("<div><strong>Erste Zeile\nZweite Zeile</strong></div>")).toBe(
      "<div><strong>Erste Zeile<br>Zweite Zeile</strong></div>"
    );
  });

  it("normalizes carriage returns the same way", () => {
    expect(converted("Erste Zeile\r\nZweite Zeile")).toBe("Erste Zeile<br>Zweite Zeile");
  });

  it("keeps the source newlines of a quoted reply out of the recipient's copy", () => {
    const quoted = '<blockquote class="rolltop-reply-body"><table>\n<tr><td>Preis</td></tr>\n</table></blockquote>';
    expect(converted(quoted)).toBe(quoted);
  });

  it("keeps the source newlines of a forwarded message out of it too", () => {
    const forwarded = '<div class="rolltop-forwarded-body"><p>Angebot</p>\n<p>Anlage</p></div>';
    expect(converted(forwarded)).toBe(forwarded);
  });

  it("leaves preformatted text as the browser already renders it", () => {
    const preformatted = "<pre>zeile eins\nzeile zwei</pre>";
    expect(converted(preformatted)).toBe(preformatted);
  });

  it("does nothing to a body that already carries only markup", () => {
    expect(converted(textToHTML("Guten Morgen,\n\nRobert"))).toBe("Guten Morgen,<br><br>Robert");
  });
});

describe("composeTextFromHTML", () => {
  it("spaces typed paragraphs the way the editor shows them", () => {
    // What Chromium leaves behind for "A", Enter, Enter, "B". innerText reports
    // two blank lines here; the editor renders one.
    expect(composeTextFromHTML("<div>A</div><div><br></div><div>B</div>")).toBe("A\n\nB");
  });

  it("keeps a second blank line when the writer typed one", () => {
    expect(composeTextFromHTML("<div>A</div><div><br></div><div><br></div><div>B</div>")).toBe("A\n\n\nB");
  });

  it("does not turn the break that ends a block into a blank line", () => {
    expect(composeTextFromHTML("<div>A<br></div><div>B</div>")).toBe("A\nB");
  });

  it("reads a break inside a block as the line break it renders", () => {
    expect(composeTextFromHTML("<div>A<br>B</div>")).toBe("A\nB");
  });

  it("reads pasted text that carries its own breaks", () => {
    expect(composeTextFromHTML("Guten Morgen,<br><br>Robert")).toBe("Guten Morgen,\n\nRobert");
  });

  it("does not add a line for a block that only wraps another", () => {
    expect(composeTextFromHTML("<div><div>A</div></div><div>B</div>")).toBe("A\nB");
  });

  it("starts a block on its own line after loose text", () => {
    expect(composeTextFromHTML("A<div>B</div>")).toBe("A\nB");
  });

  it("gives a quoted table one line per row", () => {
    expect(composeTextFromHTML("<table><tr><td>Vorspeise</td></tr><tr><td>Nachtisch</td></tr></table>")).toBe("Vorspeise\nNachtisch");
  });

  it("keeps inline formatting on the line it belongs to", () => {
    expect(composeTextFromHTML("<div>Preis <strong>29 EUR</strong> pro Person</div>")).toBe("Preis 29 EUR pro Person");
  });

  it("drops the trailing blank lines an editor leaves behind", () => {
    expect(composeTextFromHTML("<div>A</div><div><br></div><div><br></div>")).toBe("A");
  });
});
