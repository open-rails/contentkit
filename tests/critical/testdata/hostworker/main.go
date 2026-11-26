// Command hostworker is a host's own media worker: the stock worker plus one
// master hook, built on the public media/worker package alone.
package main

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/worker"
)

// invert inverts every pixel of a PNG.
type invert struct{}

func (invert) Name() string    { return "invert" }
func (invert) Version() string { return "1" }

func (invert) Process(_ context.Context, in media.MasterInput) (media.MasterOutput, error) {
	f, err := os.Open(in.Path)
	if err != nil {
		return media.MasterOutput{}, err
	}
	src, err := png.Decode(f)
	f.Close()
	if err != nil {
		return media.MasterOutput{}, err
	}
	b := src.Bounds()
	out := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
			out.SetNRGBA(x, y, color.NRGBA{R: 255 - c.R, G: 255 - c.G, B: 255 - c.B, A: c.A})
		}
	}
	w, err := os.Create(in.Out)
	if err != nil {
		return media.MasterOutput{}, err
	}
	if err := png.Encode(w, out); err != nil {
		w.Close()
		return media.MasterOutput{}, err
	}
	return media.MasterOutput{Type: media.MasterPNG}, w.Close()
}

func main() { worker.Main(invert{}) }
