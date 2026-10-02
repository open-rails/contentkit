package layout

import (
	"fmt"
	"mime"
	"path"
	"slices"
	"strings"
)

// MaxDownloadName bounds a download name in bytes.
const MaxDownloadName = 200

// downloadExts are the extensions a download name may end in, by the
// object's content type.
var downloadExts = map[string][]string{
	"application/zip": {".zip"},
	"application/pdf": {".pdf"},
	"video/mp4":       {".mp4"},
	"audio/mp4":       {".m4a", ".mp4"},
	"audio/mpeg":      {".mp3"},
	"image/webp":      {".webp"},
	"image/png":       {".png"},
	"image/jpeg":      {".jpg", ".jpeg"},
	"image/gif":       {".gif"},
	"image/avif":      {".avif"},
	"text/vtt":        {".vtt"},
}

// Disposition is the Content-Disposition of a download requested as name
// (the URL's unsigned dl) for an object of contentType: always an
// attachment. The name is used only when it is plain (no path separator,
// control character or bidirectional format character, at most
// MaxDownloadName bytes) and ends in an extension of the object's type, so a
// link cannot rename a file into another type, or make it read as one.
func Disposition(name, contentType string) string {
	typ, _, _ := mime.ParseMediaType(contentType)
	plain := name != "" && len(name) <= MaxDownloadName && !strings.HasPrefix(name, ".") && !strings.ContainsFunc(name, func(c rune) bool {
		return c < 0x20 || c == 0x7f || c == '/' || c == '\\' || c == 0xfffd ||
			c == 0x200e || c == 0x200f || c >= 0x202a && c <= 0x202e || c >= 0x2066 && c <= 0x2069
	})
	if !plain || !slices.Contains(downloadExts[typ], strings.ToLower(path.Ext(name))) {
		return "attachment"
	}
	return Attachment(name)
}

// Attachment is the Content-Disposition for a download name: an ASCII
// fallback plus the RFC 5987 UTF-8 name.
func Attachment(name string) string {
	var ascii, ext strings.Builder
	for _, c := range name {
		if c < 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			ascii.WriteByte('_')
		} else {
			ascii.WriteRune(c)
		}
	}
	for _, b := range []byte(name) {
		if b < 0x80 && (b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("!#$&+-.^_`|~", b) >= 0) {
			ext.WriteByte(b)
		} else {
			fmt.Fprintf(&ext, "%%%02X", b)
		}
	}
	return `attachment; filename="` + ascii.String() + `"; filename*=UTF-8''` + ext.String()
}
