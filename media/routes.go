package media

import (
	"github.com/open-rails/contentkit/internal/httpapi"
)

func init() {
	httpapi.Register(httpapi.Upload, uploadRoutes)
	httpapi.Register(httpapi.Media, readRoutes)
}

var refQuery = []httpapi.Param{httpapi.Text("kind", "the item's kind"), httpapi.Text("id", "the item's id")}

var uploadRoutes = []httpapi.Route[uploadHandler]{
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/presign", Resource: "uploads", Auth: httpapi.User,
		Doc:       "Plans one upload after checking its path's rules and CanUpload: exists (commit directly), one PUT, or a multipart ticket.",
		Request:   PresignBody{},
		Responses: []httpapi.Reply{httpapi.OK(PresignReply{})},
		Errors:    []string{CodeConflict, CodeForbidden, CodeNotFound, CodeQuota, CodeRate, CodeTooLarge, CodeTooManyFiles, CodeType}},
		Serve: httpapi.H(uploadHandler.presign)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/parts", Resource: "uploads", Auth: httpapi.User,
		Doc:       "Presigns multipart parts, each bound to its SHA-256.",
		Request:   PartsBody{},
		Responses: []httpapi.Reply{httpapi.OK(PartsReply{})}, Errors: []string{CodeForbidden, CodeNotFound}},
		Serve: httpapi.H(uploadHandler.parts)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/parts/list", Resource: "uploads", Auth: httpapi.User,
		Doc:       "The parts of a multipart upload that landed, to resume it.",
		Request:   TicketBody{},
		Responses: []httpapi.Reply{httpapi.OK(PartsReply{})}, Errors: []string{CodeForbidden, CodeNotFound}},
		Serve: httpapi.H(uploadHandler.listParts)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/complete", Resource: "uploads", Auth: httpapi.User,
		Doc:       "Assembles a multipart upload; blob is its staged name.",
		Request:   TicketBody{},
		Responses: []httpapi.Reply{httpapi.OK(CompleteReply{})}, Errors: []string{CodeChecksum, CodeForbidden, CodeIncomplete, CodeNotFound}},
		Serve: httpapi.H(uploadHandler.complete)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/abort", Resource: "uploads", Auth: httpapi.User,
		Doc:       "Abandons a multipart upload.",
		Request:   TicketBody{},
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{CodeForbidden, CodeNotFound}},
		Serve: httpapi.H(uploadHandler.abort)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/commit", Resource: "uploads", Auth: httpapi.User,
		Doc:       "Applies ops to an item in one conditional write, then queues placement and processing; answers its uploads as an editor reads them.",
		Request:   CommitBody{},
		Responses: []httpapi.Reply{httpapi.OK(CommitReply{})},
		Errors: []string{CodeConflict, CodeForbidden, CodeImageTooSmall, CodeNotFound, CodeNotUploaded, CodeQuota, CodeRate,
			CodeTooLarge, CodeTooManyFiles, CodeType}},
		Serve: httpapi.H(uploadHandler.commit)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/frame", Resource: "uploads", Auth: httpapi.User,
		Doc: "A JPEG still of a video upload, for the frame picker (UploadOptions.Frames).",
		Query: append(append([]httpapi.Param(nil), refQuery...), httpapi.Text("path", "the video upload's path"),
			httpapi.Number("t", "seconds into the video"), httpapi.Int("w", "the still's width in pixels")),
		Responses: []httpapi.Reply{httpapi.OK(httpapi.Stream{ContentType: "image/jpeg"})}, Errors: []string{CodeForbidden, CodeNotFound}},
		Serve: httpapi.H(uploadHandler.frame)},
}

var readRoutes = []httpapi.Route[readHandler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/{kind}/{id}", Resource: "media", Auth: httpapi.Public,
		Doc: "An item's files under a prefix in manifest order: with access each has a URL (and the item cookie is set), without each is locked.",
		Query: []httpapi.Param{httpapi.Text("prefix", "only files under this path prefix, e.g. low-res/"),
			httpapi.Int("offset", "files to skip before the URLs start"), httpapi.Int("limit", "files that get URLs"),
			httpapi.Flag("download", "URLs carry their download names"), httpapi.Flag("editor", "editors: uploads with edits, failures, progress and editor views")},
		Responses: []httpapi.Reply{httpapi.OK(ReadResult{})}},
		Serve: httpapi.H(readHandler.read)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/{kind}/{id}/hls/{path...}", Resource: "media", Auth: httpapi.Public,
		Doc: "HLS: {dir}master.m3u8 (a ladder of the read's hls), {path}.m3u8 (a track) or {dir}sprite.vtt (the seek sprite).",
		Query: []httpapi.Param{httpapi.List("audio", "audio languages the master playlist keeps; empty keeps none"),
			httpapi.List("subs", "subtitle languages the master playlist keeps; empty keeps none")},
		Responses: []httpapi.Reply{httpapi.OK(httpapi.Stream{ContentType: HLSContentType}), httpapi.OK(httpapi.Stream{ContentType: VTTContentType})}},
		Serve: httpapi.H(readHandler.playlist)},
}
