package versions

import (
	"encoding/json"

	"github.com/open-rails/contentkit/access"
)

// EncodeReleaseBaseline represents a previously completed edition without
// storage. It records the policy already served, never today's evaluation of
// desired access. Migration persists it without invoking completion or events.
func EncodeReleaseBaseline(target Target, kind, state, parentState string, desired, released access.Rule) ([]byte, error) {
	plan := &releasePlan{Target: target, Kind: kind, EffectiveAccess: released, Released: true}
	if err := finishReleasePlanFingerprint(plan, nil, state, parentState, desired); err != nil {
		return nil, err
	}
	return json.Marshal(plan)
}
