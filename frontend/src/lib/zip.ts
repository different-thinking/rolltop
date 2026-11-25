// File overview: a minimal ZIP writer for the composer's "zip this large
// attachment" offer. It writes one deflated entry per file with the browser's
// own CompressionStream, so it needs no library and does the compression in
// native code rather than on the page's thread.
//
// What it deliberately does not do: ZIP64 (an entry or archive of 4 GiB or more
// is refused, and no mail provider accepts a message that size anyway),
// encryption, or directories.

const LOCAL_HEADER_SIGNATURE = 0x04034b50;
const CENTRAL_HEADER_SIGNATURE = 0x02014b50;
const END_OF_CENTRAL_DIRECTORY_SIGNATURE = 0x06054b50;
const VERSION_NEEDED = 20; // 2.0: deflate
const FLAG_UTF8_NAMES = 0x0800; // bit 11: names are UTF-8, so umlauts survive
const METHOD_DEFLATE = 8;
const ZIP32_LIMIT = 0xffffffff;

export type ZipEntry = {
  name: string;
  data: Blob;
  lastModified?: number;
};

/** zipSupported reports whether this browser can deflate a ZIP entry. */
export function zipSupported(): boolean {
  if (typeof CompressionStream === "undefined") return false;
  try {
    new CompressionStream("deflate-raw");
    return true;
  } catch {
    return false;
  }
}

/** zipArchiveName names the archive a single file is zipped into. */
export function zipArchiveName(filename: string): string {
  const name = (filename || "").trim() || "attachment";
  const dot = name.lastIndexOf(".");
  const base = dot > 0 ? name.slice(0, dot) : name;
  return `${base}.zip`;
}

/** zipFile packs one file into a ZIP archive of its own. */
export async function zipFile(file: File): Promise<File> {
  const name = file.name || "attachment";
  return zipEntries([{ name, data: file, lastModified: file.lastModified }], zipArchiveName(name));
}

/** zipEntries packs files into one ZIP archive. */
export async function zipEntries(entries: ZipEntry[], archiveName: string): Promise<File> {
  const encoder = new TextEncoder();
  const parts: BlobPart[] = [];
  const central: Uint8Array<ArrayBuffer>[] = [];
  let offset = 0;

  for (const entry of entries) {
    if (entry.data.size >= ZIP32_LIMIT) throw new Error(`${entry.name} is too large to zip.`);
    const name = encoder.encode(entry.name);
    const [crc, compressed] = await Promise.all([crc32OfBlob(entry.data), deflateBlob(entry.data)]);
    if (compressed.size >= ZIP32_LIMIT || offset >= ZIP32_LIMIT) throw new Error("The archive is too large.");
    const { time, date } = dosDateTime(entry.lastModified ?? Date.now());

    const local = new DataView(new ArrayBuffer(30));
    local.setUint32(0, LOCAL_HEADER_SIGNATURE, true);
    local.setUint16(4, VERSION_NEEDED, true);
    local.setUint16(6, FLAG_UTF8_NAMES, true);
    local.setUint16(8, METHOD_DEFLATE, true);
    local.setUint16(10, time, true);
    local.setUint16(12, date, true);
    local.setUint32(14, crc, true);
    local.setUint32(18, compressed.size, true);
    local.setUint32(22, entry.data.size, true);
    local.setUint16(26, name.length, true);
    local.setUint16(28, 0, true);
    parts.push(local.buffer, name, compressed);

    const header = new DataView(new ArrayBuffer(46));
    header.setUint32(0, CENTRAL_HEADER_SIGNATURE, true);
    header.setUint16(4, VERSION_NEEDED, true);
    header.setUint16(6, VERSION_NEEDED, true);
    header.setUint16(8, FLAG_UTF8_NAMES, true);
    header.setUint16(10, METHOD_DEFLATE, true);
    header.setUint16(12, time, true);
    header.setUint16(14, date, true);
    header.setUint32(16, crc, true);
    header.setUint32(20, compressed.size, true);
    header.setUint32(24, entry.data.size, true);
    header.setUint16(28, name.length, true);
    // extra length, comment length, disk number, internal and external
    // attributes stay zero.
    header.setUint32(42, offset, true);
    central.push(new Uint8Array(header.buffer), name);

    offset += 30 + name.length + compressed.size;
  }

  const centralSize = central.reduce((total, part) => total + part.length, 0);
  if (offset + centralSize >= ZIP32_LIMIT) throw new Error("The archive is too large.");
  const end = new DataView(new ArrayBuffer(22));
  end.setUint32(0, END_OF_CENTRAL_DIRECTORY_SIGNATURE, true);
  end.setUint16(8, entries.length, true);
  end.setUint16(10, entries.length, true);
  end.setUint32(12, centralSize, true);
  end.setUint32(16, offset, true);

  return new File([...parts, ...central, end.buffer], archiveName, { type: "application/zip" });
}

async function deflateBlob(blob: Blob): Promise<Blob> {
  const stream = blob.stream().pipeThrough(new CompressionStream("deflate-raw"));
  return new Response(stream).blob();
}

async function crc32OfBlob(blob: Blob): Promise<number> {
  const reader = blob.stream().getReader();
  let crc = 0xffffffff;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    crc = crc32Update(crc, value);
  }
  return (crc ^ 0xffffffff) >>> 0;
}

let crcTable: Uint32Array | null = null;

function crc32Update(crc: number, bytes: Uint8Array): number {
  const table = crcTable || (crcTable = buildCRCTable());
  let value = crc;
  for (let index = 0; index < bytes.length; index += 1) {
    value = table[(value ^ bytes[index]) & 0xff] ^ (value >>> 8);
  }
  return value;
}

function buildCRCTable(): Uint32Array {
  const table = new Uint32Array(256);
  for (let n = 0; n < 256; n += 1) {
    let c = n;
    for (let k = 0; k < 8; k += 1) {
      c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    }
    table[n] = c >>> 0;
  }
  return table;
}

/** CRC-32 of a byte array, exported for the tests. */
export function crc32(bytes: Uint8Array): number {
  return (crc32Update(0xffffffff, bytes) ^ 0xffffffff) >>> 0;
}

// ZIP stores local wall-clock time at two-second resolution and cannot say
// anything before 1980.
function dosDateTime(timestamp: number): { time: number; date: number } {
  const moment = new Date(timestamp);
  const year = moment.getFullYear();
  if (!Number.isFinite(year) || year < 1980) return { time: 0, date: (1 << 5) | 1 };
  const time = (moment.getHours() << 11) | (moment.getMinutes() << 5) | Math.floor(moment.getSeconds() / 2);
  const date = ((Math.min(year, 2107) - 1980) << 9) | ((moment.getMonth() + 1) << 5) | moment.getDate();
  return { time, date };
}
