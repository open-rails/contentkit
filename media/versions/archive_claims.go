package versions

import (
	"slices"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
)

// archiveTokenClaims copies only token-backed tiers that this engine accepts.
// No bearer token, roles or unrelated identity claims enter a download URL.
func archiveTokenClaims(actor access.Actor, allowed []string) []string {
	if actor.Anonymous {
		return nil
	}
	source, ok := actor.Verified.(access.Entitled)
	if !ok {
		return nil
	}
	held := source.Entitlements()
	var out []string
	for _, claim := range allowed {
		if slices.Contains(held, claim) && !slices.Contains(out, claim) {
			out = append(out, claim)
		}
	}
	return out
}

// archiveClaimProof is created only after the exact archive ticket is verified.
// It lets token-only gates evaluate that transfer without replaying a session
// credential or implementing any general permission-checking capability.
type archiveClaimProof struct {
	identity auth.Identity
	claims   []string
}

func (p archiveClaimProof) Identity() auth.Identity { return p.identity }
func (p archiveClaimProof) Entitlements() []string  { return slices.Clone(p.claims) }
