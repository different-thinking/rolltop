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

  it("does not break a stylesheet that reached the editor", () => {
    const styled = "<style>.x {\n  color: red;\n}</style><div>Angebot</div>";
    expect(converted(styled)).toBe(styled);
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

  it("marks the quoted message of a reply", () => {
    const reply = 'Gern.<br><br><div>On Oct 9, 2026, counter@kamin-mainz.de wrote:</div>'
      + '<blockquote class="rolltop-reply-body"><div>Guten Tag,</div><div><br></div><div>das geht.</div></blockquote>';
    expect(composeTextFromHTML(reply)).toBe(
      "Gern.\n\nOn Oct 9, 2026, counter@kamin-mainz.de wrote:\n> Guten Tag,\n>\n> das geht."
    );
  });

  it("reads the indentation of a quoted message's source as the whitespace it is", () => {
    // What a reply to any ordinary HTML mail carries: one tag per line, the
    // newlines belonging to the source rather than to anything anyone wrote.
    const reply = "Danke!<br><br><div>On Oct 9, 2026, counter@kamin-mainz.de wrote:</div>"
      + '<blockquote class="rolltop-reply-body">\n  <table>\n    <tr>\n      <td>Artikel</td>\n'
      + "      <td>12,50</td>\n    </tr>\n  </table>\n</blockquote>";
    expect(composeTextFromHTML(reply)).toBe(
      "Danke!\n\nOn Oct 9, 2026, counter@kamin-mainz.de wrote:\n> Artikel 12,50"
    );
  });

  it("keeps a forwarded message's source formatting off its own lines", () => {
    const forwarded = '<div class="rolltop-forwarded-body">\n  <p>Angebot</p>\n\n  <p>Anlage</p>\n</div>';
    expect(composeTextFromHTML(forwarded)).toBe("Angebot\nAnlage");
  });

  it("reads a break that ends a formatting run as the line break it renders", () => {
    expect(composeTextFromHTML("<div><strong>Erste Zeile<br></strong>Zweite Zeile</div>")).toBe(
      "Erste Zeile\nZweite Zeile"
    );
  });

  it("keeps the two cells of a row two columns", () => {
    expect(composeTextFromHTML("<table><tr><td>Artikel</td><td>12,50</td></tr></table>")).toBe("Artikel\t12,50");
  });

  it("gives a centered block of an old newsletter its own line", () => {
    expect(composeTextFromHTML("<center>Angebot</center><center>Anlage</center>")).toBe("Angebot\nAnlage");
  });

  it("leaves a preformatted block inside a quote preformatted", () => {
    const quoted = '<blockquote class="rolltop-reply-body">\n  <pre>zeile eins\nzeile zwei</pre>\n</blockquote>';
    expect(composeTextFromHTML(quoted)).toBe("> zeile eins\n> zeile zwei");
  });

  it("leaves a stylesheet out of what the reader is shown", () => {
    expect(composeTextFromHTML("<style>.x { color: red; }</style><div>Angebot</div>")).toBe("Angebot");
  });

  it("says nothing for a quote of a message with no body", () => {
    expect(composeTextFromHTML('Danke!<blockquote class="rolltop-reply-body"></blockquote>')).toBe("Danke!");
  });

  it("marks a quote inside a quote once more", () => {
    const nested = "<blockquote><div>Antwort</div><blockquote><div>Frage</div></blockquote></blockquote>";
    expect(composeTextFromHTML(nested)).toBe("> Antwort\n>> Frage");
  });
});
