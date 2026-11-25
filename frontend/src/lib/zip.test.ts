import { inflateRawSync, crc32 as nodeCRC32 } from "node:zlib";
import { describe, expect, it } from "vitest";
import { crc32, zipArchiveName, zipEntries, zipFile, zipSupported } from "./zip";

async function bytesOf(blob: Blob): Promise<Uint8Array> {
  return new Uint8Array(await blob.arrayBuffer());
}

// readZip walks the central directory the way an unzip tool does, so the test
// checks what a reader of the archive finds rather than what the writer meant.
function readZip(bytes: Uint8Array): { name: string; content: Uint8Array; crc: number; flags: number }[] {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const end = bytes.length - 22;
  expect(view.getUint32(end, true)).toBe(0x06054b50);
  const count = view.getUint16(end + 10, true);
  let cursor = view.getUint32(end + 16, true);
  const entries = [];
  for (let index = 0; index < count; index += 1) {
    expect(view.getUint32(cursor, true)).toBe(0x02014b50);
    const flags = view.getUint16(cursor + 8, true);
    const crc = view.getUint32(cursor + 16, true);
    const compressedSize = view.getUint32(cursor + 20, true);
    const size = view.getUint32(cursor + 24, true);
    const nameLength = view.getUint16(cursor + 28, true);
    const localOffset = view.getUint32(cursor + 42, true);
    const name = new TextDecoder().decode(bytes.subarray(cursor + 46, cursor + 46 + nameLength));
    expect(view.getUint32(localOffset, true)).toBe(0x04034b50);
    const localNameLength = view.getUint16(localOffset + 26, true);
    const dataStart = localOffset + 30 + localNameLength + view.getUint16(localOffset + 28, true);
    const content = new Uint8Array(inflateRawSync(bytes.subarray(dataStart, dataStart + compressedSize)));
    expect(content.length).toBe(size);
    entries.push({ name, content, crc, flags });
    cursor += 46 + nameLength;
  }
  return entries;
}

describe("zip", () => {
  it("computes the standard CRC-32", () => {
    const sample = new TextEncoder().encode("The quick brown fox jumps over the lazy dog");
    expect(crc32(sample)).toBe(0x414fa339);
    expect(crc32(sample)).toBe(nodeCRC32(sample));
  });

  it("names the archive after the file", () => {
    expect(zipArchiveName("Vertrag.pdf")).toBe("Vertrag.zip");
    expect(zipArchiveName("archive.tar.gz")).toBe("archive.tar.zip");
    expect(zipArchiveName(".env")).toBe(".env.zip");
    expect(zipArchiveName("README")).toBe("README.zip");
    expect(zipArchiveName("")).toBe("attachment.zip");
  });

  it("writes an archive an unzip tool reads back", async () => {
    expect(zipSupported()).toBe(true);
    const text = "Rechnung ".repeat(20_000);
    const file = new File([text], "Größenübersicht.txt", { type: "text/plain", lastModified: Date.UTC(2026, 10, 25, 12) });
    const archive = await zipFile(file);
    expect(archive.name).toBe("Größenübersicht.zip");
    expect(archive.type).toBe("application/zip");
    expect(archive.size).toBeLessThan(file.size / 10);

    const entries = readZip(await bytesOf(archive));
    expect(entries).toHaveLength(1);
    expect(entries[0].name).toBe("Größenübersicht.txt");
    expect(entries[0].flags & 0x0800).toBe(0x0800);
    expect(new TextDecoder().decode(entries[0].content)).toBe(text);
    expect(entries[0].crc).toBe(nodeCRC32(entries[0].content));
  });

  it("keeps several entries and their offsets apart", async () => {
    const random = new Uint8Array(70_000);
    for (let index = 0; index < random.length; index += 1) random[index] = (index * 2654435761) >>> 24;
    const archive = await zipEntries([
      { name: "a.bin", data: new Blob([random]) },
      { name: "empty.txt", data: new Blob([]) },
      { name: "b.txt", data: new Blob(["hello"]) }
    ], "bundle.zip");
    const entries = readZip(await bytesOf(archive));
    expect(entries.map((entry) => entry.name)).toEqual(["a.bin", "empty.txt", "b.txt"]);
    expect(entries[0].content).toEqual(random);
    expect(entries[1].content.length).toBe(0);
    expect(new TextDecoder().decode(entries[2].content)).toBe("hello");
  });
});
