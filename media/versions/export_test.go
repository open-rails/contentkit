package versions

// RecoverPublications runs the recovery pass the periodic job runs.
var RecoverPublications = (*Service).recoverPublications

// Publish runs one pass of a version's publish job.
var Publish = (*Service).publish

// SetRecoveryBatch bounds a recovery pass's requeues per kind until restored.
func SetRecoveryBatch(n int) (restore func()) {
	old := recoveryBatch
	recoveryBatch = n
	return func() { recoveryBatch = old }
}

// RecordPlayable runs the pass the daily playable job runs.
var RecordPlayable = (*Service).recordPlayable
