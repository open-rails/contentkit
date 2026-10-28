package content

// ImageInput names an inline image of the item's media folder ("i-{uuid}");
// "" clears where the route allows it.
type ImageInput struct {
	Image *string `json:"image"`
}
