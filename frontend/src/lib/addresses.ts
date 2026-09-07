// File overview: Reading an RFC 5322 address line the way the thread header
// needs it - one entry per correspondent, each with the name a human reads and
// the address a machine sends to - and writing one back for the composer.
//
// The server hands the frontend the raw To and Cc lines, and the one existing
// reader of those (senderIdentity.displayLabel) only ever wants the first entry.
// The address card needs every entry, and it needs a split that survives the
// comma inside `"Doe, John" <john@example.test>`, which a split on commas does
// not.

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
 * parseAddressList splits an address line into its entries. Commas and
 * semicolons separate entries except inside a quoted name, inside angle
 * brackets or inside a comment, which is where a real name puts them.
 */
export function parseAddressList(value: string): MailAddress[] {
  const out: MailAddress[] = [];
  let part = "";
  let quoted = false;
  let escaped = false;
  let angle = 0;
  let comment = 0;
  const flush = () => {
    const entry = parseAddress(part);
    if (entry) out.push(entry);
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
      else if (comment === 0 && char === "<") angle += 1;
      else if (comment === 0 && char === ">" && angle > 0) angle -= 1;
      else if (comment === 0 && angle === 0 && (char === "," || char === ";")) {
        flush();
        continue;
      }
    }
    part += char;
  }
  flush();
  return out;
}

/** parseAddress reads one entry; it answers null for whitespace. */
export function parseAddress(value: string): MailAddress | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  const angled = trimmed.match(/^(.*)<([^<>]*)>\s*$/s);
  if (angled) {
    return { name: cleanName(angled[1]), email: normalizeEmail(angled[2]) };
  }
  const bare = trimmed.match(/^([^\s<>"(),;]+@[^\s<>"(),;]+)$/);
  if (bare) return { name: "", email: normalizeEmail(bare[1]) };
  return { name: cleanName(trimmed), email: "" };
}

function cleanName(value: string): string {
  let name = value.replace(/\([^)]*\)/g, "").trim();
  if (name.startsWith('"') && name.endsWith('"') && name.length >= 2) {
    name = name.slice(1, -1).replace(/\\(.)/g, "$1");
  }
  return name.trim();
}

function normalizeEmail(value: string): string {
  return value.trim().toLowerCase();
}

/**
 * formatAddress writes an entry back as a line the composer's To field
 * accepts, quoting the name when it carries anything a bare phrase may not.
 */
export function formatAddress(address: MailAddress): string {
  const email = address.email.trim();
  const name = address.name.trim();
  if (!email) return name;
  if (!name || name.toLowerCase() === email.toLowerCase()) return email;
  if (/^[\p{L}\p{N} .'_-]+$/u.test(name)) return `${name} <${email}>`;
  return `"${name.replace(/(["\\])/g, "\\$1")}" <${email}>`;
}

/** sameEmail compares two addresses the way the address book does. */
export function sameEmail(left: string, right: string): boolean {
  const a = left.trim().toLowerCase();
  return a !== "" && a === right.trim().toLowerCase();
}
