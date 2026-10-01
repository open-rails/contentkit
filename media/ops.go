package media

import (
	"fmt"
	"maps"
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/media/layout"
)

// Commit ops. Only upload paths are writable; derived paths belong to the
// worker.
const (
	OpPut        = "put"        // add an upload, or replace the one with the same path stem
	OpEdit       = "edit"       // crop and rotate an image upload; nil clears
	OpMove       = "move"       // reorder an upload among its Upload's; its outputs follow
	OpRename     = "rename"     // rename an upload and its outputs
	OpRemove     = "remove"     // remove an upload and its outputs
	OpAttach     = "attach"     // make an unattached upload part of the item
	OpCopy       = "copy"       // copy an upload and its current outputs from another item of the kind
	OpFrame      = "frame"      // fill an upload from a frame of its Upload.Frames video
	OpMeta       = "meta"       // set the template values (download names)
	OpRegenerate = "regenerate" // redo stale outputs (of Preset), or every output with Force
)

// Op is one commit op.
type Op struct {
	Op         string         `json:"op"`
	Path       string         `json:"path,omitempty"`
	Blob       string         `json:"blob,omitempty"`  // put
	Index      *int           `json:"index,omitempty"` // put, move, attach: among the attached uploads of its Upload
	Meta       map[string]any `json:"meta,omitempty"`  // put: the upload's meta; attach: merged into it; meta: the item's
	Edit       *Edit          `json:"edit,omitempty"`  // put, edit, frame
	Unattached bool           `json:"unattached,omitempty"`
	To         string         `json:"to,omitempty"`   // rename, copy (default the source's path)
	From       *CopyFrom      `json:"from,omitempty"` // copy
	T          *float64       `json:"t,omitempty"`    // frame: seconds into the video
	Auto       bool           `json:"auto,omitempty"` // frame: the worker chooses
	Preset     string         `json:"preset,omitempty"`
	Force      bool           `json:"force,omitempty"`
}

// CopyFrom names an upload of another item of the same kind.
type CopyFrom struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// NamedPrefix starts the names the server gives Named uploads.
const NamedPrefix = "i-"

// NewName is a fresh Named upload name: "i-{uuid}".
func NewName() string { return NamedPrefix + uuid.NewString() }

// ValidNamed reports a server-given name ("i-{uuid}").
func ValidNamed(name string) bool {
	id, ok := strings.CutPrefix(name, NamedPrefix)
	if !ok {
		return false
	}
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

func (op Op) validate() error {
	bad := func(format string, a ...any) error {
		return uploadErr(CodeInvalid, "%s %q: "+format, append([]any{op.Op, op.Path}, a...)...)
	}
	if op.Path == "" && op.Op != OpMeta && op.Op != OpRegenerate && op.Op != OpCopy {
		return bad("a path is required")
	}
	switch op.Op {
	case OpPut:
		if !layout.ValidHashName(op.Blob) {
			return bad("blob must be sha256-{hex}")
		}
	case OpMove:
		if op.Index == nil {
			return bad("an index is required")
		}
	case OpRename:
		if op.To == "" {
			return bad("to is required")
		}
	case OpCopy:
		if op.From == nil || op.From.ID == "" || op.From.Path == "" {
			return bad("from {id, path} is required")
		}
	case OpFrame:
		if (op.T == nil) == !op.Auto || op.T != nil && *op.T < 0 {
			return bad("give t (seconds) or auto")
		}
	case OpEdit, OpRemove, OpAttach, OpMeta, OpRegenerate:
	default:
		return uploadErr(CodeInvalid, "unknown op %q", op.Op)
	}
	if op.Edit != nil && op.Op != OpPut && op.Op != OpEdit && op.Op != OpFrame {
		return bad("only put, edit and frame take an edit")
	}
	if op.Unattached && op.Op != OpPut {
		return bad("only put takes unattached")
	}
	if err := op.Edit.Check(0, 0); err != nil {
		return bad("%v", err)
	}
	return nil
}

// opRun applies commit ops to a manifest of kind k.
type opRun struct {
	k *Kind
	m *Manifest
	// objects are the put blobs, HEAD-checked in the item's private/.
	objects map[string]Object
	// copies are each copy op's source upload and outputs (by op index),
	// already copied into the item.
	copies map[int][]File
}

func (o *opRun) apply(n int, op Op) error {
	k, m := o.k, o.m
	switch op.Op {
	case OpMeta:
		m.Meta = maps.Clone(op.Meta)
		return nil
	case OpRegenerate:
		if op.Preset != "" && k.private(op.Preset) == nil && k.public(op.Preset) == nil {
			return uploadErr(CodeNotFound, "kind %q has no preset %q", k.Name, op.Preset)
		}
		return nil
	case OpPut:
		return o.put(op)
	case OpFrame:
		return o.frame(op)
	case OpCopy:
		return o.copy(op, o.copies[n])
	}
	i := m.Find(op.Path)
	if i < 0 || !m.Files[i].IsUpload() {
		return uploadErr(CodeNotFound, "no upload %q", op.Path)
	}
	f := m.Files[i]
	switch op.Op {
	case OpEdit:
		if !isImageType(f.Type) {
			return uploadErr(CodeInvalid, "upload %q: only images take an edit", f.Path)
		}
		e, err := fitEdit(k.EditBounds(f.Path), op.Edit, f.W, f.H)
		if err != nil {
			return editErr(err, "upload %q: %v", f.Path)
		}
		if reflect.DeepEqual(e, f.Edit) {
			return nil
		}
		f.Edit = e
		f.Pending = k.Presets(f.Path, m.Hidden)
		m.Files[i] = f
	case OpMove:
		g, _, _, _, _ := k.upload(f.Path)
		m.Files = slices.Delete(m.Files, i, i+1)
		o.insert(g, f, op.Index, !f.Unattached)
	case OpAttach:
		if !f.Unattached {
			return nil // attached already: a retry
		}
		f.Unattached = false
		if len(op.Meta) > 0 {
			f.Meta = maps.Clone(f.Meta)
			if f.Meta == nil {
				f.Meta = map[string]any{}
			}
			maps.Copy(f.Meta, op.Meta)
		}
		g, _, _, _, _ := k.upload(f.Path)
		m.Files = slices.Delete(m.Files, i, i+1)
		o.insert(g, f, op.Index, true)
	case OpRemove:
		m.Files = slices.DeleteFunc(m.Files, func(x File) bool { return x.Path == f.Path || !x.IsUpload() && x.From == f.Path })
	case OpRename:
		return o.rename(f, op.To)
	}
	m.index = nil
	return nil
}

// put adds or replaces an upload.
func (o *opRun) put(op Op) error {
	k, m := o.k, o.m
	g, stem, name, ext, ok := k.upload(op.Path)
	if !ok {
		return uploadErr(CodeNotFound, "kind %q has no upload path %q", k.Name, op.Path)
	}
	u := k.Uploads[g]
	if u.Named && !ValidNamed(name) {
		return uploadErr(CodeInvalid, "upload %q: the server names %s uploads (presign)", op.Path, u.Path)
	}
	obj, ok := o.objects[op.Blob]
	if !ok {
		return uploadErr(CodeNotUploaded, "%s has not been uploaded", op.Blob)
	}
	if err := allows(u, obj.ContentType, obj.Size); err != nil {
		return err
	}
	if ext == "" {
		ext = typeExt(obj.ContentType)
	}
	f := File{Path: stem + "." + ext, Blob: op.Blob, Type: obj.ContentType, Size: obj.Size, Meta: op.Meta, Unattached: op.Unattached}
	i := o.stem(stem)
	if i >= 0 {
		old := m.Files[i]
		if old.Blob == f.Blob {
			f.W, f.H, f.Dur = old.W, old.H, old.Dur
		}
		if f.Meta == nil {
			f.Meta = old.Meta
		}
		f.Unattached = old.Unattached && op.Unattached
	} else if u.Max > 0 && o.count(g) >= u.Max {
		return uploadErr(CodeTooManyFiles, "kind %q allows at most %d uploads at %s", k.Name, u.Max, u.Path)
	}
	if op.Edit != nil {
		if !isImageType(f.Type) {
			return uploadErr(CodeInvalid, "upload %q: only images take an edit", f.Path)
		}
		e, err := fitEdit(k.EditBounds(f.Path), op.Edit, f.W, f.H)
		if err != nil {
			return editErr(err, "upload %q: %v", f.Path)
		}
		f.Edit = e
	}
	if i >= 0 && reflect.DeepEqual(stripPending(m.Files[i]), f) {
		return nil // a retry
	}
	f.Pending = k.Presets(f.Path, m.Hidden)
	return o.place(g, i, f, op.Index)
}

// fitEdit checks e against the upload's bounds and stores what the editor
// chose: a crop fitted to the aspect, or no crop (the producer centres one).
func fitEdit(bounds Image, e *Edit, w, h int) (*Edit, error) {
	e = e.Normalize()
	if e == nil {
		return nil, nil
	}
	fitted, err := bounds.Resolve(e, w, h)
	if err != nil || e.Crop != nil {
		return fitted, err
	}
	return e, nil
}

func stripPending(f File) File {
	f.Pending, f.Failed = nil, nil
	return f
}

// place puts upload f in group g: replacing file i (renaming its outputs'
// From when the extension changed), or inserted at index. A video it
// replaces has its frames grabbed again.
func (o *opRun) place(g, i int, f File, index *int) error {
	m := o.m
	if i < 0 {
		o.insert(g, f, index, !f.Unattached)
		m.index = nil
		return nil
	}
	old := m.Files[i]
	m.Files[i] = f
	for j := range m.Files {
		if x := &m.Files[j]; !x.IsUpload() && x.From == old.Path {
			x.From = f.Path
		}
	}
	m.index = nil
	if old.Blob != f.Blob {
		o.regrab(old.Path)
	}
	return nil
}

// regrab resets the frames grabbed from the video upload at path.
func (o *opRun) regrab(path string) {
	k, m := o.k, o.m
	g, _, _, _, ok := k.upload(path)
	if !ok {
		return
	}
	for j := range m.Files {
		f := &m.Files[j]
		if !f.IsUpload() || f.Frame == nil {
			continue
		}
		if u, _, _, _, ok := k.upload(f.Path); ok && k.Uploads[u].Frames == k.Uploads[g].Path {
			f.Blob, f.W, f.H, f.Size, f.Failed = "", 0, 0, 0, nil
			f.Frame = &Frame{T: f.Frame.T, Auto: f.Frame.Auto}
			f.Pending = k.Presets(f.Path, m.Hidden)
		}
	}
}

// frame sets an upload to a frame grab of its Upload.Frames video.
func (o *opRun) frame(op Op) error {
	k, m := o.k, o.m
	g, stem, _, _, ok := k.upload(op.Path)
	if !ok || k.Uploads[g].Frames == "" {
		return uploadErr(CodeInvalid, "upload %q takes no frames", op.Path)
	}
	video := -1
	for j, f := range m.Files {
		if s, _ := splitExt(f.Path); f.IsUpload() && s == k.Uploads[g].Frames {
			video = j
		}
	}
	if video < 0 {
		return uploadErr(CodeNotFound, "no video at %s to grab a frame from", k.Uploads[g].Frames)
	}
	if v := m.Files[video]; op.T != nil && v.Dur > 0 && *op.T > v.Dur {
		return uploadErr(CodeInvalid, "frame at %gs is past the video's %gs", *op.T, v.Dur)
	}
	f := File{Path: stem + ".png", Type: "image/png", Frame: &Frame{Auto: op.Auto}, Edit: op.Edit.Normalize()}
	if op.T != nil {
		f.Frame.T = *op.T
	}
	i := o.stem(stem)
	if i >= 0 {
		f.Meta, f.Unattached = m.Files[i].Meta, m.Files[i].Unattached
	}
	f.Pending = k.Presets(f.Path, m.Hidden)
	return o.place(g, i, f, nil)
}

// copy adds the copied upload src[0] (and its outputs src[1:]) at op.To.
func (o *opRun) copy(op Op, src []File) error {
	k, m := o.k, o.m
	if len(src) == 0 {
		return uploadErr(CodeNotFound, "no upload %q in item %s", op.From.Path, op.From.ID)
	}
	f := src[0]
	to := op.To
	if to == "" {
		to = f.Path
	}
	g, stem, _, ext, ok := k.upload(to)
	if gf, _, _, _, _ := k.upload(f.Path); !ok || g != gf {
		return uploadErr(CodeInvalid, "copy to %q: not an upload path of %s", to, f.Path)
	}
	if ext == "" {
		_, ext = splitExt(f.Path)
	}
	from := f.Path
	f.Path, f.Unattached = stem+"."+ext, false
	i := o.stem(stem)
	if i < 0 && k.Uploads[g].Max > 0 && o.count(g) >= k.Uploads[g].Max {
		return uploadErr(CodeTooManyFiles, "kind %q allows at most %d uploads at %s", k.Name, k.Uploads[g].Max, k.Uploads[g].Path)
	}
	// Private outputs come along current; public ones render here.
	for _, p := range k.PublicFor(f.Path) {
		if !m.Hidden && !slices.Contains(f.Pending, p.Name) {
			f.Pending = append(slices.Clone(f.Pending), p.Name)
		}
	}
	if i >= 0 {
		old := m.Files[i].Path
		m.Files = slices.DeleteFunc(m.Files, func(x File) bool { return !x.IsUpload() && x.From == old })
		i = o.stem(stem)
	}
	if err := o.place(g, i, f, nil); err != nil {
		return err
	}
	for _, out := range src[1:] {
		out.From = f.Path
		if p := k.private(out.Preset); p != nil {
			out.Path = rebase(out.Path, k.OutputPath(p, from), k.OutputPath(p, f.Path))
		}
		if m.Find(out.Path) >= 0 {
			return uploadErr(CodeConflict, "copy: output %q exists", out.Path)
		}
		m.Files = append(m.Files, out)
		m.index = nil
	}
	return nil
}

// rename renames upload f and re-paths its outputs.
func (o *opRun) rename(f File, to string) error {
	k, m := o.k, o.m
	g, stem, _, ext, ok := k.upload(to)
	if gf, _, _, _, _ := k.upload(f.Path); !ok || g != gf {
		return uploadErr(CodeInvalid, "rename %q to %q: not a path of the same upload", f.Path, to)
	}
	if ext == "" {
		_, ext = splitExt(f.Path)
	}
	to = stem + "." + ext
	if to == f.Path {
		return nil
	}
	if o.stem(stem) >= 0 {
		return uploadErr(CodeConflict, "upload %q exists", to)
	}
	if k.Uploads[g].Named {
		return uploadErr(CodeInvalid, "named uploads keep their name")
	}
	for j := range m.Files {
		x := &m.Files[j]
		switch {
		case x.Path == f.Path:
			x.Path = to
		case !x.IsUpload() && x.From == f.Path:
			x.From = to
			if p := k.private(x.Preset); p != nil {
				x.Path = rebase(x.Path, k.OutputPath(p, f.Path), k.OutputPath(p, to))
			}
		case x.IsUpload() && x.Meta[MetaFor] == f.Path:
			x.Meta = maps.Clone(x.Meta)
			x.Meta[MetaFor] = to
		}
	}
	m.index = nil
	return nil
}

// rebase moves p from template output old to new: a file renamed, or a
// path under a directory output moved.
func rebase(p, old, new string) string {
	if strings.HasSuffix(old, "/") {
		if rest, ok := strings.CutPrefix(p, old); ok {
			return new + rest
		}
		return p
	}
	if p == old {
		return new
	}
	return p
}

// stem finds the upload whose path has stem, or -1.
func (o *opRun) stem(stem string) int {
	for i, f := range o.m.Files {
		if s, _ := splitExt(f.Path); f.IsUpload() && s == stem {
			return i
		}
	}
	return -1
}

// count is the uploads of group g.
func (o *opRun) count(g int) int {
	n := 0
	for _, f := range o.m.Files {
		if u, _, _, _, ok := o.k.upload(f.Path); f.IsUpload() && ok && u == g {
			n++
		}
	}
	return n
}

// insert puts upload f among group g's attached uploads (or its unattached
// ones): at index when given, else at its natural-sort position.
func (o *opRun) insert(g int, f File, index *int, attached bool) {
	m := o.m
	var members []int
	for i, x := range m.Files {
		if u, _, _, _, ok := o.k.upload(x.Path); x.IsUpload() && ok && u == g && x.Unattached != attached {
			members = append(members, i)
		}
	}
	at := len(m.Files)
	if len(members) > 0 {
		at = members[len(members)-1] + 1
	}
	name := path.Base(f.Path)
	for n, i := range members {
		if index != nil && n == max(*index, 0) || index == nil && natLess(name, path.Base(m.Files[i].Path)) {
			at = i
			break
		}
	}
	m.Files = slices.Insert(m.Files, at, f)
	m.index = nil
}

// allows checks a file against its Upload's types and size cap.
func allows(u Upload, contentType string, size int64) error {
	if !slices.Contains(u.Types, contentType) {
		return &UploadError{Code: CodeType, Message: fmt.Sprintf("%s files are not allowed at %s; allowed: %s", contentType, u.Path, strings.Join(u.Types, ", ")),
			Details: &ErrorDetails{Type: contentType, Allowed: u.Types}}
	}
	if size <= 0 || size > u.MaxBytes {
		return &UploadError{Code: CodeTooLarge, Message: fmt.Sprintf("%s files at %s may be at most %d bytes; this one is %d", contentType, u.Path, u.MaxBytes, size),
			Details: &ErrorDetails{Type: contentType, Size: size, MaxBytes: u.MaxBytes}}
	}
	return nil
}
