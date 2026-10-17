package image_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"io"
	"slices"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

func TestOriginalFormatDownloadArchive(t *testing.T) {
	e := newEnv(t, func(c *media.Config) {
		c.Kinds[0].Private = []media.Private{
			{Name: "thumb", From: "originals/{name}", To: "thumb/{name}.webp", Image: &media.Image{Width: 100, Height: 150}},
			{Name: "source", From: "originals/{name}", To: "download-pages/{name}{ext}", Image: &media.Image{Format: media.ImageFormatSource, Width: 6000, Height: 6000, Quality: 90}},
			{Name: "zip", To: "download/pages.zip", Download: "{title}.zip", Zip: "download-pages/"},
		}
	})
	ref := e.ref(t, "gallery", 1)
	pngSource := solid(t, 32, 48, red)
	var jpegSource bytes.Buffer
	if err := jpeg.Encode(&jpegSource, paint(32, 48, func(int, int) color.RGBA { return green }), &jpeg.Options{Quality: 93}); err != nil {
		t.Fatal(err)
	}
	largeSource := solid(t, 24, 12000, blue)
	e.put(t, ref, "originals/001.png", "image/png", pngSource)
	e.put(t, ref, "originals/002.jpg", "image/jpeg", jpegSource.Bytes())
	e.put(t, ref, "originals/003.png", "image/png", largeSource)
	e.process(t, media.ProcessJob{Ref: ref})
	for source, want := range map[string][]byte{"originals/001.png": pngSource, "originals/002.jpg": jpegSource.Bytes(), "originals/003.png": largeSource} {
		if !bytes.Equal(e.blob(t, ref, e.file(t, ref, source)), want) {
			t.Fatalf("archival original changed: %s", source)
		}
	}
	for output, source := range map[string]string{"download-pages/001.png": "originals/001.png", "download-pages/002.jpg": "originals/002.jpg"} {
		if e.file(t, ref, output).Blob != e.file(t, ref, source).Blob {
			t.Fatalf("under-limit source was copied or re-encoded: %s", output)
		}
	}
	check := func(wantNames []string) {
		t.Helper()
		body := e.blob(t, ref, e.file(t, ref, "download/pages.zip"))
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatal(err)
			}
			switch f.Name {
			case "001.png":
				if !bytes.Equal(data, pngSource) {
					t.Fatal("ZIP re-encoded the original PNG")
				}
			case "002.jpg":
				if !bytes.Equal(data, jpegSource.Bytes()) {
					t.Fatal("ZIP re-encoded the original JPEG")
				}
			case "003.png":
				dims, format, err := stdimage.DecodeConfig(bytes.NewReader(data))
				if err != nil || format != "png" || dims.Width != 12 || dims.Height != 6000 {
					t.Fatalf("oversized source was not proportionally bounded in its original format: %+v %s %v", dims, format, err)
				}
			}
		}
		if !slices.Equal(names, wantNames) {
			t.Fatalf("ZIP order/extension=%v want=%v", names, wantNames)
		}
	}
	check([]string{"001.png", "002.jpg", "003.png"})
	e.commit(t, ref, media.Op{Op: media.OpMove, Path: "originals/003.png", Index: new(int)})
	e.process(t, media.ProcessJob{Ref: ref})
	check([]string{"003.png", "001.png", "002.jpg"})
}

func TestOriginalFormatUpgradeFromWebPArchive(t *testing.T) {
	e := newEnv(t, nil)
	ref := e.ref(t, "gallery", 1)
	source := solid(t, 32, 48, red)
	e.put(t, ref, "originals/001.png", "image/png", source)
	e.process(t, media.ProcessJob{Ref: ref})
	old := e.file(t, ref, "download/pages.zip")
	if _, err := e.ms.EditExisting(context.Background(), ref, func(m *media.Manifest) error {
		m.AddPending("originals/001.png", "high")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.deploy(t, func(c *media.Config) {
		c.Kinds[0].Private = []media.Private{
			{Name: "thumb", From: "originals/{name}", To: "thumb/{name}.webp", Image: &media.Image{Width: 100, Height: 150}},
			{Name: "source", From: "originals/{name}", To: "download-pages/{name}{ext}", Image: &media.Image{Format: media.ImageFormatSource, Width: 6000, Height: 6000}},
			{Name: "zip", To: "download/pages.zip", Download: "{title}.zip", Zip: "download-pages/"},
		}
	})
	e.process(t, media.ProcessJob{Ref: ref})
	k, _ := e.reg.Kind("gallery")
	if r := k.Readiness(e.manifest(t, ref)); !r.Ready() {
		t.Fatalf("removed high preset stranded upgraded gallery: %+v", r)
	}
	archive := e.file(t, ref, "download/pages.zip")
	if archive.Blob == old.Blob || archive.From != "download-pages/" {
		t.Fatal("old WebP ZIP survived the source-format preset upgrade")
	}
	body := e.blob(t, ref, archive)
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || len(zr.File) != 1 || zr.File[0].Name != "001.png" {
		t.Fatalf("upgraded archive did not preserve PNG: %v %v", zr, err)
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, source) || !bytes.Equal(e.blob(t, ref, e.file(t, ref, "originals/001.png")), source) {
		t.Fatalf("upgrade changed the original or downloaded PNG: %v", err)
	}
}

func TestSourceFormatPreservesAllocationArea(t *testing.T) {
	for _, originalDownload := range []bool{false, true} {
		t.Run(fmt.Sprint(originalDownload), func(t *testing.T) {
			e := newEnv(t, func(c *media.Config) {
				c.Kinds[0].Private = []media.Private{{Name: "source", From: "originals/{name}", To: "download-pages/{name}{ext}", Image: &media.Image{Format: media.ImageFormatSource}}}
				c.Kinds[0].Downloads = []string{"source"}
				if originalDownload {
					c.Kinds[0].Downloads = []string{"originals/{name}"}
				}
			})
			ref := e.ref(t, "gallery", 1)
			source := solid(t, 32, 48, red)
			e.put(t, ref, "originals/001.png", "image/png", source)
			e.process(t, media.ProcessJob{Ref: ref})
			original, output := e.file(t, ref, "originals/001.png"), e.file(t, ref, "download-pages/001.png")
			area, _, ok := layout.SplitBlobRef(output.Blob)
			want := layout.AreaDownload
			if originalDownload {
				want = layout.AreaPrivate
			}
			if !ok || area != want || output.Blob == original.Blob || !bytes.Equal(e.blob(t, ref, output), source) {
				t.Fatalf("source-format output crossed allocation areas: original=%+v output=%+v want area=%s", original, output, want)
			}
		})
	}
}
