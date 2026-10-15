package layout

import "strings"

// Scope is the trusted owner of a media root. An empty AppID identifies tenant
// account media; a nonempty AppID identifies one app's catalog media.
// The zero value retains the legacy single-segment layout.
type Scope struct {
	TenantID string `json:"tenant_id"`
	AppID    string `json:"app_id,omitempty"`
}

func (s Scope) IsZero() bool { return s == (Scope{}) }
func (s Scope) Valid() bool  { return uuid(s.TenantID) && (s.AppID == "" || uuid(s.AppID)) }

// Partition is an internal database/job key. It is never an object path or a
// caller-selected namespace. Including both IDs prevents reused app IDs in
// different tenants from sharing quotas, caches or work.
func (s Scope) Partition() string {
	if s.AppID == "" {
		return "tenant_" + s.TenantID
	}
	return "tenant_" + s.TenantID + "_app_" + s.AppID
}
func (s Scope) Root() string {
	if s.AppID == "" {
		return "tenant/" + s.TenantID + "/account/"
	}
	return "tenant/" + s.TenantID + "/app/" + s.AppID + "/"
}
func (s Scope) Prefix(kind, id, version string) string {
	return s.Root() + strings.TrimPrefix(Prefix("", kind, id, version), "/")
}
func ScopeFromPartition(p string) (Scope, bool) {
	t, ok := strings.CutPrefix(p, "tenant_")
	if !ok {
		return Scope{}, false
	}
	t, a, _ := strings.Cut(t, "_app_")
	s := Scope{TenantID: t, AppID: a}
	return s, s.Valid() && s.Partition() == p
}
func uuid(s string) bool {
	if len(s) != 36 || s == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
