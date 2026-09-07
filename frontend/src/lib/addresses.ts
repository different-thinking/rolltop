// File overview: Reading an RFC 5322 address line the way the thread header
// needs it - one entry per correspondent, each with the name a human reads and
// the address a machine sends to - and writing one back for the composer.
//
// The server stores To and Cc the way Go's net/mail re-serialises them:
// `strconv.Quote(name) <address>` per entry, or the raw header when the parser
// refused it. So a quoted name here carries Go's escapes (\" \\ \t \u00a0) as
// well as RFC 5322's quoted-pairs, and the raw fallback can still carry
// comments and group syntax. Both are read here; the composer's recipient
// tokenizer splits with the same function so the two never disagree about
// where one entry ends.

/** MailAddress is one correspondent on an address line. */
export type MailAddress = {
  /** name is the display name, without its quotes. Empty when the line carried
   * a bare address. */
  name: string;
  /** email is the addr-spec, lower-cased for comparison. Empty for a group
   * label or a token that is not an address at all. */
  email: string;
};

/**
 * splitAddressList cuts an address line into its raw entries. Commas and
 * semicolons separate entries except inside a quoted name, inside angle
 * brackets or inside a comment, which is where a real name puts them.
 */
export function splitAddressList(value: string): string[] {
  const out: string[] = [];
  let part = "";
  let quoted = false;
  let escaped = false;
  let inAngle = false;
  let comment = 0;
  const flush = () => {
    const trimmed = part.trim();
    if (trimmed) out.push(trimmed);
    part = "";
  };
  for (const char of value) {
    if (escaped) {
      part += char;
      escaped = false;
      continue;
    }
    if (char === "\\" && quoted) {
      part += char;
      escaped = true;
      continue;
    }
    if (char === '"' && comment === 0) {
      quoted = !quoted;
    } else if (!quoted) {
      if (char === "(") comment += 1;
      else if (char === ")" && comment > 0) comment -= 1;
      else if (comment === 0 && char === "<") inAngle = true;
      else if (comment === 0 && char === ">") inAngle = false;
      else if (comment === 0 && !inAngle && (char === "," || char === ";")) {
        flush();
        continue;
      }
    }
    part += char;
  }
  flush();
  return out;
}

/** parseAddressList reads every entry of an address line. */
export function parseAddressList(value: string): MailAddress[] {
  return splitAddressList(value)
    .map(parseAddress)
    .filter((entry): entry is MailAddress => entry !== null);
}

/**
 * parseAddress reads one entry. It answers null for whitespace and for an
 * entry with neither a name nor an address, such as a bare `<>`.
 */
export function parseAddress(value: string): MailAddress | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  const angled = trimmed.match(/^(.*)<([^<>]*)>\s*$/s);
  if (angled) {
    const entry = { name: cleanName(angled[1]), email: normalizeEmail(angled[2]) };
    return entry.name || entry.email ? entry : null;
  }
  const uncommented = stripComments(trimmed).trim();
  if (/^[^\s<>"(),;:]+@[^\s<>"(),;:]+$/.test(uncommented)) {
    return { name: "", email: normalizeEmail(uncommented) };
  }
  // A group (`Team: alice@x.test`) names its first member after the label; the
  // label itself is not a correspondent.
  const group = uncommented.match(/^[^"<>@:]*:\s*(.+)$/s);
  if (group) return parseAddress(group[1]);
  const name = cleanName(trimmed);
  return name ? { name, email: "" } : null;
}

function cleanName(value: string): string {
  const trimmed = stripComments(value).trim();
  if (trimmed.startsWith('"') && trimmed.endsWith('"') && trimmed.length >= 2) {
    return unescapeQuoted(trimmed.slice(1, -1)).trim();
  }
  return trimmed;
}

// stripComments drops `(...)` outside quotes; inside a quoted name parentheses
// are part of the name, as in `"Smith, John (Acme)"`.
function stripComments(value: string): string {
  let out = "";
  let quoted = false;
  let escaped = false;
  let depth = 0;
  for (const char of value) {
    if (escaped) {
      out += char;
      escaped = false;
      continue;
    }
    if (quoted && char === "\\") {
      out += char;
      escaped = true;
      continue;
    }
    if (char === '"' && depth === 0) quoted = !quoted;
    if (!quoted) {
      if (char === "(") {
        depth += 1;
        continue;
      }
      if (char === ")" && depth > 0) {
        depth -= 1;
        continue;
      }
      if (depth > 0) continue;
    }
    out += char;
  }
  return out;
}

// unescapeQuoted undoes both Go's strconv.Quote escapes and RFC 5322's
// quoted-pairs, which agree on \" and \\ and differ only in what else Go can
// write: a control character or a non-printable rune becomes \t, \xNN or
// \uNNNN rather than being written as itself.
function unescapeQuoted(value: string): string {
  return value.replace(/\\(u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8}|x[0-9a-fA-F]{2}|.)/gs, (_match, code: string) => {
    switch (code[0]) {
      case "u":
      case "U":
      case "x":
        return code.length > 1 ? String.fromCodePoint(Number.parseInt(code.slice(1), 16)) : code;
      case "t":
        return "\t";
      case "n":
        return "\n";
      case "r":
        return "\r";
      default:
        return code;
    }
  });
}

function normalizeEmail(value: string): string {
  return value.trim().toLowerCase();
}

/**
 * formatAddress writes an entry back as a line the composer's To field
 * accepts. A name that is more than letters, digits and light punctuation is
 * quoted, and any quote inside it becomes an apostrophe rather than an escape:
 * the composer's recipient chip strips only the outer quotes, so an escaped
 * quote would be shown with its backslash.
 */
export function formatAddress(address: MailAddress): string {
  const email = address.email.trim();
  const name = address.name.trim();
  if (!email) return name;
  if (!name || name.toLowerCase() === email.toLowerCase()) return email;
  if (/^[\p{L}\p{N} .'_-]+$/u.test(name)) return `${name} <${email}>`;
  return `"${name.replace(/["\\]/g, "'")}" <${email}>`;
}

/** sameEmail compares two addresses the way the address book does. */
export function sameEmail(left: string, right: string): boolean {
  const a = left.trim().toLowerCase();
  return a !== "" && a === right.trim().toLowerCase();
}
