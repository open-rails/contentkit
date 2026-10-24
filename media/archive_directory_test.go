package media

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func directoryTestZIP(t *testing.T, count int, comment string) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	if err := writer.SetComment(comment); err != nil {
		t.Fatal(err)
	}
	for i := range count {
		entry, err := writer.Create(fmt.Sprintf("__MACOSX/ignored-%04d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func checkDirectoryTest(t *testing.T, body []byte, code string) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "archive.zip")
	if err := os.WriteFile(filename, body, 0o600); err != nil {
		t.Fatal(err)
	}
	err := validateArchiveDirectory(filename)
	if code == "" {
		if err != nil {
			t.Fatal(err)
		}
		reader, err := zip.OpenReader(filename)
		if err != nil {
			t.Fatalf("valid fixture must also open with archive/zip: %v", err)
		}
		_ = reader.Close()
		return
	}
	var refusal *UploadError
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("expected typed %s refusal, got %v", code, err)
	}
}

func zip64DirectoryTest(t *testing.T) []byte {
	t.Helper()
	ordinary := directoryTestZIP(t, 1, "")
	end := ordinary[len(ordinary)-22:]
	prefix := ordinary[:len(ordinary)-22]
	record := make([]byte, 56)
	binary.LittleEndian.PutUint32(record, zip64EndSignature)
	binary.LittleEndian.PutUint64(record[4:], 44)
	binary.LittleEndian.PutUint16(record[12:], 45)
	binary.LittleEndian.PutUint16(record[14:], 45)
	binary.LittleEndian.PutUint64(record[24:], uint64(binary.LittleEndian.Uint16(end[8:])))
	binary.LittleEndian.PutUint64(record[32:], uint64(binary.LittleEndian.Uint16(end[10:])))
	binary.LittleEndian.PutUint64(record[40:], uint64(binary.LittleEndian.Uint32(end[12:])))
	binary.LittleEndian.PutUint64(record[48:], uint64(binary.LittleEndian.Uint32(end[16:])))
	locator := make([]byte, 20)
	binary.LittleEndian.PutUint32(locator, zip64LocatorSignature)
	binary.LittleEndian.PutUint64(locator[8:], uint64(len(prefix)))
	binary.LittleEndian.PutUint32(locator[16:], 1)
	end = bytes.Clone(end)
	binary.LittleEndian.PutUint16(end[8:], 0xffff)
	binary.LittleEndian.PutUint16(end[10:], 0xffff)
	binary.LittleEndian.PutUint32(end[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(end[16:], 0xffffffff)
	return bytes.Join([][]byte{prefix, record, locator, end}, nil)
}

func zip64EntryDirectoryTest(t *testing.T, mode string) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	extra := make([]byte, 4+24)
	binary.LittleEndian.PutUint16(extra, 1)
	binary.LittleEndian.PutUint16(extra[2:], 24)
	entry, err := writer.CreateHeader(&zip.FileHeader{Name: "page.png", Method: zip.Store, Extra: extra})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	out := body.Bytes()
	end := out[len(out)-22:]
	at := int(binary.LittleEndian.Uint32(end[16:]))
	header := out[at:]
	names := int(binary.LittleEndian.Uint16(header[28:]))
	wide := header[46+names+4:]
	binary.LittleEndian.PutUint64(wide, uint64(binary.LittleEndian.Uint32(header[24:])))
	binary.LittleEndian.PutUint64(wide[8:], uint64(binary.LittleEndian.Uint32(header[20:])))
	binary.LittleEndian.PutUint64(wide[16:], uint64(binary.LittleEndian.Uint32(header[42:])))
	binary.LittleEndian.PutUint32(header[24:], 0xffffffff)
	binary.LittleEndian.PutUint32(header[20:], 0xffffffff)
	binary.LittleEndian.PutUint32(header[42:], 0xffffffff)
	switch mode {
	case "offset-overflow":
		binary.LittleEndian.PutUint64(wide[16:], ^uint64(0))
	case "data-overflow":
		binary.LittleEndian.PutUint64(wide[8:], ^uint64(0))
	case "missing-wide-disk":
		binary.LittleEndian.PutUint16(header[34:], 0xffff)
	}
	return out
}

func directory65535Test(t *testing.T, hiddenLocator bool) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	comment := bytes.Repeat([]byte("a"), 65535-46-1)
	if hiddenLocator {
		// This is still one central entry, but Go's ZIP64 probe sees the
		// locator at the end of its comment. Never pass it to zip.OpenReader.
		binary.LittleEndian.PutUint32(comment[len(comment)-20:], zip64LocatorSignature)
		binary.LittleEndian.PutUint32(comment[len(comment)-16:], 0)
		binary.LittleEndian.PutUint64(comment[len(comment)-12:], ^uint64(0))
		binary.LittleEndian.PutUint32(comment[len(comment)-4:], 1)
	}
	entry, err := writer.CreateHeader(&zip.FileHeader{Name: "x", Comment: string(comment), Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	out := body.Bytes()
	if got := binary.LittleEndian.Uint32(out[len(out)-22+12:]); got != 65535 {
		t.Fatalf("fixture directory size=%d, want65535", got)
	}
	return out
}

func TestArchiveDirectoryAcceptsOrdinaryAndZIP64(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"ordinary", directoryTestZIP(t, 3, "")},
		{"empty", directoryTestZIP(t, 0, "")},
		{"long-comment", directoryTestZIP(t, 1, strings.Repeat("x", 65535))},
		{"ZIP64", zip64DirectoryTest(t)},
		{"ZIP64-entry", zip64EntryDirectoryTest(t, "valid")},
		{"65535-byte-directory", directory65535Test(t, false)},
	} {
		t.Run(test.name, func(t *testing.T) { checkDirectoryTest(t, test.body, "") })
	}
}

func TestArchiveDirectoryBoundsIgnoredEntriesAndMetadata(t *testing.T) {
	// Under one MiB, but all entries must count even though ingestion ignores
	// these names. No oversized fixture is needed to exercise metadata caps.
	many := directoryTestZIP(t, int(archiveDirectoryEntries)+1, "")
	checkDirectoryTest(t, many, CodeTooLarge)
	// A false small declared count must not hide the actual directory entries.
	binary.LittleEndian.PutUint16(many[len(many)-22+8:], 1)
	binary.LittleEndian.PutUint16(many[len(many)-22+10:], 1)
	checkDirectoryTest(t, many, CodeTooLarge)
	metadata := directoryTestZIP(t, 1, "")
	binary.LittleEndian.PutUint32(metadata[len(metadata)-22+12:], uint32(archiveDirectoryBytes+1))
	checkDirectoryTest(t, metadata, CodeTooLarge)
}

func TestArchiveDirectoryRejectsMalformedBoundsAndDisks(t *testing.T) {
	for _, name := range []string{"disk", "directory-disk", "disk-count", "count-mismatch", "directory-offset", "directory-signature", "entry-disk", "entry-offset", "entry-length", "truncated", "trailing-data"} {
		t.Run(name, func(t *testing.T) {
			body := directoryTestZIP(t, 2, "")
			end := body[len(body)-22:]
			at := int(binary.LittleEndian.Uint32(end[16:]))
			switch name {
			case "disk":
				binary.LittleEndian.PutUint16(end[4:], 1)
			case "directory-disk":
				binary.LittleEndian.PutUint16(end[6:], 1)
			case "disk-count":
				binary.LittleEndian.PutUint16(end[8:], 1)
			case "count-mismatch":
				binary.LittleEndian.PutUint16(end[8:], 1)
				binary.LittleEndian.PutUint16(end[10:], 1)
			case "directory-offset":
				binary.LittleEndian.PutUint32(end[16:], uint32(at-1))
			case "directory-signature":
				body[at] = 0
			case "entry-disk":
				binary.LittleEndian.PutUint16(body[at+34:], 1)
			case "entry-offset":
				binary.LittleEndian.PutUint32(body[at+42:], uint32(at))
			case "entry-length":
				binary.LittleEndian.PutUint16(body[at+30:], 65535)
			case "truncated":
				body = body[:len(body)-1]
			case "trailing-data":
				body = append(body, []byte("trailing")...)
			}
			checkDirectoryTest(t, body, CodeInvalid)
		})
	}
}

func TestArchiveDirectoryRejectsMaliciousZIP64(t *testing.T) {
	for _, name := range []string{"huge-count", "huge-directory", "offset-overflow", "record-size-overflow", "locator-overflow", "short-record", "locator-disks", "record-disk", "record-directory-disk", "count-mismatch", "missing-locator"} {
		t.Run(name, func(t *testing.T) {
			body := zip64DirectoryTest(t)
			recordAt := len(body) - 22 - 20 - 56
			record := body[recordAt:]
			locator := body[len(body)-22-20:]
			code := CodeInvalid
			switch name {
			case "huge-count":
				binary.LittleEndian.PutUint64(record[24:], ^uint64(0))
				binary.LittleEndian.PutUint64(record[32:], ^uint64(0))
				code = CodeTooLarge
			case "huge-directory":
				binary.LittleEndian.PutUint64(record[40:], ^uint64(0))
				code = CodeTooLarge
			case "offset-overflow":
				binary.LittleEndian.PutUint64(record[48:], ^uint64(0))
			case "record-size-overflow":
				binary.LittleEndian.PutUint64(record[4:], ^uint64(0))
			case "locator-overflow":
				binary.LittleEndian.PutUint64(locator[8:], ^uint64(0))
			case "short-record":
				binary.LittleEndian.PutUint64(record[4:], 43)
			case "locator-disks":
				binary.LittleEndian.PutUint32(locator[16:], 2)
			case "record-disk":
				binary.LittleEndian.PutUint32(record[16:], 1)
			case "record-directory-disk":
				binary.LittleEndian.PutUint32(record[20:], 1)
			case "count-mismatch":
				binary.LittleEndian.PutUint64(record[24:], 0)
			case "missing-locator":
				locator[0] = 0
			}
			checkDirectoryTest(t, body, code)
		})
	}
}

func TestArchiveDirectoryRejectsInvalidZIP64EntryData(t *testing.T) {
	for _, mode := range []string{"offset-overflow", "data-overflow", "missing-wide-disk"} {
		t.Run(mode, func(t *testing.T) { checkDirectoryTest(t, zip64EntryDirectoryTest(t, mode), CodeInvalid) })
	}
}

func TestArchiveDirectoryRejectsAlternateZIP64LocatorSelection(t *testing.T) {
	checkDirectoryTest(t, directory65535Test(t, true), CodeInvalid)
}
