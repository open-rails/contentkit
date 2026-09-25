package media

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/open-rails/contentkit/media/layout"
)

// Root is manifest.json, an item's one canonical manifest (never served):
// its files (a section per version for versioned kinds), its slots and
// inline images, whether it is hidden, and an index of every object the
// folder keeps. Originals, Private and the Public flags are rebuilt from the
// rest on every write; the sweep deletes what they do not list.
type Root struct {
	Manifest                        // an unversioned kind's files
	Versions map[string]*Manifest   `json:"versions,omitempty"`
	Slots    map[string]*SlotRecord `json:"slots,omitempty"` // registered slots and inline images ("i-{uuid}")
	Hidden   bool                   `json:"hidden,omitempty"`
	// Originals indexes originals/ by name.
	Originals map[string]OriginalEntry `json:"originals"`
	// Private indexes private/ by name; Public entries are copied to public/
	// under the same name.
	Private map[string]PrivateEntry `json:"private"`
}

// OriginalEntry is one uploaded file: its upload name, type and size, and
// the renditions derived from it.
type OriginalEntry struct {
	Name       string   `json:"name,omitempty"`
	Slot       string   `json:"slot,omitempty"`
	Type       string   `json:"type,omitempty"`
	Size       int64    `json:"size,omitempty"`
	Renditions []string `json:"renditions,omitempty"`
}

// PrivateEntry is one rendition: the file (in Version) or slot it belongs
// to, which rendition it is, and whether it is exposed in public/.
type PrivateEntry struct {
	File      string `json:"file,omitempty"`
	Version   string `json:"version,omitempty"`
	Slot      string `json:"slot,omitempty"`
	Rendition string `json:"rendition"`
	W         int    `json:"w,omitempty"`
	Type      string `json:"type,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Public    bool   `json:"public,omitempty"`
}

// Section is version v's files ("" for an unversioned kind), or nil.
func (r *Root) Section(v string) *Manifest {
	if v == "" {
		return &r.Manifest
	}
	return r.Versions[v]
}

// section is Section, creating a missing version.
func (r *Root) section(v string) *Manifest {
	if m := r.Section(v); m != nil {
		return m
	}
	if r.Versions == nil {
		r.Versions = map[string]*Manifest{}
	}
	m := &Manifest{Files: []File{}}
	r.Versions[v] = m
	return m
}

// sections visits the unversioned files and every version, in order.
func (r *Root) sections(fn func(v string, m *Manifest)) {
	fn("", &r.Manifest)
	for _, v := range slices.Sorted(maps.Keys(r.Versions)) {
		fn(v, r.Versions[v])
	}
}

// PublicNames lists the renditions exposed in public/.
func (r *Root) PublicNames() []string {
	var out []string
	for name, e := range r.Private {
		if e.Public {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// index rebuilds Originals and Private.
func (r *Root) index() {
	r.Originals, r.Private = map[string]OriginalEntry{}, map[string]PrivateEntry{}
	original := func(name string, e OriginalEntry) {
		if !layout.ValidHashName(name) {
			return // staged: not in the folder's index
		}
		if _, ok := r.Originals[name]; !ok {
			r.Originals[name] = e
		}
	}
	private := func(name, source string, e PrivateEntry) {
		if name == "" {
			return
		}
		if cur, ok := r.Private[name]; ok {
			cur.Public = cur.Public || e.Public
			r.Private[name] = cur
		} else {
			r.Private[name] = e
		}
		if o, ok := r.Originals[source]; ok && !slices.Contains(o.Renditions, name) {
			o.Renditions = append(o.Renditions, name)
			r.Originals[source] = o
		}
	}
	r.sections(func(v string, m *Manifest) {
		for _, f := range m.Files {
			original(f.Original, OriginalEntry{Name: f.Name, Type: f.Type, Size: f.Size})
			if f.Master != "" {
				original(f.Master, OriginalEntry{Name: f.Name})
			}
			for _, k := range slices.Sorted(maps.Keys(f.Variants)) {
				vr := f.Variants[k]
				private(vr.Blob, f.Source(), PrivateEntry{File: f.Name, Version: v, Rendition: k, W: vr.W, Type: vr.Type, Size: vr.Size})
			}
			if h := f.HLS; h != nil {
				e := func(rendition string, w int, typ string) PrivateEntry {
					return PrivateEntry{File: f.Name, Version: v, Rendition: rendition, W: w, Type: typ}
				}
				for _, x := range h.Video {
					private(x.Blob, h.Source, e("hls-"+strconv.Itoa(x.Rung)+"p-"+string(x.Codec), x.Width, "video/mp4"))
				}
				for _, x := range h.Audio {
					private(x.Blob, h.Source, e("audio-"+x.ID, 0, "audio/mp4"))
				}
				for _, x := range h.Subs {
					private(x.Blob, h.Source, e("subs-"+x.ID, 0, "text/vtt"))
				}
				if s := h.Sprite; s != nil {
					private(s.Blob, h.Source, e("sprite", s.Width*s.Cols, "image/jpeg"))
				}
			}
		}
		for _, k := range slices.Sorted(maps.Keys(m.Downloads)) {
			d := m.Downloads[k]
			source := ""
			if i := m.File(downloadFile(k)); i >= 0 {
				source = m.Files[i].Source()
			}
			private(d.Blob, source, PrivateEntry{Version: v, Rendition: "download-" + k, Type: d.Type, Size: d.Size})
		}
	})
	for _, slot := range slices.Sorted(maps.Keys(r.Slots)) {
		rec := r.Slots[slot]
		original(rec.Original, OriginalEntry{Name: rec.Filename, Slot: slot, Type: rec.Type, Size: rec.Size})
		if rec.Result == nil {
			continue
		}
		for _, o := range rec.Result.Outputs {
			private(o.Blob, rec.Result.Source, PrivateEntry{Slot: slot, Rendition: strconv.Itoa(o.Rung), W: o.W,
				Type: "image/webp", Size: o.Size, Public: !r.Hidden})
		}
	}
}

// validate checks every section and slot.
func (r *Root) validate() error {
	var err error
	r.sections(func(v string, m *Manifest) {
		if err == nil && (v != "" && !layout.ValidSegment(v) || m == nil) {
			err = fmt.Errorf("media: manifest: invalid version %q", v)
		}
		if err == nil {
			if err = m.Validate(); err != nil && v != "" {
				err = fmt.Errorf("version %s: %w", v, err)
			}
		}
	})
	if err != nil {
		return err
	}
	for name, rec := range r.Slots {
		if rec == nil || !layout.ValidSegment(name) {
			return fmt.Errorf("media: manifest: invalid slot %q", name)
		}
		if rec.Original != "" && !layout.ValidHashName(rec.Original) {
			return fmt.Errorf("media: manifest: slot %q: invalid original %q", name, rec.Original)
		}
		if rec.Result != nil {
			for _, o := range rec.Result.Outputs {
				if !layout.ValidHashName(o.Blob) {
					return fmt.Errorf("media: manifest: slot %q: invalid output %q", name, o.Blob)
				}
			}
		}
	}
	return nil
}

// normalize writes empty file lists as [].
func (r *Root) normalize() {
	r.sections(func(_ string, m *Manifest) {
		if m.Files == nil {
			m.Files = []File{}
		}
	})
}

// Refs is every object the manifest keeps, as "{area}/{name}": the indexed
// originals, renditions and public copies, and staged uploads.
func (r *Root) Refs() map[string]bool {
	refs := map[string]bool{}
	for n := range r.Originals {
		refs[AreaOriginals+"/"+n] = true
	}
	for n, e := range r.Private {
		refs[AreaPrivate+"/"+n] = true
		if e.Public {
			refs[AreaPublic+"/"+n] = true
		}
	}
	r.sections(func(_ string, m *Manifest) {
		for _, n := range m.Sources() {
			refs[layout.SourceArea(n)+"/"+n] = true
		}
	})
	return refs
}
