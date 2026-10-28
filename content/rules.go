package content

import (
	"time"
)

// WordAction is what a banned word does to text containing it.
type WordAction string

const (
	WordMask WordAction = "mask" // replaced by asterisks
)

const (
	WordReject WordAction = "reject" // the write is refused
)

// BannedWord is one banned word.
type BannedWord struct {
	Word      string     `json:"word"`
	Action    WordAction `json:"action"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
}

// BannedWordInput adds a banned word or changes its action (mask by default).
type BannedWordInput struct {
	Word   string     `json:"word"`
	Action WordAction `json:"action,omitempty"`
}

// IPBan refuses comments and answers from an address or network.
type IPBan struct {
	Network  string     `json:"network"`
	Reason   string     `json:"reason,omitempty"`
	Until    *time.Time `json:"until,omitempty"`
	BannedBy string     `json:"banned_by"`
	BannedAt time.Time  `json:"banned_at"`
	Expired  bool       `json:"expired"`
}

// IPBanInput bans an address ("203.0.113.7") or a network ("203.0.113.0/24"),
// or replaces its ban.
type IPBanInput struct {
	Network string     `json:"network"`
	Reason  string     `json:"reason,omitempty"`
	Until   *time.Time `json:"until,omitempty"`
}

// ScopeIP is the BanNotice scope of an IP ban.
const ScopeIP = "ip"
