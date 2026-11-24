package image

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"

	"github.com/davidbyttow/govips/v2/vips"

	"github.com/open-rails/contentkit/media"
)

// secret is metadata no copy may keep: GPS coordinates, an author, a comment.
const secret = "GPS 48.8584N 2.2945E by someone"

// exifWith is an EXIF TIFF block with an Orientation and an ImageDescription
// holding secret.
func exifWith(orientation int) []byte {
	desc := append([]byte(secret), 0)
	b := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 2}
	b = append(b, 0x01, 0x0E, 0, 2) // ImageDescription, ASCII
	b = binary.BigEndian.AppendUint32(b, uint32(len(desc)))
	b = binary.BigEndian.AppendUint32(b, 8+2+24+4)
	b = append(b, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, byte(orientation), 0, 0) // Orientation
	b = append(b, 0, 0, 0, 0)
	return append(b, desc...)
}

func segment(marker byte, body []byte) []byte {
	return append([]byte{0xFF, marker, byte((len(body) + 2) >> 8), byte(len(body) + 2)}, body...)
}

func pixels(t *testing.T, b []byte) []byte {
	t.Helper()
	img, err := vips.NewImageFromBuffer(b)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	out, err := img.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func orientationOf(t *testing.T, b []byte) int {
	t.Helper()
	img, err := vips.NewImageFromBuffer(b)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	return img.Orientation()
}

func requireStripped(t *testing.T, name string, src, out []byte) {
	t.Helper()
	for _, leak := range []string{secret, "XMP", "xmpmeta", "trailing"} {
		if bytes.Contains(out, []byte(leak)) {
			t.Fatalf("%s copy kept %q", name, leak)
		}
	}
	if !bytes.Equal(pixels(t, src), pixels(t, out)) {
		t.Fatalf("%s copy changed its pixels", name)
	}
}

func TestStripJPEGMetadata(t *testing.T) {
	plain := jpegOf(t, halves(64, 32))
	src := append([]byte{0xFF, 0xD8}, segment(0xE1, append([]byte("Exif\x00\x00"), exifWith(6)...))...)
	src = append(src, segment(0xE1, []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta>"+secret+"</x:xmpmeta>"))...)
	src = append(src, segment(0xED, []byte("Photoshop 3.0\x00"+secret))...)
	src = append(src, segment(0xFE, []byte(secret))...)
	src = append(src, segment(0xE2, []byte("ICC_PROFILE\x00\x01\x01profile"))...)
	src = append(src, plain[2:]...)
	src = append(src, "trailing motion-photo video"...)
	if orientationOf(t, src) != 6 {
		t.Fatal("fixture lacks its orientation")
	}
	out, err := stripMetadata(src, "image/jpeg", 6)
	if err != nil {
		t.Fatal(err)
	}
	requireStripped(t, "JPEG", src, out)
	if orientationOf(t, out) != 6 || !bytes.Contains(out, []byte("ICC_PROFILE")) {
		t.Fatal("JPEG copy lost its orientation or colour profile")
	}
	if !bytes.HasSuffix(out, []byte{0xFF, 0xD9}) {
		t.Fatal("JPEG copy does not end at its EOI")
	}
	// The source-format no-op serves the stripped copy; KeepMetadata the original.
	info := probed(t, src, "image/jpeg")
	copied, d, err := encode(src, "image/jpeg", info, media.Image{Format: media.ImageFormatSource}, nil, limits)
	if err != nil || !bytes.Equal(copied, out) || d != (media.Dims{W: 32, H: 64}) {
		t.Fatalf("source-format copy is not the stripped original (%+v, %v)", d, err)
	}
	kept, _, err := encode(src, "image/jpeg", info, media.Image{Format: media.ImageFormatSource, KeepMetadata: true}, nil, limits)
	if err != nil || !bytes.Equal(kept, src) {
		t.Fatalf("KeepMetadata changed the copy: %v", err)
	}
}

func pngChunk(typ string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(out, typ...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[4:]))
}

func TestStripPNGMetadata(t *testing.T) {
	plain := pngOf(t, halves(64, 32))
	ihdrEnd := 8 + 12 + 13
	src := append([]byte{}, plain[:ihdrEnd]...)
	src = append(src, pngChunk("tEXt", []byte("Author\x00"+secret))...)
	src = append(src, pngChunk("iTXt", []byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00<x:xmpmeta>"+secret))...)
	src = append(src, pngChunk("eXIf", exifWith(3))...)
	src = append(src, pngChunk("gAMA", []byte{0, 1, 0x86, 0xA0})...)
	src = append(src, plain[ihdrEnd:]...)
	src = append(src, "trailing"...)
	out, err := stripMetadata(src, "image/png", 3)
	if err != nil {
		t.Fatal(err)
	}
	requireStripped(t, "PNG", src, out)
	if orientationOf(t, out) != 3 || !bytes.Contains(out, []byte("gAMA")) {
		t.Fatal("PNG copy lost its orientation or gamma")
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("PNG copy does not decode: %v", err)
	}
}

func TestStripWebPMetadata(t *testing.T) {
	img, err := vips.NewImageFromBuffer(pngOf(t, halves(64, 32)))
	if err != nil {
		t.Fatal(err)
	}
	p := vips.NewWebpExportParams()
	p.Lossless, p.StripMetadata = true, true
	simple, _, err := img.ExportWebp(p)
	img.Close()
	if err != nil {
		t.Fatal(err)
	}
	chunk := func(fourcc string, data []byte) []byte {
		out := append([]byte(fourcc), binary.LittleEndian.AppendUint32(nil, uint32(len(data)))...)
		out = append(out, data...)
		if len(data)%2 == 1 {
			out = append(out, 0)
		}
		return out
	}
	vp8x := []byte{0x08 | 0x04, 0, 0, 0, 63, 0, 0, 31, 0, 0} // EXIF and XMP, 64x32
	body := []byte("WEBP")
	body = append(body, chunk("VP8X", vp8x)...)
	body = append(body, simple[12:]...) // its VP8L chunk
	body = append(body, chunk("EXIF", exifWith(8))...)
	body = append(body, chunk("XMP ", []byte("<x:xmpmeta>"+secret+"</x:xmpmeta>"))...)
	src := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	src = append(src, body...)
	src = append(src, "trailing"...)
	out, err := stripMetadata(src, "image/webp", 8)
	if err != nil {
		t.Fatal(err)
	}
	requireStripped(t, "WebP", src, out)
	if orientationOf(t, out) != 8 || out[20]&0x04 != 0 || int(binary.LittleEndian.Uint32(out[4:]))+8 != len(out) {
		t.Fatalf("WebP copy lost its orientation, kept its XMP flag or has a wrong RIFF size (flags %#x)", out[20])
	}
	if got, err := stripMetadata(simple, "image/webp", 0); err != nil || !bytes.Equal(got, simple) {
		t.Fatalf("a simple WebP changed: %v", err)
	}
}

func TestStripGIFMetadata(t *testing.T) {
	palette := []color.Color{color.Black, color.White}
	anim := &gif.GIF{LoopCount: 3}
	for i := range 3 {
		frame := image.NewPaletted(image.Rect(0, 0, 8, 8), palette)
		frame.SetColorIndex(i, i, 1)
		anim.Image, anim.Delay = append(anim.Image, frame), append(anim.Delay, 5)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, anim); err != nil {
		t.Fatal(err)
	}
	plain := b.Bytes()
	header := 13
	if plain[10]&0x80 != 0 {
		header += 3 << (plain[10]&7 + 1)
	}
	comment := append([]byte{0x21, 0xFE, byte(len(secret))}, secret...)
	comment = append(comment, 0)
	xmp := append([]byte{0x21, 0xFF, 11}, "XMP DataXMP"...)
	xmp = append(xmp, byte(len(secret)))
	xmp = append(xmp, secret...)
	xmp = append(xmp, 0)
	src := append(append(append([]byte{}, plain[:header]...), comment...), xmp...)
	src = append(src, plain[header:len(plain)-1]...)
	src = append(src, 0x3B)
	src = append(src, "trailing"...)
	out, err := stripMetadata(src, "image/gif", 0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(secret)) || bytes.Contains(out, []byte("XMP")) || bytes.Contains(out, []byte("trailing")) {
		t.Fatal("GIF copy kept its metadata")
	}
	decoded, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil || len(decoded.Image) != 3 || decoded.LoopCount != 3 {
		t.Fatalf("GIF copy lost frames or its loop: %v", err)
	}
	for i, frame := range decoded.Image {
		if !bytes.Equal(frame.Pix, anim.Image[i].Pix) {
			t.Fatalf("GIF frame %d changed", i)
		}
	}
}
