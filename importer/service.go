package importer

// RecordCount returns the count checked against MaxRecords, including nested
// content versions and poll options. Importers can use it to size their batches.
func (b Batch) RecordCount() int {
	n := len(b.Entities) + len(b.Channels) + len(b.Memberships) + len(b.Aliases) + len(b.Comments) + len(b.Reactions) + len(b.Favorites) +
		len(b.Votes) + len(b.LegacyReading) + len(b.Signals) + len(b.Deletes)
	for _, c := range b.Contents {
		n += 1 + len(c.Versions)
	}
	for _, p := range b.Polls {
		n += 1 + len(p.Options)
	}
	return n
}
