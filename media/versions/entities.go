package versions

// CoverOf names a content's or an entity's cover: Kind and ID are its owner;
// Image an image uploaded to the owner's item ("i-{uuid}"), else ContentID a
// content whose cover it shows; with neither, a content owner's own. A
// content's cover is its Version's, else its default edition's in Language
// (internal/edition). With Preset media.AvatarPreset it names an entity's
// avatar: Image, else the item's first, else the kind's default.
type CoverOf struct {
	Kind      string
	ID        string
	Image     string
	ContentID string
	Version   string
	Language  string
	Preset    string
}
