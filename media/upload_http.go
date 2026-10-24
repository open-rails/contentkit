package media

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// UploadHandlerOptions configure UploadHandler.
type UploadHandlerOptions struct {
	// Auth says who a request is (authkit/auth; AuthKit's
	// ak.Authenticator()): every route takes a signed-in person. Required.
	Auth   auth.Authenticator
	Logger *slog.Logger // 5xx causes; default slog.Default()
}

// UploadHandler serves the upload API the browser SDK calls (uploadRoutes).
// The engine serves it as the upload route group (/v1/media/upload). It panics without UploadHandlerOptions.Auth.
// Errors are ErrorReply with the status of its code (Retry-After on 429).
func UploadHandler(u *Uploads, o UploadHandlerOptions) http.Handler {
	if err := gate.Check(o.Auth); err != nil {
		panic("media: UploadHandlerOptions.Auth: " + err.Error())
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	mux := http.NewServeMux()
	g := &gate.Gate{Auth: o.Auth, Log: o.Logger}
	httpapi.Mount(mux, g.Guard, uploadHandler{u, o}, uploadRoutes)
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
	out := PresignReply{Path: p.Path, SourceName: p.SourceName, Blob: p.Blob, Exists: p.Exists, ProcessOnUpload: p.ProcessOnUpload}
	if p.Put != nil {
		out.Put = request(*p.Put)
	}
	if m := p.Multipart; m != nil {
		out.Multipart = &MultipartReply{Ticket: m.Ticket, MinPartSize: m.MinPartSize, MaxPartSize: m.MaxPartSize, MaxParts: m.MaxParts}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h uploadHandler) copy(w http.ResponseWriter, r *http.Request) {
	var b CopyBody
	actor, ok := h.read(w, r, &b)
	if !ok {
		return
	}
	ref, err := h.ref(b.Ref)
	var staged []StagedCopy
	if err == nil {
		staged, err = h.u.StageCopies(r.Context(), actor, ref, b.Files)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, CopyReply{Files: staged})
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
		man, err = h.u.Commit(r.Context(), actor, ref, b.OperationID, b.Ops)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := CommitReply{Files: []FileInfo{}}
	kind, _ := h.u.reg.Kind(ref.ContentKind)
	for _, f := range man.Files {
		if f.IsUpload() {
			out.Files = append(out.Files, uploadInfo(f, kind))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// uploadInfo is an upload as an editor reads it, without URLs.
func uploadInfo(f File, k *Kind) FileInfo {
	pending := slices.DeleteFunc(slices.Clone(f.Pending), func(name string) bool { return k.private(name) == nil && k.public(name) == nil })
	if statusArchivePending(k, f) {
		pending = append(pending, "extract")
	}
	failed := f.Fail()
	if k.audioOriginal(f) {
		failed = nil
	}
	return FileInfo{Path: f.Path, Type: f.Type, Size: f.Size, W: f.W, H: f.H, Dur: f.Dur, Upload: true,
		PageID: k.PageID(f), CreateID: f.CreateID, Staged: f.Staged != "", Edit: f.Edit, Frame: f.Frame, Meta: f.Meta, Unattached: f.Unattached, Pending: pending, Failed: failed}
}

func (h uploadHandler) frame(w http.ResponseWriter, r *http.Request) {
	actor := gate.Actor(r.Context())
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
	ref, err := h.ref(RefBody{Kind: q.Get("kind"), ID: q.Get("id"), Version: q.Get("version")})
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
	actor := gate.Actor(r.Context())
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorReply{Error: httpapi.BodyError(err), Code: CodeInvalid})
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

// ref is b's item; a ref the registry refuses is the caller's mistake.
func (h uploadHandler) ref(b RefBody) (contentref.ContentRef, error) {
	ref, err := h.u.reg.VersionRef(b.Kind, b.ID, b.Version)
	switch {
	case errors.Is(err, ErrUnknownKind):
		return ref, uploadErr(CodeNotFound, "%v", err)
	case err != nil:
		return ref, uploadErr(CodeInvalid, "%v", err)
	}
	return ref, nil
}

func (h uploadHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	ue, ok := AsUploadError(err)
	if !ok {
		switch {
		case errors.Is(err, ErrUpgradeRequired):
			ue = &UploadError{Code: CodeUpgrade, Message: "this item was stored before v0.68; migrate it with ContentKit v0.119.1 first"}
		case errors.Is(err, ErrCommitPending), errors.Is(err, ErrManifestConflict):
			ue = &UploadError{Code: CodeUnavailable, Message: "commit recovery is pending; retry the same operation_id", RetryAfter: time.Second}
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
