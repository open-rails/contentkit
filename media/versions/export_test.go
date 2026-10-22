package versions

// Publish runs one pass of a version's publish job.
var Publish = (*Service).publish

// LegacyArchiveScope builds a pre-upgrade whole-version ticket for boundary tests.
var LegacyArchiveScope = legacyArchiveScope

// RecordPlayable runs the pass the daily playable job runs.
var RecordPlayable = (*Service).recordPlayable
