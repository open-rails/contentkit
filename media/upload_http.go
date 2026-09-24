package media

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// UploadHandlerOptions configure UploadHandler.
type UploadHandlerOptions struct {
	Tenant string                                   // every ref is scoped to it
	Actor  func(*http.Request) (access.Actor, bool) // the host's authenticated caller; false answers 401
	Logger *slog.Logger                             // 5xx causes; default slog.Default()
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
//	POST /commit-slot  SlotBody     -> 204
//	POST /commit-slot-from-file  SlotFromFileBody -> 204
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
	mux.HandleFunc("POST /commit-slot", h.commitSlot)
	mux.HandleFunc("POST /commit-slot-from-file", h.slotFromFile)
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
	SHA256 string  `json:"sha256,omitempty"` // required up to 64 MiB and for slots
	Slot   string  `json:"slot,omitempty"`
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
type PresignReply struct {
	Name      string          `json:"name"`
	Exists    bool            `json:"exists,omitempty"`
	Put       *RequestReply   `json:"put,omitempty"`
	Multipart *MultipartReply `json:"multipart,omitempty"`
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
	Name     string         `json:"name"`
	Original string         `json:"original"`
	Type     string         `json:"type,omitempty"`
	Size     int64          `json:"size,omitempty"`
	Edit     *Edit          `json:"edit,omitempty"`
	Meta     map[string]any `json:"meta,omitempty"`
}

// CommitReply is the committed file order.
type CommitReply struct {
	Files []CommitFile `json:"files"`
}

type SlotBody struct {
	Ref    RefBody `json:"ref"`
	Slot   string  `json:"slot"`
	SHA256 string  `json:"sha256"`
}

// SlotFromFileBody makes File (a manifest image of Ref) the slot's original,
// through Edit (default: the file's edit; {} clears it).
type SlotFromFileBody struct {
	Ref  RefBody `json:"ref"`
	Slot string  `json:"slot"`
	File string  `json:"file"`
	Edit *Edit   `json:"edit,omitempty"`
}

// ErrorReply is the error body; Code is one of the media Code* constants,
// "unauthorized" or "internal_error".
type ErrorReply struct {
	Error      string   `json:"error"`
	Code       string   `json:"code"`
	RetryAfter int      `json:"retry_after,omitempty"` // seconds, with 429
	Originals  []string `json:"originals,omitempty"`   // not_uploaded at commit: the originals to upload again
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
	req := PresignRequest{Ref: h.ref(b.Ref), Type: b.Type, Size: b.Size, Slot: b.Slot}
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
	out := PresignReply{Name: p.Name, Exists: p.Exists}
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
		out.Files[i] = CommitFile{Name: f.Name, Original: f.Original, Type: f.Type, Size: f.Size, Edit: f.Edit, Meta: f.Meta}
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
	if err := h.u.CommitSlot(r.Context(), actor, h.ref(b.Ref), b.Slot, sum); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h uploadHandler) slotFromFile(w http.ResponseWriter, r *http.Request) {
	var b SlotFromFileBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	if err := h.u.SetSlotFromFile(r.Context(), actor, h.ref(b.Ref), b.Slot, b.File, b.Edit); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		if errors.Is(err, ErrManifestConflict) {
			ue = &UploadError{Code: CodeConflict, Message: "manifest kept changing; retry"}
		} else {
			h.o.Logger.ErrorContext(r.Context(), "media upload", "method", r.Method, "path", r.URL.Path, "error", err)
			writeJSON(w, http.StatusInternalServerError, ErrorReply{Error: "internal error", Code: "internal_error"})
			return
		}
	}
	out := ErrorReply{Error: ue.Message, Code: ue.Code, Originals: ue.Originals}
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
