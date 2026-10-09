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
	Actor  func(*http.Request) (access.Actor, bool) // the host's authenticated caller; false answers 401
	Logger *slog.Logger                             // 5xx causes; default slog.Default()
}

// UploadHandler serves the upload API the browser SDK calls. Every route
// but GET /frame is POST with a JSON body. Errors are ErrorReply with the
// status of its code (Retry-After on 429).
//
//	POST /presign     PresignBody -> PresignReply
//	POST /parts       PartsBody   -> PartsReply   presign multipart parts
//	POST /parts/list  TicketBody  -> PartsReply   parts that landed (resume)
//	POST /complete    TicketBody  -> CompleteReply
//	POST /abort       TicketBody  -> 204
//	POST /commit      CommitBody  -> CommitReply
//	GET  /frame?kind=&id=&path=&t=&w= -> image/jpeg   a still of a video upload (UploadOptions.Frames)
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
	mux.HandleFunc("GET /frame", h.frame)
	return mux
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
	sum, ok := h.digest(w, b.SHA256)
	if !ok {
		return
	}
	ref, err := h.ref(b.Ref)
	var p Presigned
	if err == nil {
		p, err = h.u.Presign(r.Context(), actor, PresignRequest{Ref: ref, Path: b.Path, Type: b.Type, Size: b.Size, SHA256: sum})
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := PresignReply{Path: p.Path, Blob: p.Blob, Exists: p.Exists, ProcessOnUpload: p.ProcessOnUpload}
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
	done, err := h.u.Complete(r.Context(), actor, b.Ticket)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, CompleteReply(done))
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
	ref, err := h.ref(b.Ref)
	var man *Manifest
	if err == nil {
		man, err = h.u.Commit(r.Context(), actor, ref, b.Ops)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := CommitReply{Files: []FileInfo{}}
	for _, f := range man.Files {
		if f.IsUpload() {
			out.Files = append(out.Files, uploadInfo(f))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// uploadInfo is an upload as an editor reads it, without URLs.
func uploadInfo(f File) FileInfo {
	return FileInfo{Path: f.Path, Type: f.Type, Size: f.Size, W: f.W, H: f.H, Dur: f.Dur, Upload: true,
		Staged: f.Staged != "", Edit: f.Edit, Frame: f.Frame, Meta: f.Meta, Unattached: f.Unattached, Pending: f.Pending, Failed: f.Fail()}
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
	ref, err := h.ref(RefBody{Kind: q.Get("kind"), ID: q.Get("id")})
	var jpeg []byte
	if err == nil {
		jpeg, err = h.u.Frame(r.Context(), actor, ref, q.Get("path"), t, width)
	}
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

func (h uploadHandler) ref(b RefBody) (contentref.ContentRef, error) {
	return h.u.reg.Ref(b.Kind, b.ID)
}

func (h uploadHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	ue, ok := AsUploadError(err)
	if !ok {
		switch {
		case errors.Is(err, ErrManifestConflict):
			ue = &UploadError{Code: CodeConflict, Message: "manifest kept changing; retry"}
		case errors.Is(err, ErrUnavailable):
			h.o.Logger.WarnContext(r.Context(), "media upload", "method", r.Method, "path", r.URL.Path, "error", err)
			ue = &UploadError{Code: CodeUnavailable, Message: "media storage is unavailable; retry", RetryAfter: 5 * time.Second}
		case errors.Is(err, ErrConditionalPutRequired):
			h.o.Logger.ErrorContext(r.Context(), "media upload", "method", r.Method, "path", r.URL.Path, "error", err)
			ue = &UploadError{Code: CodeUnavailable, Message: "media storage does not support conditional writes"}
		default:
			h.o.Logger.ErrorContext(r.Context(), "media upload", "method", r.Method, "path", r.URL.Path, "error", err)
			writeJSON(w, http.StatusInternalServerError, ErrorReply{Error: "internal error", Code: "internal_error"})
			return
		}
	}
	out := ErrorReply{Error: ue.Message, Code: ue.Code, Blobs: ue.Blobs, Details: ue.Details}
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
