package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// stripMetadata copies src without its metadata (EXIF, XMP, IPTC, comments
// and any data after the image), byte for byte otherwise: the pixels are not
// re-encoded. The colour profile stays, and an orientation other than 1 is
// kept as a minimal EXIF block holding only it.
func stripMetadata(src []byte, contentType string, orientation int) ([]byte, error) {
	var out []byte
	var err error
	switch contentType {
	case "image/jpeg":
		out, err = stripJPEG(src, orientation)
	case "image/png":
		out, err = stripPNG(src, orientation)
	case "image/webp":
		out, err = stripWebP(src, orientation)
	case "image/gif":
		out, err = stripGIF(src)
	default:
		return nil, fmt.Errorf("metadata cannot be removed from %s without re-encoding", contentType)
	}
	if err != nil {
		return nil, fmt.Errorf("remove %s metadata: %w", contentType, err)
	}
	return out, nil
}

var errTruncated = errors.New("truncated")

// orientationTIFF is a big-endian TIFF header and one IFD holding only the
// Orientation tag.
func orientationTIFF(orientation int) []byte {
	b := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(b[18:], uint16(orientation))
	return b
}

func keepsOrientation(o int) bool { return o >= 2 && o <= 8 }

// stripJPEG keeps every marker segment but APPn and COM, except JFIF/JFXX
// (APP0), ICC profiles (APP2) and Adobe's colour transform (APP14).
func stripJPEG(src []byte, orientation int) ([]byte, error) {
	if len(src) < 4 || src[0] != 0xFF || src[1] != 0xD8 {
		return nil, errors.New("no SOI")
	}
	out := make([]byte, 0, len(src))
	out = append(out, 0xFF, 0xD8)
	if keepsOrientation(orientation) {
		exif := append([]byte("Exif\x00\x00"), orientationTIFF(orientation)...)
		out = append(out, 0xFF, 0xE1, byte((len(exif)+2)>>8), byte(len(exif)+2))
		out = append(out, exif...)
	}
	i := 2
	scan := false
	for i < len(src) {
		if scan {
			// Entropy-coded data: copy to the next marker that is not a
			// stuffed 0xFF00 or a restart.
			start := i
			for i+1 < len(src) && !(src[i] == 0xFF && src[i+1] != 0 && (src[i+1] < 0xD0 || src[i+1] > 0xD7)) {
				i++
			}
			if i+1 >= len(src) {
				return nil, errTruncated
			}
			out = append(out, src[start:i]...)
			scan = false
			continue
		}
		if src[i] != 0xFF {
			return nil, fmt.Errorf("no marker at %d", i)
		}
		for i < len(src) && src[i] == 0xFF { // fill bytes
			i++
		}
		if i >= len(src) {
			return nil, errTruncated
		}
		marker := src[i]
		i++
		if marker == 0xD9 { // EOI: anything after it is dropped
			return append(out, 0xFF, 0xD9), nil
		}
		if marker >= 0xD0 && marker <= 0xD7 || marker == 0x01 {
			out = append(out, 0xFF, marker)
			continue
		}
		if i+2 > len(src) {
			return nil, errTruncated
		}
		n := int(binary.BigEndian.Uint16(src[i:]))
		if n < 2 || i+n > len(src) {
			return nil, errTruncated
		}
		segment := src[i : i+n]
		i += n
		if marker >= 0xE0 && marker <= 0xEF || marker == 0xFE {
			body := segment[2:]
			keep := marker == 0xE0 && (bytes.HasPrefix(body, []byte("JFIF\x00")) || bytes.HasPrefix(body, []byte("JFXX\x00"))) ||
				marker == 0xE2 && bytes.HasPrefix(body, []byte("ICC_PROFILE\x00")) ||
				marker == 0xEE && bytes.HasPrefix(body, []byte("Adobe"))
			if !keep {
				continue
			}
		}
		out = append(out, 0xFF, marker)
		out = append(out, segment...)
		scan = marker == 0xDA // SOS: its scan data follows
	}
	return nil, errTruncated
}

// pngKept are the ancillary chunks a PNG keeps: rendering, colour and
// animation. Text, time, EXIF and private chunks go.
var pngKept = map[string]bool{"tRNS": true, "gAMA": true, "cHRM": true, "sRGB": true, "iCCP": true, "sBIT": true, "bKGD": true,
	"pHYs": true, "sPLT": true, "hIST": true, "acTL": true, "fcTL": true, "fdAT": true, "cICP": true, "mDCV": true, "cLLI": true}

func stripPNG(src []byte, orientation int) ([]byte, error) {
	const signature = "\x89PNG\r\n\x1a\n"
	if !bytes.HasPrefix(src, []byte(signature)) {
		return nil, errors.New("no signature")
	}
	out := append(make([]byte, 0, len(src)), signature...)
	for i := len(signature); ; {
		if i+12 > len(src) {
			return nil, errTruncated
		}
		n := int(binary.BigEndian.Uint32(src[i:]))
		if n < 0 || i+12+n > len(src) {
			return nil, errTruncated
		}
		typ := string(src[i+4 : i+8])
		chunk := src[i : i+12+n]
		i += 12 + n
		if typ[0] >= 'A' && typ[0] <= 'Z' || pngKept[typ] {
			if typ == "IDAT" && keepsOrientation(orientation) {
				out = appendPNGChunk(out, "eXIf", orientationTIFF(orientation)) // must precede the image data
				orientation = 0
			}
			out = append(out, chunk...)
		}
		if typ == "IEND" {
			return out, nil
		}
	}
}

func appendPNGChunk(out []byte, typ string, data []byte) []byte {
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	start := len(out)
	out = append(out, typ...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[start:]))
}

// stripWebP drops the EXIF and XMP chunks of an extended (VP8X) WebP and
// clears their flags; a simple one carries none.
func stripWebP(src []byte, orientation int) ([]byte, error) {
	if len(src) < 12 || string(src[:4]) != "RIFF" || string(src[8:12]) != "WEBP" {
		return nil, errors.New("no RIFF WEBP header")
	}
	size := int(binary.LittleEndian.Uint32(src[4:]))
	if size < 4 || 8+size > len(src) {
		return nil, errTruncated
	}
	body := src[12 : 8+size] // bytes past the RIFF size are not the image's
	out := append(make([]byte, 0, len(src)), src[:12]...)
	vp8x := -1
	for i := 0; i < len(body); {
		if i+8 > len(body) {
			return nil, errTruncated
		}
		fourcc := string(body[i : i+4])
		n := int(binary.LittleEndian.Uint32(body[i+4:]))
		end := i + 8 + n + n&1
		if n < 0 || end > len(body) {
			if i+8+n != len(body) { // an unpadded last chunk
				return nil, errTruncated
			}
			end = len(body)
		}
		switch fourcc {
		case "EXIF", "XMP ":
		case "VP8X":
			if n < 10 {
				return nil, errTruncated
			}
			vp8x = len(out)
			fallthrough
		default:
			out = append(out, body[i:end]...)
		}
		i = end
	}
	if vp8x >= 0 {
		out[vp8x+8] &^= 0x08 | 0x04 // EXIF and XMP flags
		if keepsOrientation(orientation) {
			out[vp8x+8] |= 0x08
			if len(out)%2 == 1 { // pad an unpadded last chunk
				out = append(out, 0)
			}
			tiff := orientationTIFF(orientation)
			out = append(out, "EXIF"...)
			out = binary.LittleEndian.AppendUint32(out, uint32(len(tiff)))
			out = append(out, tiff...)
		}
	}
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out, nil
}

// gifKept are the application extensions a GIF keeps: its loop count and
// colour profile.
var gifKept = []string{"NETSCAPE2.0", "ANIMEXTS1.0", "ICCRGBG1012"}

// stripGIF drops comment and other application extensions (XMP among them).
func stripGIF(src []byte) ([]byte, error) {
	if len(src) < 13 || string(src[:3]) != "GIF" {
		return nil, errors.New("no GIF header")
	}
	i := 13
	if flags := src[10]; flags&0x80 != 0 {
		i += 3 << (flags&7 + 1)
	}
	if i > len(src) {
		return nil, errTruncated
	}
	out := append(make([]byte, 0, len(src)), src[:i]...)
	// subBlocks is the end of the data sub-blocks starting at j.
	subBlocks := func(j int) (int, error) {
		for {
			if j >= len(src) {
				return 0, errTruncated
			}
			n := int(src[j])
			j++
			if n == 0 {
				return j, nil
			}
			j += n
		}
	}
	for i < len(src) {
		switch src[i] {
		case 0x3B: // trailer: anything after it is dropped
			return append(out, 0x3B), nil
		case 0x2C: // image descriptor, local colour table, LZW data
			j := i + 10
			if j > len(src) {
				return nil, errTruncated
			}
			if flags := src[i+9]; flags&0x80 != 0 {
				j += 3 << (flags&7 + 1)
			}
			j++ // LZW minimum code size
			end, err := subBlocks(j)
			if err != nil {
				return nil, err
			}
			out = append(out, src[i:end]...)
			i = end
		case 0x21:
			if i+2 > len(src) {
				return nil, errTruncated
			}
			label := src[i+1]
			end, err := subBlocks(i + 2)
			if err != nil {
				return nil, err
			}
			keep := label == 0xF9 || label == 0x01 // graphic control, plain text
			if label == 0xFF && i+3 < len(src) && int(src[i+2]) == 11 && i+14 <= len(src) {
				for _, app := range gifKept {
					keep = keep || string(src[i+3:i+14]) == app
				}
			}
			if keep {
				out = append(out, src[i:end]...)
			}
			i = end
		default:
			return nil, fmt.Errorf("unknown block 0x%02x at %d", src[i], i)
		}
	}
	return nil, errTruncated
}
