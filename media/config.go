// Package media stores app media as a self-describing file system in one
// private bucket. The app declares everything in one registry at startup
// (Config): its kinds, their uploads, the private files derived from them
// and the public images at fixed names. ContentKit hard-codes no kinds, names
// or layouts; it owns the mechanics: uploads, the worker's generic producers,
// conditional manifest writes, the storage areas, the access rule, and
// purge, sweep and erasure. No database table records what media exists.
//
// An item is a folder, {namespace}/{kind}/{id}/ (see media/layout), with a
// manifest.json: an ordered virtual file system over its private blobs.
package media

import (
	"context"
	"io/fs"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
)

// Config is the app's media registry (NewRegistry).
type Config struct {
	// Namespace is the app's own namespace, e.g. "doujins": its kinds' items
	// live under {Namespace}/{kind}/{id}/.
	Namespace string `json:"namespace"`
	// BaseURL is the site's media origin, e.g. "https://media.doujins.ai":
	// every URL is {BaseURL}/v1/{namespace}/{kind}/{id}/{public|private}/{name}.
	BaseURL string `json:"base_url,omitempty"`
	// Kinds are the app's kinds plus the shared kinds it imports.
	Kinds []Kind `json:"kinds"`
	// Editor is how editor views render (the whole oriented source the
	// cropper draws on); zero is DefaultEditor.
	Editor Image `json:"editor,omitzero"`
	Hooks  Hooks `json:"-"`
}

// DefaultEditor renders editor views.
var DefaultEditor = Image{Width: 2048, Height: 2048, Quality: 82}

// Kind is one kind of item: what may be uploaded, what is derived from the
// uploads in private/, and which public images it has.
type Kind struct {
	Name string `json:"name"`
	// Namespace is "" for Config.Namespace, or a shared one such as
	// "accounts" whose items every app importing the kind serves.
	Namespace string   `json:"namespace,omitempty"`
	Uploads   []Upload `json:"uploads"`
	// KeepOriginals keeps an upload's blob once its private outputs exist;
	// otherwise it is dropped (File.Gone). Public presets' sources are
	// always kept, so unhiding can render them again.
	KeepOriginals bool `json:"keep_originals,omitempty"`
	// ServeOriginals lists and serves uploads to viewers with access, like
	// any private file. Otherwise viewer URLs are scoped to served files,
	// not the whole private folder, and reads omit upload paths and blobs.
	ServeOriginals bool      `json:"serve_originals,omitempty"`
	Private        []Private `json:"private,omitempty"`
	Public         []Public  `json:"public,omitempty"`
	// Defaults holds the images the kind's public presets name in
	// Public.Default, so a shared kind ships its own (go:embed). A registry
	// read from JSON (the stock worker) has none; PublishDefaults needs them.
	Defaults fs.FS `json:"-"`

	kindState
}

// Upload is an app path that may be uploaded: a literal ("cover") or a
// pattern ending in {name} ("originals/{name}"). A file's path is the
// path plus its extension ("cover.png", "originals/001.png"); a put to the
// same stem replaces it.
type Upload struct {
	Path     string   `json:"path"`
	Types    []string `json:"types"`            // accepted content types
	MaxBytes int64    `json:"max_bytes"`        // per file
	Max      int      `json:"max,omitempty"`    // uploads matching Path; 0 is unlimited
	Frames   string   `json:"frames,omitempty"` // may be grabbed from a frame of this video upload (the frame op)
	Named    bool     `json:"named,omitempty"`  // the server names it ({name} is "i-{uuid}"): inline images
	// Video bounds a video or audio upload as probed from its real stream;
	// nil or zero fields take the defaults (DefaultVideoLimits).
	Video *VideoLimits `json:"video,omitempty"`
}

// VideoLimits bound what one video or audio upload may cost the worker; an
// upload past them fails (video_too_long, video_too_large,
// video_over_budget) before any encode runs.
type VideoLimits struct {
	MaxSeconds float64 `json:"max_seconds,omitempty"` // real running time (audio too)
	MaxFPS     float64 `json:"max_fps,omitempty"`     // output frame rate (at most 60); a faster source is encoded at it
	MaxPixels  int     `json:"max_pixels,omitempty"`  // displayed frame area
	// MaxWork bounds the planned encode: the sum over the HLS and MP4
	// presets' rungs and codecs of output pixels × output frames.
	MaxWork float64 `json:"max_work,omitempty"`
}

// Private derives files in private/, recorded in the manifest with their
// provenance (File.From, Preset, FP). Exactly one producer is set.
type Private struct {
	Name string `json:"name"`
	// From is an upload path or pattern; "" for Zip.
	From string `json:"from,omitempty"`
	// To is the output path template: a file ("low-res/{name}.webp",
	// "video/source-1080p.mp4") or, for producers with several outputs, a
	// directory ending in "/" ("hls/", "listen/{name}/").
	To string `json:"to"`
	// Download is the human name template a download read signs into the
	// URL, filled from the manifest's meta and {name}: "{title}.zip".
	Download string `json:"download,omitempty"`
	// HostOnly leaves this preset out of generic reads and playlists; a host
	// route applies its own policy and calls Grant.HostURL. It decides what
	// is listed, not what a token opens: an item's token opens every private
	// file of the item.
	HostOnly  bool       `json:"host_only,omitempty"`
	Image     *Image     `json:"image,omitempty"`
	HLS       *HLS       `json:"hls,omitempty"`
	MP4       *MP4       `json:"mp4,omitempty"`
	Zip       string     `json:"zip,omitempty"` // the files under this prefix, in manifest order: "high/"
	Audio     *Audio     `json:"audio,omitempty"`
	Subtitles *Subtitles `json:"subtitles,omitempty"`
	// Choose is an optional per-file image spec (Go only, not in the JSON
	// registry); nil keeps Image.
	Choose func(f File) *Image `json:"-"`
}

// Public is a fixed public image: {To} with {w} at each of Widths (or one
// file without Widths), rendered from an upload through its edit, at
// public/{name}. With Widths, each width is Image at that width: in the
// shape of its Width×Height box (both or neither), fitted per Fit, else of
// the edit. It is never in the manifest; the object carries its from and fp
// as metadata. A missing one is served Default by the access agent.
//
// First makes it a preview: the first First attached uploads of From (a
// {name} pattern) in manifest order, {n} in To their position from 1
// ("preview-{n}.webp"). Anyone who can see the item sees them; a position is
// rendered again when another upload takes it, and names past the last
// upload are deleted. Everything else of the item stays private.
type Public struct {
	Name   string `json:"name"`
	From   string `json:"from"`
	To     string `json:"to"` // "cover-{w}.webp", "{name}.webp", "preview-{n}.webp"
	First  int    `json:"first,omitempty"`
	Widths []int  `json:"widths,omitempty"`
	Image  Image  `json:"image"`
	// Default is a path in Kind.Defaults, rendered to the kind's _default
	// item at every width (PublishDefaults).
	Default string `json:"default,omitempty"`
}

// Image is a WebP rendering. Zero Width and Height keep the full size;
// nothing is upscaled.
type Image struct {
	Width   int     `json:"width,omitempty"`
	Height  int     `json:"height,omitempty"`
	Fit     FitMode `json:"fit,omitempty"`
	Quality int     `json:"quality,omitempty"` // default 80
	Blur    float64 `json:"blur,omitempty"`
	// Aspect and MinWidth bound an upload's edit when this is its public
	// preset's image: the crop is fitted to Aspect, and an edit narrower
	// than MinWidth fails.
	Aspect    Aspect    `json:"aspect,omitzero"`
	MinWidth  int       `json:"min_width,omitempty"`
	Animation Animation `json:"animation,omitempty"`
}

// FitMode is how an image fits its Width×Height box.
type FitMode string

const (
	FitInside FitMode = "" // within the box, keeping the aspect
	FitCover  FitMode = "cover"
)

// Animation is a policy for animated images (GIF, WebP; AVIF/HEIF sequences
// are refused as animation_unsupported, never flattened).
type Animation string

const (
	// AnimationAllow keeps every frame, delay and the loop count.
	AnimationAllow Animation = ""
	// AnimationReject refuses an animated upload with animation_not_allowed.
	AnimationReject Animation = "reject"
)

// HLS is a byte-range fMP4 ladder, one blob per rendition, with the source's
// audio and subtitle tracks and a seek sprite. A rung above the source is
// not produced.
type HLS struct {
	// Ladder is the rendition short sides, largest first; empty is DefaultLadder.
	Ladder []int `json:"ladder,omitempty"`
	// MinAspect and MaxAspect bound the source's display width/height; zero
	// is DefaultMinAspect and DefaultMaxAspect. A source outside fails.
	MinAspect float64 `json:"min_aspect,omitempty"`
	MaxAspect float64 `json:"max_aspect,omitempty"`
	Profile   string  `json:"profile,omitempty"` // VideoLive or VideoAnimation
}

// MP4 is a muxed H.264 MP4 at one rung (short side), with the default audio.
type MP4 struct {
	Rung    int    `json:"rung"`
	Profile string `json:"profile,omitempty"`
}

// Audio is AAC-LC: an HLS audio track ({To}audio.mp4) and a faststart M4A
// ({To}audio.m4a).
type Audio struct {
	// Loudness normalizes to this integrated loudness in LUFS (EBU R128),
	// e.g. -16; 0 keeps the source's level.
	Loudness float64 `json:"loudness,omitempty"`
}

// Subtitles converts SRT, SSA/ASS and WebVTT to clean UTF-8 WebVTT. A video
// lists the item's converted subtitles as tracks after its own.
type Subtitles struct{}

// Video profiles (HLS.Profile, MP4.Profile).
const (
	VideoLive      = ""
	VideoAnimation = "animation"
)

// DefaultLadder is the HLS ladder by short side.
var DefaultLadder = []int{2160, 1080, 480}

// Default aspect bounds: 1:2.4 vertical to 2.4:1 wide.
const (
	DefaultMinAspect = 1 / 2.4
	DefaultMaxAspect = 2.4
)

// Fit is an image within an n×n box.
func Fit(n int) *Image { return &Image{Width: n, Height: n} }

// Rung is an MP4 at short side n.
func Rung(n int) *MP4 { return &MP4{Rung: n} }

// Hooks are the app's callbacks.
type Hooks struct {
	// Resolver says who may see an item: the read API, Expose and a new
	// item's first commit (Uploads) use it, and both require it.
	Resolver access.ContentResolver
	// CanUpload says who may write which upload path.
	CanUpload UploadAuthorizer
	// PurgePublic hears of public URLs overwritten, deleted or first
	// written (the CDN may hold the default), for a CDN purge.
	PurgePublic func(ctx context.Context, urls []string)
	// ItemReady reports an item whose processing settled (ready, or failed
	// with nothing processing) in a host transaction, after the worker's
	// job, through the host's media queue. It must be idempotent; an error
	// retries.
	ItemReady func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, r Readiness) error
	// Failed reports an upload a producer cannot process, where it runs.
	Failed func(ctx context.Context, ref contentref.ContentRef, path string, err error)
}

// UploadAuthorizer is the app's upload permission check, run at presign and
// commit against what is written.
type UploadAuthorizer interface {
	CanUpload(ctx context.Context, actor access.Actor, t UploadTarget) (UploadGrant, error)
}

// UploadTarget is what an upload writes: an item and an upload path stem
// ("cover", "originals/001"); "" for ops that change no one upload (move,
// meta, regenerate).
type UploadTarget struct {
	Ref  contentref.ContentRef
	Path string
}

// UploadGrant is the app's verdict. Exempt (trusted roles) skips the
// UploadLimiter; Owner is the quota owner, "" for none.
type UploadGrant struct {
	Allowed bool
	Exempt  bool
	Owner   string
}
