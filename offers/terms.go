package offers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Amount is money in micros (millionths) of a currency unit. It reads and
// writes as a decimal: "4.99" (JSON strings or numbers, YAML scalars).
type Amount int64

// MaxAmount bounds every price: 999999.99.
const MaxAmount Amount = 999_999_990_000

var amountRe = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})(\.[0-9]{1,6})?$`)

// ParseAmount parses a non-negative decimal with at most six decimals.
func ParseAmount(s string) (Amount, error) {
	if !amountRe.MatchString(s) {
		return 0, fmt.Errorf("%w: amount %q is not a decimal like 4.99", ErrInvalid, s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	w, _ := strconv.ParseInt(whole, 10, 64)
	f, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	return Amount(w*1_000_000 + f), nil
}

// String is the shortest decimal with at least two decimals: "4.99", "0.50".
func (a Amount) String() string {
	s := fmt.Sprintf("%d.%06d", a/1_000_000, a%1_000_000)
	for strings.HasSuffix(s, "0") && len(s)-strings.IndexByte(s, '.') > 3 {
		s = s[:len(s)-1]
	}
	return s
}

func (a Amount) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

func (a *Amount) UnmarshalText(b []byte) error {
	v, err := ParseAmount(string(b))
	*a = v
	return err
}

// UnmarshalJSON takes a decimal string or number.
func (a *Amount) UnmarshalJSON(b []byte) error {
	if bytes.HasPrefix(b, []byte(`"`)) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		b = []byte(s)
	}
	return a.UnmarshalText(b)
}

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// Terms are what an offer sells for.
type Terms struct {
	// Amount is the price; "0" makes an entity free to join.
	Amount Amount `json:"amount"`
	// Currency is an ISO 4217 code; Config.Currency when empty.
	Currency string `json:"currency"`
	// AccessHours is how long a purchase opens it; none is forever.
	AccessHours *int `json:"access_hours,omitempty"`
	// RenewHours makes an entity's offer a subscription renewing this often.
	RenewHours *int `json:"renew_hours,omitempty"`
}

func (t Terms) equal(o Terms) bool {
	return t.Amount == o.Amount && t.Currency == o.Currency && eqInt(t.AccessHours, o.AccessHours) && eqInt(t.RenewHours, o.RenewHours)
}

func eqInt(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// Config is the engine's offer policy.
type Config struct {
	// Currency is the default currency: USD when empty.
	Currency string `json:"currency,omitempty" yaml:"currency"`
	// Minimum is the least amount per target kind; a version without its own
	// takes content's. Free (0) is allowed for entities only.
	Minimum map[TargetKind]Amount `json:"minimum,omitempty" yaml:"minimum"`
	// PSPs are the payment providers selling every price.
	PSPs []string `json:"psps,omitempty" yaml:"psps"`
}

func (c Config) minimum(k TargetKind) Amount {
	if m, ok := c.Minimum[k]; ok {
		return m
	}
	if k == VersionTarget {
		return c.Minimum[ContentTarget]
	}
	return 0
}

// normalize fills defaults and checks terms for a target kind.
func (c Config) normalize(k TargetKind, t Terms) (Terms, error) {
	if t.Currency == "" {
		t.Currency = c.Currency
		if t.Currency == "" {
			t.Currency = "USD"
		}
	}
	switch {
	case !currencyRe.MatchString(t.Currency):
		return t, fmt.Errorf("%w: currency %q is not an ISO 4217 code", ErrInvalid, t.Currency)
	case t.Amount < 0 || t.Amount > MaxAmount:
		return t, fmt.Errorf("%w: amount must be at most %s", ErrInvalid, MaxAmount)
	case t.Amount == 0 && k != EntityTarget:
		return t, fmt.Errorf("%w: only a group may be free", ErrInvalid)
	case t.Amount == 0 && (t.RenewHours != nil || t.AccessHours != nil):
		return t, fmt.Errorf("%w: a free group takes no access or renewal hours", ErrInvalid)
	case t.Amount > 0 && t.Amount < c.minimum(k):
		return t, fmt.Errorf("%w: a %s sells for at least %s", ErrInvalid, k, c.minimum(k))
	case t.RenewHours != nil && k != EntityTarget:
		return t, fmt.Errorf("%w: only a group renews", ErrInvalid)
	case t.RenewHours != nil && *t.RenewHours < 1, t.AccessHours != nil && *t.AccessHours < 1:
		return t, fmt.Errorf("%w: hours must be positive", ErrInvalid)
	}
	return t, nil
}
