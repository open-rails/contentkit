import { crc32, deflateSync } from "node:zlib";

/** Real distinct PNG pages in a stored ZIP, about 30 MB for 25 pages. */
export function pageArchive(count = 25): Buffer {
  const entries: Buffer[] = [], directory: Buffer[] = [];
  let offset = 0;
  for (let index = 0; index < count; index++) {
    const width = 640, height = 640;
    const pixels = Buffer.alloc((width * 3 + 1) * height);
    let seed = index + 1;
    for (let y = 0; y < height; y++) for (let x = 1; x <= width * 3; x++) {
      seed ^= seed << 13; seed ^= seed >>> 17; seed ^= seed << 5;
      pixels[y * (width * 3 + 1) + x] = seed & 255;
    }
    const ihdr = Buffer.alloc(13);
    ihdr.writeUInt32BE(width); ihdr.writeUInt32BE(height, 4); ihdr[8] = 8; ihdr[9] = 2;
    const chunk = (type: string, data: Buffer) => {
      const size = Buffer.alloc(4), sum = Buffer.alloc(4), name = Buffer.from(type);
      size.writeUInt32BE(data.length); sum.writeUInt32BE(crc32(Buffer.concat([name, data])));
      return Buffer.concat([size, name, data, sum]);
    };
    const image = Buffer.concat([Buffer.from("89504e470d0a1a0a", "hex"), chunk("IHDR", ihdr), chunk("IDAT", deflateSync(pixels)), chunk("IEND", Buffer.alloc(0))]);
    const name = Buffer.from(`${String(index + 1).padStart(3, "0")}.png`);
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50); local.writeUInt16LE(20, 4); local.writeUInt32LE(crc32(image), 14);
    local.writeUInt32LE(image.length, 18); local.writeUInt32LE(image.length, 22); local.writeUInt16LE(name.length, 26);
    entries.push(local, name, image);
    const central = Buffer.alloc(46);
    central.writeUInt32LE(0x02014b50); central.writeUInt16LE(20, 4); central.writeUInt16LE(20, 6); central.writeUInt32LE(crc32(image), 16);
    central.writeUInt32LE(image.length, 20); central.writeUInt32LE(image.length, 24); central.writeUInt16LE(name.length, 28); central.writeUInt32LE(offset, 42);
    directory.push(central, name);
    offset += local.length + name.length + image.length;
  }
  const index = Buffer.concat(directory), end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50); end.writeUInt16LE(count, 8); end.writeUInt16LE(count, 10); end.writeUInt32LE(index.length, 12); end.writeUInt32LE(offset, 16);
  return Buffer.concat([...entries, index, end]);
}
