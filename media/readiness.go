package media

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// Processing states of a file, a slot or an item.
const (
	StateReady      = "ready"
	StateProcessing = "processing"
	StateFailed     = "failed"
)

// Readiness is whether an item's media is fully processed: ready when every
// attached file, set slot and video poster is; processing while any is still
// being derived or encoded; failed once nothing is processing and some could
// not be. Processing and Failed name them ("{version}/{file}" for a versioned
// kind's files, the slot name for slots).
type Readiness struct {
	State      string   `json:"state"`
	Processing []string `json:"processing,omitempty"`
	Failed     []string `json:"failed,omitempty"`
}

func (r Readiness) Ready() bool { return r.State == StateReady }

func (r *Readiness) add(name, state string) {
	switch state {
	case StateProcessing:
		r.Processing = append(r.Processing, name)
	case StateFailed:
		r.Failed = append(r.Failed, name)
	}
}

func (r Readiness) settle() Readiness {
	switch {
	case len(r.Processing) > 0:
		r.State = StateProcessing
	case len(r.Failed) > 0:
		r.State = StateFailed
	default:
		r.State = StateReady
	}
	return r
}

// State is the file's processing state. A video is ready when its current
// source's ladder has every stage (no hls.pending) and failed on hls.error;
// audio when its current source's track is encoded, failed on hls.error; an image when its variants were derived from its current source and edit
// (Derived) and failed on Failure; a staged upload is processing. Other
// types need no processing.
func (f File) State() string { return f.state(true) }

// state is State; images false marks a kind whose images are never processed.
func (f File) state(images bool) string {
	h := f.HLS
	switch {
	case isEncodedType(f.Type) && h != nil && h.Source == f.Source() && h.Error != "":
		return StateFailed
	case isImageType(f.Type) && f.Failed() != nil:
		return StateFailed
	case layout.ValidStagedName(f.Original) && (images || isEncodedType(f.Type)):
		return StateProcessing
	case isVideoType(f.Type):
		if h == nil || h.Source != f.Source() || len(h.Video) == 0 || len(h.Pending) > 0 {
			return StateProcessing
		}
	case isAudioType(f.Type):
		if h == nil || h.Source != f.Source() || len(h.Audio) == 0 {
			return StateProcessing
		}
	case isImageType(f.Type) && images && f.Derived != f.FailureKey():
		return StateProcessing
	}
	return StateReady
}

// Servable reports a file viewers may be shown: processed, or still serving
// the outputs of a source or edit being replaced (a stale ladder plays until
// its successor is promoted; old variants stay until new ones land). A file
// that has nothing complete to serve, or failed, is not.
func (f File) Servable() bool {
	switch {
	case isVideoType(f.Type):
		h := f.HLS
		return h != nil && h.Error == "" && len(h.Video) > 0 && (h.Source != f.Source() || len(h.Pending) == 0)
	case isAudioType(f.Type):
		h := f.HLS
		return h != nil && h.Error == "" && len(h.Audio) > 0
	case isImageType(f.Type):
		return f.Failed() == nil && len(f.Variants) > 0
	}
	return true
}

// Servable is the manifest without the files viewers may not be shown
// (File.Servable) and their video downloads, as non-editors read it; m itself
// when every file is servable.
func (m *Manifest) Servable() *Manifest {
	return m.without(func(f File) bool { return !f.Servable() })
}

// Readiness is the attached files' readiness.
func (m *Manifest) Readiness() Readiness {
	var r Readiness
	for _, f := range m.Files {
		if !f.Unattached {
			r.add(f.Name, f.State())
		}
	}
	return r.settle()
}

// Readiness is the item's readiness under kind k (the registry's, so a video
// kind has its poster slot): every section's attached files, every set slot
// and a video kind's poster.
func (r *Root) Readiness(k Kind) Readiness {
	var out Readiness
	images := len(k.Specs) > 0 || len(k.Slots) > 0 || k.Inline != nil // the image job runs
	r.sections(func(v string, m *Manifest) {
		for _, f := range m.Files {
			if f.Unattached {
				continue
			}
			name := f.Name
			if v != "" {
				name = v + "/" + name
			}
			out.add(name, f.state(images))
		}
	})
	for _, name := range slices.Sorted(maps.Keys(r.Slots)) {
		if spec, ok := k.Slots[name]; ok && !(name == PosterSlot && k.Video != nil) {
			out.add(name, r.Slots[name].state(spec))
		}
	}
	if k.Video != nil {
		out.add(PosterSlot, r.posterState(k.Slots[PosterSlot]))
	}
	return out.settle()
}

// state is a set slot's: ready once its outputs match its original, edit and
// spec; failed on the result's error for them. An unset slot is ready.
func (rec *SlotRecord) state(spec Slot) string {
	switch {
	case rec.Original == "":
		return StateReady
	case rec.Result == nil || rec.Result.Of != rec.Fingerprint(spec):
		return StateProcessing
	case rec.Result.Error != "":
		return StateFailed
	}
	return StateReady
}

// posterState is a video item's poster: processing while a frame is due to be
// grabbed from an encoded video (the automatic one, or a selection whose
// video changed) or the grabbed or uploaded image is encoding. "" when there
// is no poster to wait for.
func (r *Root) posterState(poster Slot) string {
	rec := r.Slots[PosterSlot]
	if rec == nil {
		rec = &SlotRecord{}
	}
	if rec.Frame == nil && rec.Original != "" {
		return rec.state(poster) // uploaded
	}
	if rec.Frame == nil {
		due := false
		r.sections(func(_ string, m *Manifest) {
			f, ok := VideoFile(m, "")
			due = due || ok && Encoded(f)
		})
		if due {
			return StateProcessing
		}
		return ""
	}
	sel := *rec.Frame
	if m := r.Section(sel.Version); m != nil {
		f, ok := VideoFile(m, sel.File)
		if !ok && sel.Auto {
			f, ok = VideoFile(m, "")
		}
		if ok && Encoded(f) && sel.Source != f.Source() {
			return StateProcessing
		}
	}
	if rec.Original == "" {
		return ""
	}
	return rec.state(poster)
}

// Readiness reads the item's readiness (Root.Readiness); ErrNotFound when it
// has no manifest.
func (m *Manifests) Readiness(ctx context.Context, ref contentref.ContentRef) (Readiness, error) {
	k, err := m.kinds.Kind(ref.ContentKind)
	if err != nil {
		return Readiness{}, err
	}
	root, _, err := m.Root(ctx, ref)
	if err != nil {
		return Readiness{}, err
	}
	return root.Readiness(k), nil
}

func isVideoType(t string) bool { return strings.HasPrefix(t, "video/") }
func isImageType(t string) bool { return strings.HasPrefix(t, "image/") }
func isAudioType(t string) bool { return strings.HasPrefix(t, "audio/") }

// isEncodedType is a type the media worker encodes with ffmpeg.
func isEncodedType(t string) bool { return isVideoType(t) || isAudioType(t) }
