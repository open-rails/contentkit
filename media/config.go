// Package media stores application media in a private bucket. A registry
// declares media kinds, uploads, renditions and access. Each item has an
// ordered manifest that is authoritative for retained source and derived files.
// A database journal records conditional manifest writes, pending allocations
// and exact retirement effects, including interrupted uploads and caches.
//
// Scoped roots are tenant/<tenant>/app/<app>/<kind>/<item>/; versioned
// content uses the version as the physical item ID. Account media uses
// tenant/<tenant>/account/. Legacy roots remain documented in media/layout.
package media

import (
	"context"
	"io/fs"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media/layout"
)

// Config is the app's media registry (NewRegistry).
type Config struct {
	// Namespace is the app's own namespace, e.g. "doujins": its kinds' items
	// live under {Namespace}/{kind}/{id}/.
	Namespace string `json:"namespace,omitempty"`
	// Scope derives object roots and internal partitions; Namespace is legacy-only.
	Scope layout.Scope `json:"scope,omitzero"`
	// BaseURL is the site's media origin, e.g. "https://media.doujins.ai":
	// every URL is {BaseURL}/v1/{namespace}/{kind}/{id}/{public|private}/{name}.
	BaseURL string `json:"base_url,omitempty"`
	// Kinds are the app's kinds plus the shared kinds it imports.
	Kinds []Kind `json:"kinds"`
	// Avatars names people's avatars: content shows them beside comment
	// authors and in moderation lists. Zero: people have none.
	Avatars Avatars `json:"avatars,omitzero"`
	// Editor is how editor views render (the whole oriented source the
	// cropper draws on); zero is DefaultEditor.
	Editor Image `json:"editor,omitzero"`
	Hooks  Hooks `json:"-"`
}

// Avatars is Config.Avatars: Kind's items are people's accounts, keyed by the
// host auth's subject (auth.Identity.Subject), such as the shared accounts
// kind "user", and Preset is the public image each is shown with.
type Avatars struct {
	Kind   string `json:"kind"`
	Preset string `json:"preset"`
}

// DefaultEditor renders editor views.
var DefaultEditor = Image{Width: 2048, Height: 2048, Quality: 82}

// Kind is one kind of item: what may be uploaded, what is derived from the
// uploads in private/, and which public images it has.
type Kind struct {
	Name string `json:"name"`
	// Namespace is "" for Config.Namespace, or a shared one such as
	// "accounts" whose items every app importing the kind serves.
	Namespace string `json:"namespace,omitempty"`
	// Account places this kind in the tenant account root of a scoped registry.
	Account bool `json:"account,omitempty"`
	// Versions marks a content kind (ForContent). Each version owns its
	// folder; its parent content exists in the catalog, not in storage.
	Versions bool `json:"versions,omitempty"`
	// Publish describes the existing files a version's reader can use.
	// Published snapshots contain metadata, not separate media folders.
	Publish *Publish `json:"publish,omitempty"`
	Uploads []Upload `json:"uploads"`
	// KeepOriginals keeps an upload's blob once its private outputs exist;
	// otherwise it is dropped (File.Gone). Public presets' sources are
	// always kept, so unhiding can render them again, as are body images,
	// which new versions copy.
	KeepOriginals bool `json:"keep_originals,omitempty"`
	// ServeOriginals lists and serves uploads to viewers with access, like
	// any private file. Otherwise viewer URLs are scoped to served files,
	// not the whole private folder, and reads omit upload paths and blobs.
	ServeOriginals bool `json:"serve_originals,omitempty"`
	// Downloads are the upload paths and private presets (Image, MP4 or Zip)
	// whose files live in download/: only a download token opens them and
	// reads never list them. ForContent adds a content kind's originals, its
	// zip and what the zip packs.
	Downloads []string `json:"downloads,omitempty"`
	// Download is what a version's download serves, one of Downloads: a
	// literal upload (the original) or an MP4 or Zip preset. ForContent sets
	// it from the content kind's download; "" serves none.
	Download string    `json:"download,omitempty"`
	Private  []Private `json:"private,omitempty"`
	Public   []Public  `json:"public,omitempty"`
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
	Path string `json:"path"`
	// Body marks a content kind's body images (Named, with a private image
	// preset and no public one): each version keeps its copy in private/,
	// which only the version's openers read.
	Body     bool     `json:"body,omitempty"`
	Types    []string `json:"types"`            // accepted content types
	MaxBytes int64    `json:"max_bytes"`        // per file
	Max      int      `json:"max,omitempty"`    // uploads matching Path; 0 is unlimited
	Frames   string   `json:"frames,omitempty"` // may be grabbed from a frame of this video upload (the frame op)
	Named    bool     `json:"named,omitempty"`  // the server names it ({name} is "i-{uuid}"): inline images
	// Video bounds a video or audio upload as probed from its real stream;
	// nil or zero fields take the defaults (DefaultVideoLimits).
	Video *VideoLimits `json:"video,omitempty"`
}

// Publish declares which existing version files a reader may consume. Its
// immutable metadata snapshot contains no chapter folders or duplicated files;
// chapter boundaries and release dates are evaluated when the reader asks.
type Publish struct {
	// Pages is the {name} upload of the version's ordered pages:
	// "originals/{name}".
	Pages string `json:"pages"`
	// Renditions are the private image presets from Pages a reader reads,
	// the first at reading size: "low", "thumb".
	Renditions []string `json:"renditions"`
	// Previews names the legacy preview policy preset (First caps its count).
	// Reader plans sign existing renditions; no preview uploads are created.
	Previews string `json:"previews,omitempty"`
	// Playback is the private Audio preset whose existing M4As the reader uses.
	Playback string `json:"playback,omitempty"`
	// Import is the {name} upload of page archives (application/zip),
	// expanded into Pages in name order.
	Import string `json:"import,omitempty"`
	// Download is the HostOnly Zip preset of the version's pages, which a
	// download serves when its content kind serves pages.
	Download string `json:"download,omitempty"`
}

// PublishedSuffix is retained only to identify retired legacy storage during
// migration. New registries never register a kind with this suffix.
const PublishedSuffix = ".published"

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
	// directory ending in "/" ("hls/", "listen/{name}/"). Source-format
	// images end in {ext}, the upload's extension: "download/{name}{ext}".
	To string `json:"to"`
	// Download is the human name template a download read signs into the
	// URL, filled from the manifest's meta and {name}: "{title}.zip".
	Download string `json:"download,omitempty"`
	// HostOnly leaves this preset out of generic reads and playlists. It
	// decides what is listed, not what a token opens: an item's token opens
	// every private file of the item; only Kind.Downloads keeps one from it.
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
// as metadata. A missing one is served Default by the media gateway.
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
	// Stable also keeps the current image at the logical names (To without
	// a generation), overwritten in place: a URL built from the item's id,
	// which the gateway answers with Default while none is current. One
	// image per item: no {name}, no First.
	Stable bool `json:"stable,omitempty"`
}

// Image is a rendering, WebP by default. Source-format private presets retain
// the upload's codec and copy unchanged bytes when no resize/edit is needed.
// Resizing and editing in source format support JPEG, PNG, WebP and GIF.
// Zero Width and Height keep the full size; nothing is upscaled.
type Image struct {
	Format  ImageFormat `json:"format,omitempty"`
	Width   int         `json:"width,omitempty"`
	Height  int         `json:"height,omitempty"`
	Fit     FitMode     `json:"fit,omitempty"`
	Quality int         `json:"quality,omitempty"` // default 80
	Blur    float64     `json:"blur,omitempty"`
	// Aspect and MinWidth bound an upload's edit when this is its public
	// preset's image: the crop is fitted to Aspect, and an edit narrower
	// than MinWidth fails.
	Aspect    Aspect    `json:"aspect,omitzero"`
	MinWidth  int       `json:"min_width,omitempty"`
	Animation Animation `json:"animation,omitempty"`
}

// ImageFormat selects WebP encoding or the upload's original codec.
type ImageFormat string

const (
	// ImageFormatWebP is the default for all images and the only format for
	// public presets and editor previews.
	ImageFormatWebP ImageFormat = ""
	// ImageFormatSource preserves the upload's codec in a private preset.
	ImageFormatSource ImageFormat = "source"
)

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
	Resolver resolve.Resolver
	// CanUpload says who may write which upload path.
	CanUpload UploadAuthorizer
	// ItemReady reports an item whose processing settled (ready, or failed
	// with nothing processing) in a host transaction, after the worker's
	// job, through the host's media queue. It must be idempotent; an error
	// retries.
	ItemReady func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, r Readiness) error
	// ItemProgressed reports an item still processing once a job left it
	// playable (a video's first HLS rendition is out), in a host transaction
	// through the host's media queue like ItemReady, which still reports the
	// far end. Optional; idempotent; an error retries.
	ItemProgressed func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef) error
	// BeforeRetire is the catalog integration barrier. It runs after the
	// manifest commits and before its removed objects are deleted, including
	// recovery. It must invalidate references to those objects idempotently.
	// The manifest lease is held: this callback must not mutate media.
	BeforeRetire func(ctx context.Context, ref contentref.ContentRef, current *Manifest) error
	// ItemCommitted reports an item a commit changed, before its processing
	// runs; ItemReady reports the far end. Idempotent; an error fails the
	// commit.
	ItemCommitted func(ctx context.Context, ref contentref.ContentRef) error
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
