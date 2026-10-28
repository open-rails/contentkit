package mediastore

import (
	"fmt"
	"maps"
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// NewName is a fresh Named upload name: "i-{uuid}".
func NewName() string { return media.NamedPrefix + uuid.NewString() }

func validateOp(op media.Op) error {
	bad := func(format string, a ...any) error {
		return uploadErr(media.CodeInvalid, "%s %q: "+format, append([]any{op.Op, op.Path}, a...)...)
	}
	if op.Path == "" && op.Op != media.OpMeta && op.Op != media.OpRegenerate && op.Op != media.OpCopy {
		return bad("a path is required")
	}
	switch op.Op {
	case media.OpPut:
		if !layout.ValidBlobRef(op.Blob) && !layout.ValidStagedName(op.Blob) {
			return bad("blob must be a blob reference or a staged u-{uuid}")
		}
	case media.OpMove:
		if op.Index == nil {
			return bad("an index is required")
		}
	case media.OpRename:
		if op.To == "" {
			return bad("to is required")
		}
	case media.OpCopy:
		if op.From == nil || op.From.ID == "" || op.From.Path == "" || op.From.Kind != "" {
			return bad("from {id, path} is required; kind is for frame")
		}
	case media.OpFrame:
		if (op.T == nil) == !op.Auto || op.T != nil && *op.T < 0 {
			return bad("give t (seconds) or auto")
		}
		if op.From != nil && (op.From.ID == "" || op.From.Path == "") {
			return bad("from {id, path} names the video")
		}
	case media.OpEdit, media.OpRemove, media.OpAttach, media.OpMeta, media.OpRegenerate:
	default:
		return uploadErr(media.CodeInvalid, "unknown op %q", op.Op)
	}
	if op.Edit != nil && op.Op != media.OpPut && op.Op != media.OpEdit && op.Op != media.OpCopy && op.Op != media.OpFrame {
		return bad("only put, edit, copy and frame take an edit")
	}
	if op.Unattached && op.Op != media.OpPut {
		return bad("only put takes unattached")
	}
	if op.Takedown && op.Op != media.OpRemove {
		return bad("only remove takes takedown")
	}
	if op.CreateID != "" {
		id, err := uuid.Parse(op.CreateID)
		if op.Op != media.OpPut || err != nil || id == uuid.Nil || id.String() != op.CreateID {
			return bad("create_id requires a put and a canonical nonzero UUID")
		}
	}
	if op.Replace && (op.Op != media.OpPut || op.CreateID != "") {
		return bad("replace requires a put without create_id")
	}
	if limit := metaLimit(op); metaBytes(op.Meta) > limit {
		return uploadErr(media.CodeTooLarge, "%s %q: meta is at most %d bytes of JSON", op.Op, op.Path, limit)
	}
	if err := op.Edit.Check(0, 0); err != nil {
		return bad("%v", err)
	}
	return nil
}

// metaLimit bounds op.Meta: the item's for a meta op, else an upload's.
func metaLimit(op media.Op) int {
	if op.Op == media.OpMeta {
		return media.MaxItemMetaBytes
	}
	return media.MaxMetaBytes
}

// opRun applies commit ops to a manifest of kind k.
type opRun struct {
	renames map[string]string
	removed []string
	k       *RegisteredKind
	m       *Manifest
	ref     contentref.ContentRef
	// objects are the put names, HEAD-checked in the item's folder.
	objects map[string]media.Object
	// copies are each copy op's source upload and outputs (by op index),
	// already copied into the item.
	copies map[int][]media.File
	// frames are the videos frame ops name in other items, by op index.
	frames map[int]media.File
	// exempt is an exempt grant: its takedown of a path already gone is a
	// retry, not an error.
	exempt         bool
	trusted        bool // in-process system commits may update reserved provenance
	admission      func(string, string) (*media.SourceAdmission, error)
	archiveAllowed func(string, string) error // verified source policy, only resolved when admitting a ZIP
	// authorizePath rechecks a collision allocation against host path policy.
	authorizePath func(string) error
}

func (o *opRun) apply(n int, op media.Op) error {
	k, m := o.k, o.m
	switch op.Op {
	case media.OpMeta:
		next := maps.Clone(op.Meta)

		m.Meta = next
		return nil
	case media.OpRegenerate:
		if op.Preset != "" && k.private(op.Preset) == nil && k.public(op.Preset) == nil {
			return uploadErr(media.CodeNotFound, "kind %q has no preset %q", k.Name, op.Preset)
		}
		// Uploads that failed for their current blob and edit are retried:
		// their presets are pending again, as after a new upload. A generated
		// frame can fail before it has its first blob and must be retried too.
		for i, f := range m.Files {
			if !f.IsUpload() || f.Gone || f.Blob == "" && f.Frame == nil || f.Fail() == nil {
				continue
			}
			presets := k.Presets(f.Path, false)
			if op.Preset != "" && !slices.Contains(presets, op.Preset) {
				continue
			}
			f.Failed, f.Pending = nil, presets
			m.Files[i] = f
		}
		return nil
	case media.OpPut:
		return o.put(op)
	case media.OpFrame:
		return o.frame(n, op)
	case media.OpCopy:
		src := o.copies[n]
		if op.From.ID == o.ref.ContentID && op.From.Version == o.ref.Version() {
			src = copyFiles(m, op.From.Path)
		}
		return o.copy(op, src)
	}
	i := m.Find(op.Path)
	publicRemoval := op.Op == media.OpRemove && (op.Takedown && o.exempt || len(k.PublicFor(op.Path)) > 0)
	if i < 0 && publicRemoval {
		return nil // retry after the manifest changed but the cleanup failed
	}
	if i < 0 || !m.Files[i].IsUpload() {
		return uploadErr(media.CodeNotFound, "no upload %q", op.Path)
	}
	f := m.Files[i]
	if op.Op == media.OpRemove && op.Meta[media.MetaAutoCover] == true && f.Meta[media.MetaAutoCover] != true {
		return uploadErr(media.CodeConflict, "an editor owns cover %q", f.Path)
	}
	switch op.Op {
	case media.OpEdit:
		if !isImageType(f.Type) {
			return uploadErr(media.CodeInvalid, "upload %q: only images take an edit", f.Path)
		}
		e, err := fitEdit(k.EditBounds(f.Path), op.Edit, f.W, f.H)
		if err != nil {
			return editErr(err, "upload %q: %v", f.Path)
		}
		if reflect.DeepEqual(e, f.Edit) {
			return nil
		}
		f.Edit = e
		f.Pending = k.Presets(f.Path, false)
		m.Files[i] = f
	case media.OpMove:
		g, _, _, _, _ := k.upload(f.Path)
		m.Files = slices.Delete(m.Files, i, i+1)
		o.insert(g, f, op.Index, !f.Unattached)
	case media.OpAttach:
		if !f.Unattached {
			return nil // attached already: a retry
		}
		if k.ArchiveDestination(f.Path) != "" {
			if err := o.attachArchiveBatch(f); err != nil {
				return err
			}
			i = m.Find(f.Path)
		}
		f.Unattached = false
		if len(op.Meta) > 0 {
			f.Meta = maps.Clone(f.Meta)
			if f.Meta == nil {
				f.Meta = map[string]any{}
			}
			maps.Copy(f.Meta, op.Meta)
			if metaBytes(f.Meta) > media.MaxMetaBytes {
				return uploadErr(media.CodeTooLarge, "attach %q: meta is at most %d bytes of JSON", f.Path, media.MaxMetaBytes)
			}
		}
		g, _, _, _, _ := k.upload(f.Path)
		m.Files = slices.Delete(m.Files, i, i+1)
		o.insert(g, f, op.Index, true)
	case media.OpRemove:
		gone := append([]string{f.Path}, o.cancelledArchivePages(f)...)
		if op.Takedown {
			// The frames grabbed from it show it too.
			stem, _ := splitExt(f.Path)
			for _, x := range m.Files {
				if g, _, _, _, ok := k.upload(x.Path); ok && x.IsUpload() && x.Frame != nil && x.Frame.From == nil && k.Uploads[g].Frames == stem {
					gone = append(gone, x.Path)
				}
			}
			for _, p := range gone {
				o.dropZips(p)
			}
		}
		o.dropUploads(gone)
	case media.OpRename:
		return o.rename(f, op.To)
	}
	m.index = nil
	return nil
}

func (o *opRun) dropUploads(paths []string) {
	if len(paths) == 0 {
		return
	}
	o.removed = append(o.removed, paths...)
	o.m.Files = slices.DeleteFunc(o.m.Files, func(file media.File) bool {
		return slices.Contains(paths, file.Path) || !file.IsUpload() && slices.Contains(paths, file.From)
	})
	o.m.index = nil
}

// dropZips removes zips bundling the upload at path or its outputs; the
// worker builds them again without it.
func (o *opRun) dropZips(path string) {
	for i := range o.k.Private {
		z := &o.k.Private[i]
		if z.Zip == "" || !strings.HasPrefix(path, z.Zip) && !slices.ContainsFunc(o.m.Files, func(x media.File) bool { return x.From == path && strings.HasPrefix(x.Path, z.Zip) }) {
			continue
		}
		o.m.Files = slices.DeleteFunc(o.m.Files, func(x media.File) bool { return x.Preset == z.Name && x.From == z.Zip })
	}
}

// put adds or replaces an upload.
func (o *opRun) put(op media.Op) error {
	k, m := o.k, o.m
	requested := op.Path
	source := op.SourceName
	if source == "" {
		source = requested
	}
	collision := false
	if k.IsPageUpload(media.File{Path: op.Path}) {
		if op.CreateID != "" {
			for _, f := range m.Files {
				if k.IsPageUpload(f) && f.CreateID == op.CreateID {
					op.Path = f.Path
					break
				}
			}
		}
		stem, _ := splitExt(op.Path)
		if i := o.stem(stem); i >= 0 {
			old := m.Files[i]
			if op.SourceName == "" && (op.CreateID == "" || op.CreateID == old.CreateID) {
				source = sourceName(old.Meta, old.Path)
			}
			collision = !op.Replace && (sourceName(old.Meta, old.Path) != sourceName(nil, source) ||
				op.CreateID != "" && op.CreateID != old.CreateID)
		} else {
			collision = pageNameUsed(k, m, k.NameOf(op.Path))
		}
		if collision {
			op.Path = nextPagePath(k, m, op.Path)
		}
	}
	if op.Path != requested && o.authorizePath != nil {
		if err := o.authorizePath(op.Path); err != nil {
			return err
		}
	}
	g, stem, name, ext, ok := k.upload(op.Path)
	if !ok {
		return uploadErr(media.CodeNotFound, "kind %q has no upload path %q", k.Name, op.Path)
	}
	u := k.Uploads[g]
	if u.Named && !media.ValidNamed(name) {
		return uploadErr(media.CodeInvalid, "upload %q: the server names %s uploads (presign)", op.Path, u.Path)
	}
	obj, ok := o.objects[op.Blob]
	if !ok {
		return uploadErr(media.CodeNotUploaded, "%s has not been uploaded", op.Blob)
	}
	if err := allows(u, obj.ContentType, obj.Size); err != nil {
		return err
	}
	if ext == "" {
		ext = typeExt(obj.ContentType)
	}
	f := media.File{Path: stem + "." + ext, Type: obj.ContentType, Size: obj.Size, Meta: op.Meta, Unattached: op.Unattached, CreateID: op.CreateID}
	if layout.ValidStagedName(op.Blob) {
		f.Staged = op.Blob
	} else if area, _, _ := layout.SplitBlobRef(op.Blob); area != k.Area(op.Path, "") {
		return uploadErr(media.CodeInvalid, "upload %q: the blob is in %s/, the path's files in %s/", op.Path, area, k.Area(op.Path, ""))
	} else {
		f.Blob = op.Blob
	}
	i := o.stem(stem)
	legacy := false
	if i >= 0 {
		old := m.Files[i]
		page, _ := pageMetadata(old.Meta)
		legacy = page["schema_version"] == nil && page["name"] == nil
		if old.Source() == f.Source() {
			f.W, f.H, f.Dur = old.W, old.H, old.Dur
		}
		if f.Meta == nil {
			f.Meta = old.Meta
		}
		if !o.trusted {
			f.Meta = preservePageMeta(f.Meta, old.Meta)
		}
		if o.trusted && op.CreateID == "" && old.Source() == f.Source() {
			f.CreateID = old.CreateID
		}
		if old.Source() != f.Source() {
			f.Meta = clearArchiveState(f.Meta)
		} else if k.ArchiveDestination(f.Path) != "" {
			f.Meta = maps.Clone(f.Meta)
			if f.Meta == nil {
				f.Meta = map[string]any{}
			}
			for _, key := range archiveStateKeys {
				if value, ok := old.Meta[key]; ok {
					f.Meta[key] = value
				}
			}
		}
		f.Unattached = old.Unattached && op.Unattached
	} else if u.Max > 0 && o.count(g) >= u.Max {
		return tooManyFiles(u.Max, "kind %q allows at most %d uploads at %s", k.Name, u.Max, u.Path)
	}
	if k.IsPageUpload(f) && !legacy {
		f.Meta = withPageSource(f.Meta, k.NameOf(f.Path), source)
		if collision {
			f.Meta = pageCollisionMeta(f.Meta, k.NameOf(f.Path))
		}
	}
	// Keep the supplied source basename for every upload, including ZIP,
	// audio and video. The virtual path is cleaned independently and physical
	// object names never use this metadata. A rename retains this value.
	f.Meta = maps.Clone(f.Meta)
	if f.Meta == nil {
		f.Meta = map[string]any{}
	}
	if op.SourceName != "" || f.Meta["source_name"] == nil {
		f.Meta["source_name"] = path.Base(strings.ReplaceAll(source, "\\", "/"))
	}
	if f.Meta[media.MetaAutoCover] != nil {
		f.Meta = maps.Clone(f.Meta)
		delete(f.Meta, media.MetaAutoCover)
	}
	if metaBytes(f.Meta) > media.MaxMetaBytes {
		return uploadErr(media.CodeTooLarge, "page source metadata exceeds %d bytes", media.MaxMetaBytes)
	}
	if op.Edit != nil {
		if !isImageType(f.Type) {
			return uploadErr(media.CodeInvalid, "upload %q: only images take an edit", f.Path)
		}
		e, err := fitEdit(k.EditBounds(f.Path), op.Edit, f.W, f.H)
		if err != nil {
			return editErr(err, "upload %q: %v", f.Path)
		}
		f.Edit = e
	}
	if err := o.admitSource(&f, i); err != nil {
		return err
	}
	var cancelled []string
	if i >= 0 {
		if m.Files[i].Source() != f.Source() {
			cancelled = o.cancelledArchivePages(m.Files[i])
		}
		old := stripPending(m.Files[i])
		old.CreateID = f.CreateID
		if old.Key() == f.Key() {
			f.Public = old.Public
		}
		if reflect.DeepEqual(old, f) {
			m.Files[i].CreateID = f.CreateID
			return nil // unchanged source and edit
		}
	}
	f.Pending = k.Presets(f.Path, false)
	if err := o.place(g, i, f, op.Index); err != nil {
		return err
	}
	o.dropUploads(cancelled)
	if !f.Unattached && k.ArchiveDestination(f.Path) != "" {
		return o.attachArchiveBatch(f)
	}
	return nil
}

// fitEdit checks e against the upload's bounds and stores what the editor
// chose: a crop fitted to the aspect, or no crop (the producer centres one).
func fitEdit(bounds media.Image, e *media.Edit, w, h int) (*media.Edit, error) {
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

func stripPending(f media.File) media.File {
	f.Pending, f.Failed = nil, nil
	return f
}

// place puts upload f in group g: replacing file i (renaming its outputs'
// From when the extension changed), or inserted at index. A video it
// replaces has its frames grabbed again.
func (o *opRun) place(g, i int, f media.File, index *int) error {
	if err := o.admitSource(&f, i); err != nil {
		return err
	}
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
	if old.Source() != f.Source() {
		o.regrab(old.Path)
		// Retired producers cannot refresh retained legacy outputs. Keep them
		// only while the original source bytes stay unchanged.
		m.Files = slices.DeleteFunc(m.Files, func(out media.File) bool {
			return !out.IsUpload() && out.From == f.Path && o.k.private(out.Preset) == nil
		})
		m.index = nil
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
		if !f.IsUpload() || f.Frame == nil || f.Frame.From != nil {
			continue
		}
		if u, _, _, _, ok := k.upload(f.Path); ok && k.Uploads[u].Frames == k.Uploads[g].Path {
			f.Blob, f.W, f.H, f.Size, f.Failed = "", 0, 0, 0, nil
			f.Frame = &media.Frame{T: f.Frame.T, Auto: f.Frame.Auto}
			f.Pending = k.Presets(f.Path, false)
		}
	}
}

// frame sets an upload to a frame grab of its Upload.Frames video, or of
// the video op.From names.
func (o *opRun) frame(n int, op media.Op) error {
	k, m := o.k, o.m
	g, stem, _, _, ok := k.upload(op.Path)
	if op.From != nil {
		if !ok || !slices.Contains(k.Uploads[g].Types, "image/png") {
			return uploadErr(media.CodeType, "upload %q takes no frame: it does not allow image/png", op.Path)
		}
		v, ok := o.frames[n]
		if !ok { // this item's, read under its lock
			v, ok = uploadAt(m, op.From.Path)
			if err := frameVideo(v, ok, op); err != nil {
				return err
			}
		}
		from := &media.CopyFrom{Kind: cmpOr(op.From.Kind, k.Name), ID: op.From.ID, Version: op.From.Version, Path: v.Path}
		return o.placeFrame(g, stem, op, &media.Frame{Auto: op.Auto, From: from})
	}
	if !ok || k.Uploads[g].Frames == "" {
		return uploadErr(media.CodeInvalid, "upload %q takes no frames", op.Path)
	}
	video := -1
	for j, f := range m.Files {
		if s, _ := splitExt(f.Path); f.IsUpload() && s == k.Uploads[g].Frames {
			video = j
		}
	}
	if video < 0 {
		return uploadErr(media.CodeNotFound, "no video at %s to grab a frame from", k.Uploads[g].Frames)
	}
	if m.Files[video].Unattached {
		return uploadErr(media.CodeConflict, "the video at %s is unattached: attach it first", k.Uploads[g].Frames)
	}
	if v := m.Files[video]; op.T != nil && v.Dur > 0 && *op.T > v.Dur {
		return uploadErr(media.CodeInvalid, "frame at %gs is past the video's %gs", *op.T, v.Dur)
	}
	return o.placeFrame(g, stem, op, &media.Frame{Auto: op.Auto})
}

func (o *opRun) placeFrame(g int, stem string, op media.Op, frame *media.Frame) error {
	f := media.File{Path: stem + ".png", Type: "image/png", Frame: frame, Edit: op.Edit.Normalize()}
	if op.T != nil {
		f.Frame.T = *op.T
	}
	i := o.stem(stem)
	if i >= 0 {
		f.Meta, f.Unattached = o.m.Files[i].Meta, o.m.Files[i].Unattached
	}
	f.Pending = o.k.Presets(f.Path, false)
	return o.place(g, i, f, nil)
}

// uploadAt is the upload at path, or whose path's stem is path.
func uploadAt(m *Manifest, path string) (media.File, bool) {
	if f, ok := m.Get(path); ok {
		return f, ok
	}
	for _, f := range m.Files {
		if s, _ := splitExt(f.Path); f.IsUpload() && s == path {
			return f, true
		}
	}
	return media.File{}, false
}

// frameVideo checks the video upload v (found: ok) a frame op's From names.
func frameVideo(v media.File, ok bool, op media.Op) error {
	if !ok || !v.IsUpload() || !isVideoType(v.Type) || v.Gone || v.Fail() != nil {
		return uploadErr(media.CodeNotFound, "no video upload %q in item %s", op.From.Path, op.From.ID)
	}
	if v.Unattached {
		return uploadErr(media.CodeConflict, "the video %q is unattached: attach it first", v.Path)
	}
	if v.Blob == "" {
		return uploadErr(media.CodeConflict, "video %q is still being placed; retry", v.Path)
	}
	if op.T != nil && v.Dur > 0 && *op.T > v.Dur {
		return uploadErr(media.CodeInvalid, "frame at %gs is past the video's %gs", *op.T, v.Dur)
	}
	return nil
}

// copyFiles snapshots an upload and its derived files before an edit moves them.
func copyFiles(m *Manifest, path string) []media.File {
	f, ok := m.Get(path)
	if !ok || !f.IsUpload() || f.Blob == "" {
		return nil
	}
	files := []media.File{f}
	for _, out := range m.Files {
		if !out.IsUpload() && out.From == f.Path {
			files = append(files, out)
		}
	}
	return files
}

// copy preserves current outputs within an upload group; a different group
// derives its own outputs from the copied original.
func (o *opRun) copy(op media.Op, src []media.File) error {
	k, m := o.k, o.m
	if len(src) == 0 {
		return uploadErr(media.CodeNotFound, "no upload %q in item %s", op.From.Path, op.From.ID)
	}
	f := src[0]
	f.Public = nil    // copied originals never transfer another item's allocation
	f.Admission = nil // copied ZIPs must be admitted by their destination owner
	if op.Meta != nil {
		f.Meta = maps.Clone(op.Meta)
	} else if f.Meta[media.MetaAutoCover] != nil {
		f.Meta = maps.Clone(f.Meta)
		delete(f.Meta, media.MetaAutoCover)
	}
	to := op.To
	if to == "" {
		to = f.Path
	}
	if k.IsPageUpload(media.File{Path: to}) {
		stem, _ := splitExt(to)
		if o.stem(stem) >= 0 || pageNameUsed(k, m, k.NameOf(to)) {
			to = nextPagePath(k, m, to)
		}
	}
	if to != cmpOr(op.To, src[0].Path) && o.authorizePath != nil {
		if err := o.authorizePath(to); err != nil {
			return err
		}
	}
	g, stem, name, ext, ok := k.upload(to)
	if !ok {
		return uploadErr(media.CodeInvalid, "copy to %q: not an upload path", to)
	}
	if k.Uploads[g].Named && !media.ValidNamed(name) {
		return uploadErr(media.CodeInvalid, "upload %q: the server names %s uploads (presign)", to, k.Uploads[g].Path)
	}
	if err := allows(k.Uploads[g], f.Type, f.Size); err != nil {
		return err
	}
	if ext == "" {
		_, ext = splitExt(f.Path)
	}
	from := f.Path
	f.Path, f.Unattached, f.CreateID = stem+"."+ext, false, ""
	if k.IsPageUpload(f) || f.Meta["page"] != nil {
		// A copied page is a new source entry, not the old page's manual
		// choice or logical identity. Its bytes remain traceable by digest.
		f.Meta = maps.Clone(f.Meta)
		original := sourceName(f.Meta, from)
		delete(f.Meta, "page")
		f.Meta = withPageSource(f.Meta, k.NameOf(f.Path), original)
	}
	f.Meta = clearArchiveState(f.Meta)
	if err := o.admitSource(&f, -1); err != nil {
		return err
	}
	if metaBytes(f.Meta) > media.MaxMetaBytes {
		return uploadErr(media.CodeTooLarge, "page source metadata exceeds %d bytes", media.MaxMetaBytes)
	}
	group, _, _, _, _ := k.upload(from)
	// Two pages can belong to different chapters after an order edit. Their
	// derived allocations must therefore move independently. Same-item copies
	// retain the internal original and render fresh outputs for the new path.
	if k.Versions && op.From.ID == o.ref.ContentID && op.From.Version == o.ref.Version() {
		if f.Gone {
			return uploadErr(media.CodeNotFound, "upload %q: original is no longer available", from)
		}
		f.Pending, f.Failed, f.Editor = k.Presets(f.Path, false), nil, nil
		src = src[:1]
	}
	if g != group || op.Edit != nil {
		if f.Gone {
			return uploadErr(media.CodeNotFound, "upload %q: original is no longer available", from)
		}
		f.Edit, f.Frame, f.Failed = nil, nil, nil
		if op.Edit != nil {
			if !isImageType(f.Type) {
				return uploadErr(media.CodeInvalid, "upload %q: only images take an edit", f.Path)
			}
			e, err := fitEdit(k.EditBounds(f.Path), op.Edit, f.W, f.H)
			if err != nil {
				return editErr(err, "upload %q: %v", f.Path)
			}
			f.Edit = e
		}
		f.Pending = k.Presets(f.Path, false)
		src = src[:1]
	}
	i := o.stem(stem)
	var cancelled []string
	if i >= 0 && m.Files[i].Source() != f.Source() {
		cancelled = o.cancelledArchivePages(m.Files[i])
	}
	if i >= 0 && op.Meta[media.MetaAutoCover] == true && m.Files[i].Meta[media.MetaAutoCover] != true {
		return uploadErr(media.CodeConflict, "an editor owns cover %q", m.Files[i].Path)
	}
	if i < 0 && k.Uploads[g].Max > 0 && o.count(g) >= k.Uploads[g].Max {
		return tooManyFiles(k.Uploads[g].Max, "kind %q allows at most %d uploads at %s", k.Name, k.Uploads[g].Max, k.Uploads[g].Path)
	}
	// Private outputs come along current; public ones render here.
	for _, p := range k.PublicFor(f.Path) {
		if !slices.Contains(f.Pending, p.Name) {
			f.Pending = append(slices.Clone(f.Pending), p.Name)
		}
	}
	if i >= 0 {
		old := m.Files[i].Path
		m.Files = slices.DeleteFunc(m.Files, func(x media.File) bool { return !x.IsUpload() && x.From == old })
		i = o.stem(stem)
	}
	if err := o.place(g, i, f, nil); err != nil {
		return err
	}
	o.dropUploads(cancelled)
	for _, out := range src[1:] {
		out.From = f.Path
		if p := k.private(out.Preset); p != nil {
			out.Path = rebase(out.Path, k.OutputPath(p, from), k.OutputPath(p, f.Path))
		}
		if m.Find(out.Path) >= 0 {
			return uploadErr(media.CodeConflict, "copy: output %q exists", out.Path)
		}
		m.Files = append(m.Files, out)
		m.index = nil
	}
	return nil
}

// rename renames upload f and re-paths its outputs.
func (o *opRun) rename(f media.File, to string) error {
	k, m := o.k, o.m
	g, stem, _, ext, ok := k.upload(to)
	if gf, _, _, _, _ := k.upload(f.Path); !ok || g != gf {
		return uploadErr(media.CodeInvalid, "rename %q to %q: not a path of the same upload", f.Path, to)
	}
	if ext == "" {
		_, ext = splitExt(f.Path)
	}
	to = stem + "." + ext
	if to == f.Path {
		return nil
	}
	if o.stem(stem) >= 0 {
		return uploadErr(media.CodeConflict, "upload %q exists", to)
	}
	if k.IsPageUpload(f) {
		name := k.NameOf(to)
		for _, other := range m.Files {
			if other.Path == f.Path || !k.IsPageUpload(other) {
				continue
			}
			if strings.EqualFold(k.NameOf(other.Path), name) {
				return uploadErr(media.CodeConflict, "upload %q exists", to)
			}
			if page, err := pageMetadata(other.Meta); err == nil {
				if prior, ok := page["name"].(string); ok && strings.EqualFold(prior, name) {
					return uploadErr(media.CodeConflict, "upload %q exists", to)
				}
			}
		}
	}
	if k.Uploads[g].Named {
		return uploadErr(media.CodeInvalid, "named uploads keep their name")
	}
	if o.renames == nil {
		o.renames = map[string]string{}
		for _, file := range m.Files {
			if file.IsUpload() {
				o.renames[file.Path] = file.Path
			}
		}
	}
	for before, after := range o.renames {
		if after == f.Path {
			o.renames[before] = to
		}
	}
	for j := range m.Files {
		x := &m.Files[j]
		switch {
		case x.Path == f.Path:
			x.Path = to
			if k.IsPageUpload(*x) {
				if page, err := pageMetadata(x.Meta); err == nil {
					page["name"] = k.NameOf(to)
					x.Meta = maps.Clone(x.Meta)
					if x.Meta == nil {
						x.Meta = map[string]any{}
					}
					x.Meta["page"] = page
				}
			}
		case !x.IsUpload() && x.From == f.Path:
			x.From = to
			if p := k.private(x.Preset); p != nil {
				x.Path = rebase(x.Path, k.OutputPath(p, f.Path), k.OutputPath(p, to))
			}
		case x.IsUpload() && x.Meta[media.MetaFor] == f.Path:
			x.Meta = maps.Clone(x.Meta)
			x.Meta[media.MetaFor] = to
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

// created checks a create-only put against the current manifest. A retry
// preserves later changes unless a failed upload supplies a new source.
func (o *opRun) created(op media.Op) (bool, error) {
	if op.CreateID == "" {
		return false, nil
	}
	if o.k.IsPageUpload(media.File{Path: op.Path}) {
		for _, f := range o.m.Files {
			if o.k.IsPageUpload(f) && f.CreateID == op.CreateID {
				return f.Fail() == nil || f.Source() == op.Blob, nil
			}
		}
		return false, nil // a new create may receive a collision-safe path
	}
	stem, _ := splitExt(op.Path)
	i := o.stem(stem)
	if i < 0 {
		return false, nil
	}
	f := o.m.Files[i]
	if f.CreateID != op.CreateID {
		return false, uploadErr(media.CodeConflict, "upload %q already exists", op.Path)
	}
	return f.Fail() == nil || f.Source() == op.Blob, nil
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
func (o *opRun) insert(g int, f media.File, index *int, attached bool) {
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
func allows(u media.Upload, contentType string, size int64) error {
	if !slices.Contains(u.Types, contentType) {
		return &media.UploadError{Code: media.CodeType, Message: fmt.Sprintf("%s files are not allowed at %s; allowed: %s", contentType, u.Path, strings.Join(u.Types, ", ")),
			Details: &media.ErrorDetails{Type: contentType, Allowed: u.Types}}
	}
	if size <= 0 || size > u.MaxBytes {
		return &media.UploadError{Code: media.CodeTooLarge, Message: fmt.Sprintf("%s files at %s may be at most %d bytes; this one is %d", contentType, u.Path, u.MaxBytes, size),
			Details: &media.ErrorDetails{Type: contentType, Size: size, MaxBytes: u.MaxBytes}}
	}
	return nil
}
