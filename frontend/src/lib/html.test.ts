// File overview: What happens to the line breaks a writer pastes into the
// compose editor on the way out to a recipient's mail client.

import { describe, expect, it } from "vitest";
import { convertTextNewlinesToBreaks, textToHTML } from "./html";

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
