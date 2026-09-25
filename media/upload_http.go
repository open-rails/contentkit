package media

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// UploadHandlerOptions configure UploadHandler.
type UploadHandlerOptions struct {
	Tenant string                                   // every ref is scoped to it
	Actor  func(*http.Request) (access.Actor, bool) // the host's authenticated caller; false answers 401
	Logger *slog.Logger                             // 5xx causes; default slog.Default()
	// Reader builds slot and video-image reply URLs (the access worker
	// origin; editor tokens for unpublished video posters); required for
	// slot and video routes.
	Reader *Reader
}

// UploadHandler serves the upload API the browser SDK calls. All routes are
// POST with JSON bodies; SHA-256 values are lowercase hex. Errors are
// ErrorReply with the status of its code (Retry-After on 429).
//
//	POST /presign      PresignBody  -> PresignReply
//	POST /parts        PartsBody    -> PartsReply     presign multipart parts
//	POST /parts/list   TicketBody   -> PartsReply     parts that landed (resume)
//	POST /complete     TicketBody   -> CompleteReply
//	POST /abort        TicketBody   -> 204
//	POST /commit       CommitBody   -> CommitReply
//	POST /files        FilesBody    -> FilesReply     an editor's files, unattached ones included: processing state and progress
//	POST /commit-slot            SlotBody         -> SlotManifest (204 for an inline image)
//	POST /commit-slot-from-file  SlotFromFileBody -> SlotManifest
//	POST /edit-slot              SlotEditBody     -> SlotManifest   re-edit the committed original
//	POST /slot                   SlotRefBody      -> SlotManifest
//	POST /slot-original          SlotRefBody      -> the committed original's bytes (editor)
//	POST /video-images   VideoImagesBody  -> VideoImages   poster, with selections
//	POST /video-poster   VideoPosterBody  -> VideoImages
//	GET  /frame?kind=&id=&version=&file=&t=&w= -> image/jpeg   poster picker frame (UploadOptions.Frames)
func UploadHandler(u *Uploads, o UploadHandlerOptions) http.Handler {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	h := uploadHandler{u, o}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /presign", h.presign)
	mux.HandleFunc("POST /parts", h.parts)
	mux.HandleFunc("POST /parts/list", h.listParts)
	mux.HandleFunc("POST /complete", h.complete)
	mux.HandleFunc("POST /abort", h.abort)
	mux.HandleFunc("POST /commit", h.commit)
	mux.HandleFunc("POST /files", h.files)
	mux.HandleFunc("POST /commit-slot", h.commitSlot)
	mux.HandleFunc("POST /commit-slot-from-file", h.slotFromFile)
	mux.HandleFunc("POST /edit-slot", h.editSlot)
	mux.HandleFunc("POST /slot", h.slot)
	mux.HandleFunc("POST /slot-original", h.slotOriginal)
	mux.HandleFunc("POST /video-images", h.videoImages)
	mux.HandleFunc("POST /video-poster", h.videoPoster)
	mux.HandleFunc("GET /frame", h.frame)
	return mux
}

// RefBody names an item within the handler's tenant.
type RefBody struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
}

type PresignBody struct {
	Ref    RefBody `json:"ref"`
	Type   string  `json:"type"`
	Size   int64   `json:"size"`
	SHA256 string  `json:"sha256,omitempty"` // required up to 64 MiB, for slots and inline images
	Slot   string  `json:"slot,omitempty"`
	Inline bool    `json:"inline,omitempty"` // a new inline image; the reply names it
}

// RequestReply is a presigned request: send exactly these headers (the
// browser adds Content-Length, which is signed too).
type RequestReply struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Expires time.Time         `json:"expires"`
}

type MultipartReply struct {
	Ticket      string `json:"ticket"`
	MinPartSize int64  `json:"min_part_size"`
	MaxPartSize int64  `json:"max_part_size"`
	MaxParts    int    `json:"max_parts"`
}

// PresignReply: exists (commit directly), put (one PUT) or multipart.
// ProcessOnUpload asks the client to commit the file unattached as soon as
// it is uploaded (UploadOptions.ProcessOnUpload).
type PresignReply struct {
	Name            string          `json:"name"`
	Exists          bool            `json:"exists,omitempty"`
	Put             *RequestReply   `json:"put,omitempty"`
	Multipart       *MultipartReply `json:"multipart,omitempty"`
	ProcessOnUpload bool            `json:"process_on_upload,omitempty"`
}

type PartBody struct {
	Number int32  `json:"number"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type PartsBody struct {
	Ticket string     `json:"ticket"`
	Parts  []PartBody `json:"parts"`
}

type TicketBody struct {
	Ticket string `json:"ticket"`
}

// PartReply is a presigned part (parts) or a landed part (parts/list).
type PartReply struct {
	Number  int32         `json:"number"`
	Size    int64         `json:"size,omitempty"`
	SHA256  string        `json:"sha256,omitempty"`
	Request *RequestReply `json:"request,omitempty"`
}

type PartsReply struct {
	Parts []PartReply `json:"parts"`
}

type CompleteReply struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type CommitBody struct {
	Ref RefBody `json:"ref"`
	Ops []Op    `json:"ops"`
}

type CommitFile struct {
	Name       string         `json:"name"`
	Original   string         `json:"original"`
	Type       string         `json:"type,omitempty"`
	Size       int64          `json:"size,omitempty"`
	Edit       *Edit          `json:"edit,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
	Unattached bool           `json:"unattached,omitempty"`
}

// CommitReply is the committed file order.
type CommitReply struct {
	Files []CommitFile `json:"files"`
}

// SlotBody commits an uploaded slot (or inline image) original. Edit's crop
// is in the EXIF-oriented original's pixels, its height derived from its
// width at the slot's aspect; omitted crops centred at the aspect.
type SlotBody struct {
	Ref    RefBody `json:"ref"`
	Slot   string  `json:"slot"`
	SHA256 string  `json:"sha256"`
	Edit   *Edit   `json:"edit,omitempty"`
}

// SlotEditBody re-edits the committed original; omitted crops centred.
type SlotEditBody struct {
	Ref  RefBody `json:"ref"`
	Slot string  `json:"slot"`
	Edit *Edit   `json:"edit,omitempty"`
}

type SlotRefBody struct {
	Ref  RefBody `json:"ref"`
	Slot string  `json:"slot"`
}

// SlotFromFileBody makes File (a manifest image of From, default Ref) the
// slot's original, through Edit (default: the file's edit; {} clears it).
type SlotFromFileBody struct {
	Ref  RefBody  `json:"ref"`
	Slot string   `json:"slot"`
	From *RefBody `json:"from,omitempty"`
	File string   `json:"file"`
	Edit *Edit    `json:"edit,omitempty"`
}

// VideoImagesBody names a video item; with a version (versioned kinds) the
// reply describes its file (file, default the first video file).
type VideoImagesBody struct {
	Ref  RefBody `json:"ref"`
	File string  `json:"file,omitempty"`
}

// VideoPosterBody selects the poster. source "frame" needs time (seconds, in
// the video) and a ref version for versioned kinds, its edit in the grabbed
// frame's pixels (VideoInfo w×h); "upload" needs the sha256 of the image
// presigned with slot "poster", its edit in that image's pixels; "auto"
// returns to the default. Omitted edits keep the whole image (native aspect).
type VideoPosterBody struct {
	Ref    RefBody  `json:"ref"`
	Source string   `json:"source"`
	File   string   `json:"file,omitempty"`
	Time   *float64 `json:"time,omitempty"`
	SHA256 string   `json:"sha256,omitempty"`
	Edit   *Edit    `json:"edit,omitempty"`
}

// ErrorReply is the error body; Code is one of the media Code* constants,
// "unauthorized" or "internal_error".
type ErrorReply struct {
	Error      string        `json:"error"`
	Code       string        `json:"code"`
	RetryAfter int           `json:"retry_after,omitempty"` // seconds, with 429
	Originals  []string      `json:"originals,omitempty"`   // not_uploaded at commit: the originals to upload again
	Details    *ErrorDetails `json:"details,omitempty"`     // image refusals
}

type uploadHandler struct {
	u *Uploads
	o UploadHandlerOptions
}

func (h uploadHandler) presign(w http.ResponseWriter, r *http.Request) {
	var b PresignBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	req := PresignRequest{Ref: h.ref(b.Ref), Type: b.Type, Size: b.Size, Slot: b.Slot, Inline: b.Inline}
	if b.SHA256 != "" {
		if req.SHA256, ok = h.digest(w, b.SHA256); !ok {
			return
		}
	}
	p, err := h.u.Presign(r.Context(), actor, req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := PresignReply{Name: p.Name, Exists: p.Exists, ProcessOnUpload: p.ProcessOnUpload}
	if p.Put != nil {
		out.Put = request(*p.Put)
	}
	if m := p.Multipart; m != nil {
		out.Multipart = &MultipartReply{Ticket: m.Ticket, MinPartSize: m.MinPartSize, MaxPartSize: m.MaxPartSize, MaxParts: m.MaxParts}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h uploadHandler) parts(w http.ResponseWriter, r *http.Request) {
	var b PartsBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	reqs := make([]PartRequest, len(b.Parts))
	for i, p := range b.Parts {
		reqs[i] = PartRequest{Number: p.Number, Size: p.Size}
		if reqs[i].SHA256, ok = h.digest(w, p.SHA256); !ok {
			return
		}
	}
	signed, err := h.u.PresignParts(r.Context(), actor, b.Ticket, reqs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := PartsReply{Parts: make([]PartReply, len(signed))}
	for i, p := range signed {
		out.Parts[i] = PartReply{Number: p.Number, Request: request(p.PresignedRequest)}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h uploadHandler) listParts(w http.ResponseWriter, r *http.Request) {
	var b TicketBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	parts, err := h.u.ListParts(r.Context(), actor, b.Ticket)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := PartsReply{Parts: make([]PartReply, len(parts))}
	for i, p := range parts {
		out.Parts[i] = PartReply{Number: p.Number, Size: p.Size, SHA256: hex.EncodeToString(p.SHA256)}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h uploadHandler) complete(w http.ResponseWriter, r *http.Request) {
	var b TicketBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	obj, err := h.u.Complete(r.Context(), actor, b.Ticket)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, CompleteReply{Name: obj.Name, Type: obj.Type, Size: obj.Size})
}

func (h uploadHandler) abort(w http.ResponseWriter, r *http.Request) {
	var b TicketBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	if err := h.u.Abort(r.Context(), actor, b.Ticket); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h uploadHandler) commit(w http.ResponseWriter, r *http.Request) {
	var b CommitBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	man, err := h.u.Commit(r.Context(), actor, h.ref(b.Ref), b.Ops)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := CommitReply{Files: make([]CommitFile, len(man.Files))}
	for i, f := range man.Files {
		out.Files[i] = CommitFile{Name: f.Name, Original: f.Original, Type: f.Type, Size: f.Size, Edit: f.Edit, Meta: f.Meta, Unattached: f.Unattached}
	}
	writeJSON(w, http.StatusOK, out)
}

// FilesBody names files of an item; empty names lists them all.
type FilesBody struct {
	Ref   RefBody  `json:"ref"`
	Names []string `json:"names,omitempty"`
}

// FilesReply is the named files as an editor reads them (unattached ones
// included): dimensions once derived, hls once encoded, failed, progress.
type FilesReply struct {
	Files []FileInfo `json:"files"`
}

func (h uploadHandler) files(w http.ResponseWriter, r *http.Request) {
	var b FilesBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	if h.o.Reader == nil {
		h.fail(w, r, uploadErr(CodeNotFound, "files are not served here"))
		return
	}
	res, err := h.o.Reader.Read(r.Context(), h.ref(b.Ref), actor, ReadOptions{Unattached: true, Limit: h.o.Reader.maxLimit})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := FilesReply{Files: []FileInfo{}}
	for _, f := range res.Files {
		if len(b.Names) == 0 || slices.Contains(b.Names, f.Name) {
			out.Files = append(out.Files, f)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h uploadHandler) commitSlot(w http.ResponseWriter, r *http.Request) {
	var b SlotBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	sum, ok := h.digest(w, b.SHA256)
	if !ok {
		return
	}
	err := h.u.CommitSlot(r.Context(), actor, h.ref(b.Ref), b.Slot, sum, b.Edit)
	if err == nil && layout.ValidInlineName(b.Slot) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.slotReply(w, r, b.Ref, b.Slot, err)
}

func (h uploadHandler) slotFromFile(w http.ResponseWriter, r *http.Request) {
	var b SlotFromFileBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	req := SlotFromFile{Ref: h.ref(b.Ref), Slot: b.Slot, File: b.File, Edit: b.Edit}
	if b.From != nil {
		req.From = h.ref(*b.From)
	}
	h.slotReply(w, r, b.Ref, b.Slot, h.u.SetSlotFromFile(r.Context(), actor, req))
}

func (h uploadHandler) editSlot(w http.ResponseWriter, r *http.Request) {
	var b SlotEditBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	h.slotReply(w, r, b.Ref, b.Slot, h.u.EditSlot(r.Context(), actor, h.ref(b.Ref), b.Slot, b.Edit))
}

func (h uploadHandler) slot(w http.ResponseWriter, r *http.Request) {
	var b SlotRefBody
	if _, ok := h.read(w, r, &b); ok {
		h.slotReply(w, r, b.Ref, b.Slot, nil)
	}
}

func (h uploadHandler) slotOriginal(w http.ResponseWriter, r *http.Request) {
	var b SlotRefBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	rc, obj, err := h.u.SlotOriginal(r.Context(), actor, h.ref(b.Ref), b.Slot)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", obj.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, rc)
}

// slotReply answers a slot route with the slot's manifest once err is nil.
func (h uploadHandler) slotReply(w http.ResponseWriter, r *http.Request, ref RefBody, slot string, err error) {
	var urls OutputURLs
	if err == nil {
		urls, err = h.urls(ref)
	}
	var m SlotManifest
	if err == nil {
		m, err = h.u.o.Manifests.SlotManifest(r.Context(), urls, h.ref(ref), slot)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// urls are an uploader's reply URLs: uploaders edit the item.
func (h uploadHandler) urls(ref RefBody) (OutputURLs, error) {
	if h.o.Reader == nil {
		return OutputURLs{}, errors.New("media: UploadHandlerOptions.Reader is required for slot and video routes")
	}
	return h.o.Reader.EditorURLs(h.ref(ref))
}

func (h uploadHandler) videoImages(w http.ResponseWriter, r *http.Request) {
	var b VideoImagesBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	_, err := h.u.authorize(r.Context(), actor, h.ref(b.Ref).Content())
	h.videoReply(w, r, b.Ref, b.File, err)
}

func (h uploadHandler) videoPoster(w http.ResponseWriter, r *http.Request) {
	var b VideoPosterBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	req := PosterRequest{Source: b.Source, File: b.File, Edit: b.Edit}
	switch b.Source {
	case PosterSourceFrame:
		if b.Time == nil {
			writeJSON(w, http.StatusBadRequest, ErrorReply{Error: "a frame poster needs time", Code: CodeInvalid})
			return
		}
		req.Time = *b.Time
	case PosterSourceUpload:
		if req.SHA256, ok = h.digest(w, b.SHA256); !ok {
			return
		}
	}
	h.videoReply(w, r, b.Ref, b.File, h.u.SetVideoPoster(r.Context(), actor, h.ref(b.Ref), req))
}

func (h uploadHandler) videoReply(w http.ResponseWriter, r *http.Request, ref RefBody, file string, err error) {
	var urls OutputURLs
	if err == nil {
		urls, err = h.urls(ref)
	}
	var v VideoImages
	if err == nil {
		v, err = h.u.o.Manifests.VideoImages(r.Context(), urls, h.ref(ref), true, file)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h uploadHandler) frame(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.o.Actor(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, ErrorReply{Error: "authentication required", Code: "unauthorized"})
		return
	}
	q := r.URL.Query()
	t, err := strconv.ParseFloat(q.Get("t"), 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorReply{Error: "t must be seconds", Code: CodeInvalid})
		return
	}
	width := 0
	if v := q.Get("w"); v != "" {
		if width, err = strconv.Atoi(v); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorReply{Error: "w must be pixels", Code: CodeInvalid})
			return
		}
	}
	ref := h.ref(RefBody{Kind: q.Get("kind"), ID: q.Get("id"), Version: q.Get("version")})
	jpeg, err := h.u.Frame(r.Context(), actor, ref, q.Get("file"), t, width)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(jpeg)))
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(jpeg)
}

func (h uploadHandler) read(w http.ResponseWriter, r *http.Request, v any) (access.Actor, bool) {
	actor, ok := h.o.Actor(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, ErrorReply{Error: "authentication required", Code: "unauthorized"})
		return actor, false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorReply{Error: "invalid JSON body: " + err.Error(), Code: CodeInvalid})
		return actor, false
	}
	return actor, true
}

func (h uploadHandler) digest(w http.ResponseWriter, s string) ([]byte, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		writeJSON(w, http.StatusBadRequest, ErrorReply{Error: "sha256 must be 64 hex characters", Code: CodeInvalid})
		return nil, false
	}
	return b, true
}

func (h uploadHandler) ref(b RefBody) contentref.ContentRef {
	return contentref.NewVersion(h.o.Tenant, b.Kind, b.ID, b.Version)
}

func (h uploadHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	ue, ok := AsUploadError(err)
	if !ok {
		switch {
		case errors.Is(err, ErrManifestConflict):
			ue = &UploadError{Code: CodeConflict, Message: "manifest kept changing; retry"}
		case errors.Is(err, ErrNotVisible):
			ue = &UploadError{Code: CodeNotFound, Message: err.Error()}
		default:
			h.o.Logger.ErrorContext(r.Context(), "media upload", "method", r.Method, "path", r.URL.Path, "error", err)
			writeJSON(w, http.StatusInternalServerError, ErrorReply{Error: "internal error", Code: "internal_error"})
			return
		}
	}
	out := ErrorReply{Error: ue.Message, Code: ue.Code, Originals: ue.Originals, Details: ue.Details}
	if ue.RetryAfter > 0 {
		out.RetryAfter = int(math.Ceil(ue.RetryAfter.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(out.RetryAfter))
	}
	writeJSON(w, ue.Status(), out)
}

func request(p PresignedRequest) *RequestReply {
	h := make(map[string]string, len(p.Header))
	for k := range p.Header {
		h[k] = p.Header.Get(k)
	}
	return &RequestReply{Method: p.Method, URL: p.URL, Headers: h, Expires: p.Expires}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
