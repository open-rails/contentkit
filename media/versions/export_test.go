package versions

// RecoverPublications runs the recovery pass the periodic job runs.
var RecoverPublications = (*Service).recoverPublications

// Publish runs one pass of a version's publish job.
var Publish = (*Service).publish
