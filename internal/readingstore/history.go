package readingstore

import (
	"encoding/binary"
	"errors"
	"math/bits"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// historyVersion is the first byte of every packed day.
const historyVersion = 2

// ActionKind is what one history action was.
type ActionKind uint8

const (
	ActionGallery ActionKind = 0
	ActionSearch  ActionKind = 1
	ActionFollow  ActionKind = 2
)

// Reaction is a gallery action's reaction that day.
type Reaction uint8

const (
	ReactionNone Reaction = iota
	ReactionLike
	ReactionDislike
	ReactionFavorite
	ReactionHidden
)

// Surface is where the person's first open of a gallery that day came from.
// The values are stable: they are stored.
type Surface uint8

const (
	// SurfaceOutside is an open from no list of ours (or no open that day).
	SurfaceOutside Surface = iota
	SurfaceHome
	// SurfaceList is a listing page or a channel's feed.
	SurfaceList
	SurfacePopular
	SurfaceSimilar
	SurfaceSearch
	// SurfaceEntity is an entity's page or rail: a tag, artist, series...
	SurfaceEntity
	// SurfaceRecommended is the reader's recommendations ("for you").
	SurfaceRecommended
	// SurfaceContinue is continue reading or the reader's history.
	SurfaceContinue
	SurfaceFavorites
	// SurfaceOther is any other surface name.
	SurfaceOther Surface = 15
)

// SurfaceOf maps an open's surface name to its Surface; entityKind reports
// whether a name is one of the site's entity kinds.
func SurfaceOf(name string, entityKind func(string) bool) Surface {
	name = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
	switch name {
	case "":
		return SurfaceOutside
	case "home":
		return SurfaceHome
	case "list", "channel":
		return SurfaceList
	case "popular":
		return SurfacePopular
	case "similar":
		return SurfaceSimilar
	case "search", "search_suggestions":
		return SurfaceSearch
	case "featured", "entity":
		return SurfaceEntity
	case "recommendations", "recommended", "for_you":
		return SurfaceRecommended
	case "continue", "continue_reading", "history":
		return SurfaceContinue
	case "favorites":
		return SurfaceFavorites
	}
	if entityKind != nil && entityKind(name) {
		return SurfaceEntity
	}
	return SurfaceOther
}

// maxQueryBytes bounds a search's stored query.
const maxQueryBytes = 255

// Action is one entry of a person's day: a gallery at its first open, a
// search or a follow, in time order.
type Action struct {
	Kind ActionKind
	// At is the action's time, kept to the minute.
	At time.Time
	// Offset is the local UTC offset in minutes, kept in 15-minute steps.
	Offset int16
	// SessionStart marks the first action after a gap of over 30 minutes
	// in the person's raw activity.
	SessionStart bool
	// Gallery and Follow: the gallery or entity.
	ID uuid.UUID
	// Gallery: tenths read (0-10), active seconds, the reaction and where
	// the day's first open came from.
	Tenths   uint8
	ActiveS  uint32
	Reaction Reaction
	Surface  Surface
	// Search: the normalized query.
	Query string
}

// ActiveBucket is an active time's stored bucket: 0 for none, n for
// [2^(n-1), 2^n) seconds.
func ActiveBucket(seconds uint32) uint8 { return uint8(bits.Len32(seconds)) }

// EncodeDay packs one day's actions, sorted by time. day is their UTC day.
func EncodeDay(day time.Time, actions []Action) []byte {
	actions = slices.Clone(actions)
	slices.SortStableFunc(actions, func(a, b Action) int { return a.At.Compare(b.At) })
	midnight := utcDay(day)
	out := []byte{historyVersion}
	for _, a := range actions {
		header := byte(a.Kind) & 0b11
		if a.SessionStart {
			header |= 1 << 2
		}
		out = append(out, header, byte(int8(clampOffset(a.Offset)/15)))
		minute := int(a.At.Sub(midnight) / time.Minute)
		out = binary.AppendUvarint(out, uint64(min(max(minute, 0), 24*60-1)))
		switch a.Kind {
		case ActionGallery:
			out = append(out, a.ID[:]...)
			out = append(out, min(a.Tenths, 10)|byte(a.Reaction&0b111)<<4, ActiveBucket(a.ActiveS), byte(min(a.Surface, SurfaceOther)))
		case ActionSearch:
			q := truncate(a.Query, maxQueryBytes)
			out = binary.AppendUvarint(out, uint64(len(q)))
			out = append(out, q...)
		case ActionFollow:
			out = append(out, a.ID[:]...)
		}
	}
	return out
}

var errHistory = errors.New("readingstore: malformed reading history")

// DecodeDay unpacks a day EncodeDay packed. A gallery's ActiveS is its
// bucket's lower bound.
func DecodeDay(day time.Time, b []byte) ([]Action, error) {
	if len(b) == 0 || b[0] != historyVersion {
		return nil, errHistory
	}
	midnight := utcDay(day)
	var out []Action
	for i := 1; i < len(b); {
		if len(b)-i < 2 {
			return nil, errHistory
		}
		a := Action{Kind: ActionKind(b[i] & 0b11), SessionStart: b[i]&(1<<2) != 0, Offset: int16(int8(b[i+1])) * 15}
		i += 2
		minute, n := binary.Uvarint(b[i:])
		if n <= 0 {
			return nil, errHistory
		}
		i += n
		a.At = midnight.Add(time.Duration(minute) * time.Minute)
		switch a.Kind {
		case ActionGallery:
			if len(b)-i < 19 {
				return nil, errHistory
			}
			copy(a.ID[:], b[i:i+16])
			a.Tenths, a.Reaction = b[i+16]&0x0f, Reaction(b[i+16]>>4&0b111)
			if bucket := b[i+17]; bucket > 0 {
				a.ActiveS = 1 << (bucket - 1)
			}
			a.Surface = min(Surface(b[i+18]), SurfaceOther)
			i += 19
		case ActionSearch:
			size, n := binary.Uvarint(b[i:])
			if n <= 0 || uint64(len(b)-i-n) < size {
				return nil, errHistory
			}
			i += n
			a.Query = string(b[i : i+int(size)])
			i += int(size)
		case ActionFollow:
			if len(b)-i < 16 {
				return nil, errHistory
			}
			copy(a.ID[:], b[i:i+16])
			i += 16
		default:
			return nil, errHistory
		}
		out = append(out, a)
	}
	return out, nil
}

func clampOffset(minutes int16) int16 { return min(max(minutes, -14*60), 14*60) }

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
