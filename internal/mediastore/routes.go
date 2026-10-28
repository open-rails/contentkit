package mediastore

import (
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/httpapi"
	media "github.com/open-rails/contentkit/media"
)

func init() {
	httpapi.Register(httpapi.Upload, uploadRoutes)
	httpapi.Register(httpapi.Media, readRoutes)
}

var refQuery = []httpapi.Param{httpapi.Text("kind", "the item's kind"), httpapi.Text("id", "the item's id"),
	httpapi.Text("version", "a content kind's version id; absent for any other kind's item")}

var uploadRoutes = []httpapi.Route[uploadHandler]{
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/presign", Resource: "uploads", Auth: httpapi.Editor, Perm: string(access.ContentWrite),
		Doc:       "Plans one upload after checking its path's rules and CanUpload: exists (commit directly), one PUT, or a multipart ticket.",
		Request:   media.PresignBody{},
		Responses: []httpapi.Reply{httpapi.OK(media.PresignReply{})},
		Errors:    []string{media.CodeConflict, media.CodeForbidden, media.CodeNotFound, media.CodeQuota, media.CodeRate, media.CodeTooLarge, media.CodeTooManyFiles, media.CodeType}},
		Serve: httpapi.H(uploadHandler.presign)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/copy", Resource: "uploads", Auth: httpapi.Staff, Perm: string(access.ImportWrite),
		Doc:       "Stages server-side copies of keys of the item's bucket outside ContentKit's namespaces, for one commit's puts (an importer's files).",
		Request:   media.CopyBody{},
		Responses: []httpapi.Reply{httpapi.OK(media.CopyReply{})},
		Errors:    []string{media.CodeForbidden, media.CodeNotFound, media.CodeQuota, media.CodeRate, media.CodeTooLarge, media.CodeType}},
		Serve: httpapi.H(uploadHandler.copy)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/parts", Resource: "uploads", Auth: httpapi.Editor, Perm: string(access.ContentWrite),
		Doc:       "Presigns multipart parts, each bound to its SHA-256.",
		Request:   media.PartsBody{},
		Responses: []httpapi.Reply{httpapi.OK(media.PartsReply{})}, Errors: []string{media.CodeForbidden, media.CodeNotFound}},
		Serve: httpapi.H(uploadHandler.parts)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/parts/list", Resource: "uploads", Auth: httpapi.Editor, Perm: string(access.ContentWrite),
		Doc:       "The parts of a multipart upload that landed, to resume it.",
		Request:   media.TicketBody{},
		Responses: []httpapi.Reply{httpapi.OK(media.PartsReply{})}, Errors: []string{media.CodeForbidden, media.CodeNotFound}},
		Serve: httpapi.H(uploadHandler.listParts)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/complete", Resource: "uploads", Auth: httpapi.Editor, Perm: string(access.ContentWrite),
		Doc:       "Assembles a multipart upload; blob is its staged name.",
		Request:   media.TicketBody{},
		Responses: []httpapi.Reply{httpapi.OK(media.CompleteReply{})}, Errors: []string{media.CodeChecksum, media.CodeForbidden, media.CodeIncomplete, media.CodeNotFound}},
		Serve: httpapi.H(uploadHandler.complete)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/abort", Resource: "uploads", Auth: httpapi.Editor, Perm: string(access.ContentWrite),
		Doc:       "Abandons a multipart upload.",
		Request:   media.TicketBody{},
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{media.CodeForbidden, media.CodeNotFound}},
		Serve: httpapi.H(uploadHandler.abort)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/commit", Resource: "uploads", Auth: httpapi.Editor, Perm: string(access.ContentWrite),
		Doc:       "Applies ops to an item in one conditional write, then queues placement and processing; answers its uploads as an editor reads them.",
		Request:   media.CommitBody{},
		Responses: []httpapi.Reply{httpapi.OK(media.CommitReply{})},
		Errors: []string{media.CodeConflict, media.CodeForbidden, media.CodeImageTooSmall, media.CodeNotFound, media.CodeNotUploaded, media.CodeQuota, media.CodeRate,
			media.CodeTooLarge, media.CodeTooManyFiles, media.CodeType}},
		Serve: httpapi.H(uploadHandler.commit)},
}

var mediaReadQuery = []httpapi.Param{httpapi.Text("prefix", "only files under this path prefix, e.g. low-res/"),
	httpapi.Repeated("path", "at most 64 exact paths to sign, without offset or limit; all authorized metadata remains listed"),
	httpapi.Int("offset", "files to skip before the URLs start"), httpapi.Int("limit", "files that get URLs"),
	httpapi.Flag("download", "URLs carry their download names"), httpapi.Flag("editor", "editors: uploads with edits, failures, progress and editor views"),
	httpapi.Flag("status", "editors: whole-item processing counts only, without URLs, media grants or editor-view work; Accept: text/event-stream observes retained progress"),
	httpapi.Text("after", "SSE reconnect cursor; Last-Event-ID is also accepted. Retention expiry sends a resync snapshot")}

var readRoutes = []httpapi.Route[readHandler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/presets", Resource: "media", Auth: httpapi.Open,
		Doc:       "Every kind's public preset rules: crop bounds, widths and default images. Current image URLs come from a read's public.",
		Responses: []httpapi.Reply{httpapi.OK([]media.PresetRule{})}},
		Serve: httpapi.H(readHandler.presets)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/{kind}/{id}", Resource: "media", Auth: httpapi.Public,
		Doc:       "Released files under a prefix in manifest order. Offset/limit or repeated path selects which get URLs. Version access covers its private folder; optional restricted integrations use exact-file tokens. Explicit editor reads can inspect unserved source/prepared files through short-lived storage URLs. Unreadable files are omitted.",
		Query:     mediaReadQuery,
		Responses: []httpapi.Reply{httpapi.OK(media.ReadResult{})}},
		Serve: httpapi.H(readHandler.read)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/{kind}/{id}/hls/{path...}", Resource: "media", Auth: httpapi.Public,
		Doc: "HLS: {dir}master.m3u8 (a ladder of the read's hls), {path}.m3u8 (a track) or {dir}sprite.vtt (the seek sprite).",
		Query: []httpapi.Param{httpapi.List("audio", "audio languages the master playlist keeps; empty keeps none"),
			httpapi.List("subs", "subtitle languages the master playlist keeps; empty keeps none"), httpapi.Flag("editor", "explicit administrative inspection; current authority is rechecked")},
		Responses: []httpapi.Reply{httpapi.OK(httpapi.Stream{ContentType: media.HLSContentType}), httpapi.OK(httpapi.Stream{ContentType: media.VTTContentType})}},
		Serve: httpapi.H(readHandler.playlist)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/{kind}/{id}/versions/{version}", Resource: "media", Auth: httpapi.Public,
		Doc:       "A content version's item, read as GET /{kind}/{id} reads any other kind's item.",
		Query:     mediaReadQuery,
		Responses: []httpapi.Reply{httpapi.OK(media.ReadResult{})}},
		Serve: httpapi.H(readHandler.read)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/{kind}/{id}/versions/{version}/hls/{path...}", Resource: "media", Auth: httpapi.Public,
		Doc: "A version's HLS playlists, as GET /{kind}/{id}/hls/{path...} serves an item's.",
		Query: []httpapi.Param{httpapi.List("audio", "audio languages the master playlist keeps; empty keeps none"),
			httpapi.List("subs", "subtitle languages the master playlist keeps; empty keeps none"), httpapi.Flag("editor", "explicit administrative inspection; current authority is rechecked")},
		Responses: []httpapi.Reply{httpapi.OK(httpapi.Stream{ContentType: media.HLSContentType}), httpapi.OK(httpapi.Stream{ContentType: media.VTTContentType})}},
		Serve: httpapi.H(readHandler.playlist)},
}
