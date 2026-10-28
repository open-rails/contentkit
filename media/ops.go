package media

import (
	"strings"

	"github.com/google/uuid"
)

const (
	OpPut = "put" // add an upload, or replace the one with the same path stem
)

const (
	OpEdit = "edit" // crop and rotate an image upload; nil clears
)

const (
	OpMove = "move" // reorder an upload among its Upload's; its outputs follow
)

const (
	OpRename = "rename" // rename an upload and its outputs
)

const (
	OpRemove = "remove" // remove an upload and its outputs
)

const (
	OpAttach = "attach" // make an unattached upload part of the item
)

const (
	OpCopy = "copy" // copy an upload within the kind (CopyFrom), optionally with a new edit
)

const (
	OpFrame = "frame" // fill an upload from a frame of its Upload.Frames video, or of another item's (From)
)

const (
	OpMeta = "meta" // set the template values (download names)
)

const (
	OpRegenerate = "regenerate" // redo stale outputs and retry failed uploads (of Preset), or every output with Force
)

// Op is one commit op.
type Op struct {
	Op         string         `json:"op"`
	SourceName string         `json:"source_name,omitempty"`
	Path       string         `json:"path,omitempty"`
	Blob       string         `json:"blob,omitempty"`      // put: the staged upload (u-{uuid}) or a blob in the folder
	CreateID   string         `json:"create_id,omitempty"` // put: create-only UUID; repeat the same ID to retry
	Replace    bool           `json:"replace,omitempty"`   // put: explicitly replace the target, including another source alias
	Index      *int           `json:"index,omitempty"`     // put, move, attach: among the attached uploads of its Upload
	Meta       map[string]any `json:"meta,omitempty"`      // put, copy: upload meta; attach: merged; meta: item meta; remove: auto_cover guards ownership
	Edit       *Edit          `json:"edit,omitempty"`      // put, edit, copy, frame
	Unattached bool           `json:"unattached,omitempty"`
	To         string         `json:"to,omitempty"`   // rename, copy (default the source's path)
	From       *CopyFrom      `json:"from,omitempty"` // copy; frame: the video
	T          *float64       `json:"t,omitempty"`    // frame: the one a player shows t seconds in
	Auto       bool           `json:"auto,omitempty"` // frame: the worker chooses
	Preset     string         `json:"preset,omitempty"`
	Force      bool           `json:"force,omitempty"`
	// Takedown (remove) also removes the frames grabbed from the upload and
	// the zips that bundled it, then deletes at once, not a grace period
	// later, the blobs the commit dropped, the removed uploads' editor views
	// and the public files no upload renders. With an exempt grant it deletes
	// every private blob the item no longer references (earlier versions and
	// every editor view too), and repeating it, the path already gone, runs
	// that sweep again: how staff complete a takedown that failed part way.
	Takedown bool `json:"takedown,omitempty"`
}

// CopyFrom names an upload of an item of the same kind, including this item:
// of a versioned kind, one version's. Into
// a published kind's item, it names a file of a version of the kind it
// publishes: an upload, or a finished output, which comes alone, as an upload.
// A frame op's names a video upload of any item in the namespace (Kind,
// default this item's) the editor may edit; the worker grabs it once.
type CopyFrom struct {
	Kind    string `json:"kind,omitempty"` // frame only
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	Path    string `json:"path"`
}

// NamedPrefix starts the names the server gives Named uploads.
const NamedPrefix = "i-"

// ValidNamed reports a server-given name ("i-{uuid}").
func ValidNamed(name string) bool {
	id, ok := strings.CutPrefix(name, NamedPrefix)
	if !ok {
		return false
	}
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}
