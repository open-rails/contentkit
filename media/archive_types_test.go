package media

import (
	"encoding/binary"
	"testing"
)

func TestArchiveRecognizesAVIFAndHEIFContainers(t *testing.T) {
	for _, tc := range []struct{ brand, want string }{{"avif", "image/avif"}, {"avis", "image/avif"}, {"heic", "image/heic"}, {"mif1", "image/heif"}} {
		body := make([]byte, 24)
		binary.BigEndian.PutUint32(body, 24)
		copy(body[4:8], "ftyp")
		copy(body[8:12], tc.brand)
		copy(body[16:20], "mif1")
		if got := archiveContentType(body); got != tc.want {
			t.Fatalf("%s container=%q want=%q", tc.brand, got, tc.want)
		}
	}
	malformed := []byte{0, 0, 0, 4, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f', 0, 0, 0, 0}
	if got := archiveContentType(malformed); got == "image/avif" {
		t.Fatal("malformed file-type box accepted as AVIF")
	}
}
