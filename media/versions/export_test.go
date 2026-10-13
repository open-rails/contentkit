package versions

// RecoverPublications runs the recovery pass the periodic job runs.
var RecoverPublications = (*Service).recoverPublications

// Publish runs one version's publication job.
var Publish = (*Service).publish
