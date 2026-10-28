package mediastore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	media "github.com/open-rails/contentkit/media"
)

const (
	archiveDirectoryEntries = uint64(4096)
)

const (
	archiveDirectoryBytes = uint64(8 << 20)
)

const (
	zipDirectorySignature = uint32(0x02014b50)
)

const (
	zipEndSignature = uint32(0x06054b50)
)

const (
	zip64EndSignature = uint32(0x06064b50)
)

const (
	zip64LocatorSignature = uint32(0x07064b50)
)

// validateArchiveDirectory bounds metadata before archive/zip allocates its
// file list. Limits cover every entry, including ignored files and directories.
// Directory offsets must describe one contiguous single-disk directory ending
// at its end record; ambiguous offsets and trailing data are refused.
func validateArchiveDirectory(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 22 {
		return badArchiveDirectory("missing end record")
	}
	size := uint64(info.Size())
	tail := make([]byte, min(size, uint64(22+65535)))
	if err := archiveDirectoryRead(file, tail, size-uint64(len(tail)), size); err != nil {
		return err
	}
	end := -1
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != zipEndSignature {
			continue
		}
		if int(binary.LittleEndian.Uint16(tail[i+20:])) != len(tail)-i-22 {
			return badArchiveDirectory("truncated comment or trailing data")
		}
		end = i
		break
	}
	if end < 0 {
		return badArchiveDirectory("missing end record")
	}
	b := tail[end:]
	endAt := size - uint64(len(tail)) + uint64(end)
	disk, startDisk := uint64(binary.LittleEndian.Uint16(b[4:])), uint64(binary.LittleEndian.Uint16(b[6:]))
	onDisk, count := uint64(binary.LittleEndian.Uint16(b[8:])), uint64(binary.LittleEndian.Uint16(b[10:]))
	length, offset := uint64(binary.LittleEndian.Uint32(b[12:])), uint64(binary.LittleEndian.Uint32(b[16:]))
	needs64 := onDisk == 0xffff || count == 0xffff || length == 0xffffffff || offset == 0xffffffff
	use64 := needs64
	var locator [20]byte
	// archive/zip also probes ZIP64 when the legacy directory length is
	// exactly 0xffff. Follow that branch so a locator hidden in a comment
	// cannot make the downstream reader choose different, unbounded counts.
	if needs64 || length == 0xffff {
		if endAt < 20 {
			return badArchiveDirectory("missing ZIP64 locator")
		}
		if err := archiveDirectoryRead(file, locator[:], endAt-20, size); err != nil {
			return err
		}
		use64 = binary.LittleEndian.Uint32(locator[:]) == zip64LocatorSignature
		if needs64 && !use64 {
			return badArchiveDirectory("missing ZIP64 locator")
		}
	}
	if use64 {
		if binary.LittleEndian.Uint32(locator[4:]) != 0 || binary.LittleEndian.Uint32(locator[16:]) != 1 {
			return badArchiveDirectory("multi-disk ZIP64 archive")
		}
		recordAt := binary.LittleEndian.Uint64(locator[8:])
		var record [56]byte
		if err := archiveDirectoryRead(file, record[:], recordAt, endAt-20); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(record[:]) != zip64EndSignature {
			return badArchiveDirectory("invalid ZIP64 end record")
		}
		recordSize := binary.LittleEndian.Uint64(record[4:])
		// Subtraction after the checked fixed read avoids offset+size overflow.
		if recordSize < 44 || recordSize != endAt-20-recordAt-12 {
			return badArchiveDirectory("invalid ZIP64 end-record bounds")
		}
		if binary.LittleEndian.Uint32(record[16:]) != 0 || binary.LittleEndian.Uint32(record[20:]) != 0 ||
			(disk != 0 && disk != 0xffff) || (startDisk != 0 && startDisk != 0xffff) {
			return badArchiveDirectory("multi-disk ZIP64 archive")
		}
		a, n := binary.LittleEndian.Uint64(record[24:]), binary.LittleEndian.Uint64(record[32:])
		l, o := binary.LittleEndian.Uint64(record[40:]), binary.LittleEndian.Uint64(record[48:])
		if onDisk != 0xffff && onDisk != a || count != 0xffff && count != n ||
			length != 0xffffffff && length != l || offset != 0xffffffff && offset != o {
			return badArchiveDirectory("inconsistent ZIP64 end records")
		}
		onDisk, count, length, offset, endAt = a, n, l, o, recordAt
	} else if disk != 0 || startDisk != 0 {
		return badArchiveDirectory("multi-disk archive")
	}
	if onDisk != count {
		return badArchiveDirectory("entry counts span multiple disks")
	}
	if count > archiveDirectoryEntries || length > archiveDirectoryBytes {
		return uploadErr(media.CodeTooLarge, "ZIP directory exceeds %d entries or %d metadata bytes", archiveDirectoryEntries, archiveDirectoryBytes)
	}
	if !archiveDirectoryWithin(offset, length, endAt) || length != endAt-offset {
		return badArchiveDirectory("directory offset or length is outside its end record")
	}
	if count > length/46 {
		return badArchiveDirectory("entry count exceeds directory bounds")
	}
	pos := offset
	var actual uint64
	for pos < endAt {
		var header [46]byte
		if err := archiveDirectoryRead(file, header[:4], pos, endAt); err != nil {
			return err
		}
		// The optional central-directory digital signature follows its entries.
		if binary.LittleEndian.Uint32(header[:]) == 0x05054b50 {
			if err := archiveDirectoryRead(file, header[:6], pos, endAt); err != nil {
				return err
			}
			if uint64(binary.LittleEndian.Uint16(header[4:]))+6 != endAt-pos {
				return badArchiveDirectory("invalid directory signature bounds")
			}
			pos = endAt
			break
		}
		if binary.LittleEndian.Uint32(header[:]) != zipDirectorySignature {
			return badArchiveDirectory("invalid directory entry")
		}
		if err := archiveDirectoryRead(file, header[:], pos, endAt); err != nil {
			return err
		}
		actual++
		if actual > archiveDirectoryEntries {
			return uploadErr(media.CodeTooLarge, "ZIP directory exceeds %d entries", archiveDirectoryEntries)
		}
		nameLen := uint64(binary.LittleEndian.Uint16(header[28:]))
		extraLen := uint64(binary.LittleEndian.Uint16(header[30:]))
		commentLen := uint64(binary.LittleEndian.Uint16(header[32:]))
		entrySize := uint64(46) + nameLen + extraLen + commentLen
		if !archiveDirectoryWithin(pos, entrySize, endAt) {
			return badArchiveDirectory("truncated directory entry")
		}
		extra := make([]byte, extraLen)
		if err := archiveDirectoryRead(file, extra, pos+46+nameLen, endAt); err != nil {
			return err
		}
		if err := archiveDirectoryEntry(file, header[:], extra, offset); err != nil {
			return err
		}
		pos += entrySize
	}
	if actual != count {
		return badArchiveDirectory("declared and actual entry counts differ")
	}
	return nil
}

// archiveDirectoryEntry validates disk and local-data bounds, including the
// conditional order of fields in each entry's ZIP64 extra data.
func archiveDirectoryEntry(file *os.File, header, extra []byte, directoryAt uint64) error {
	compressed := uint64(binary.LittleEndian.Uint32(header[20:]))
	uncompressed := uint64(binary.LittleEndian.Uint32(header[24:]))
	offset := uint64(binary.LittleEndian.Uint32(header[42:]))
	disk := uint32(binary.LittleEndian.Uint16(header[34:]))
	needs64 := compressed == 0xffffffff || uncompressed == 0xffffffff || offset == 0xffffffff || disk == 0xffff
	var wide []byte
	for len(extra) > 0 {
		if len(extra) < 4 {
			return badArchiveDirectory("truncated entry extra field")
		}
		kind, length := binary.LittleEndian.Uint16(extra), int(binary.LittleEndian.Uint16(extra[2:]))
		extra = extra[4:]
		if length > len(extra) {
			return badArchiveDirectory("truncated entry extra data")
		}
		if kind == 1 {
			if wide != nil {
				return badArchiveDirectory("duplicate ZIP64 extra field")
			}
			wide = extra[:length]
		}
		extra = extra[length:]
	}
	if needs64 {
		for _, value := range []*uint64{&uncompressed, &compressed, &offset} {
			if *value != 0xffffffff {
				continue
			}
			if len(wide) < 8 {
				return badArchiveDirectory("missing ZIP64 entry data")
			}
			*value, wide = binary.LittleEndian.Uint64(wide), wide[8:]
		}
		if disk == 0xffff {
			if len(wide) < 4 {
				return badArchiveDirectory("missing ZIP64 disk number")
			}
			disk = binary.LittleEndian.Uint32(wide)
		}
	}
	if disk != 0 {
		return badArchiveDirectory("multi-disk directory entry")
	}
	var local [30]byte
	if err := archiveDirectoryRead(file, local[:], offset, directoryAt); err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(local[:]) != 0x04034b50 {
		return badArchiveDirectory("invalid local-file offset")
	}
	headerSize := uint64(30) + uint64(binary.LittleEndian.Uint16(local[26:])) + uint64(binary.LittleEndian.Uint16(local[28:]))
	if !archiveDirectoryWithin(offset, headerSize, directoryAt) || !archiveDirectoryWithin(offset+headerSize, compressed, directoryAt) {
		return badArchiveDirectory("local-file data crosses directory bounds")
	}
	return nil
}

func archiveDirectoryWithin(offset, length, limit uint64) bool {
	return offset <= limit && length <= limit-offset
}

func archiveDirectoryRead(file *os.File, dst []byte, offset, limit uint64) error {
	if !archiveDirectoryWithin(offset, uint64(len(dst)), limit) {
		return badArchiveDirectory("metadata extends beyond archive bounds")
	}
	if len(dst) == 0 {
		return nil
	}
	_, err := file.ReadAt(dst, int64(offset))
	if errors.Is(err, io.EOF) {
		return badArchiveDirectory("truncated metadata")
	}
	if err != nil {
		return fmt.Errorf("media: read ZIP metadata: %w", err)
	}
	return nil
}

func badArchiveDirectory(reason string) error {
	return uploadErr(media.CodeInvalid, "invalid ZIP directory: %s", reason)
}
