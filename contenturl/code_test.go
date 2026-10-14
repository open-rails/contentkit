package contenturl

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

type vectors struct {
	Codes []struct {
		In  string  `json:"in"`
		Out *string `json:"out"`
	} `json:"codes"`
	VersionCodes []struct {
		In  string  `json:"in"`
		Out *string `json:"out"`
	} `json:"version_codes"`
	Paths struct {
		Routes    Routes   `json:"routes"`
		Languages []string `json:"languages"`
		Cases     []struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			Query string `json:"query"`
			// Negotiated is the language an unprefixed request asks for.
			Negotiated string `json:"negotiated"`
			Link       *Link  `json:"link"`
			Matched    bool   `json:"matched"`
			Redirect   bool   `json:"redirect"`
			Location   string `json:"location"`
		} `json:"cases"`
	} `json:"paths"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseCodeVectors(t *testing.T) {
	for _, c := range loadVectors(t).Codes {
		got, err := ParseCode(c.In)
		if c.Out == nil {
			if !errors.Is(err, ErrInvalidCode) || !errors.Is(err, ErrInvalid) {
				t.Errorf("ParseCode(%q) = %q, %v; want ErrInvalidCode", c.In, got, err)
			}
			continue
		}
		if err != nil || string(got) != *c.Out || !got.Valid() {
			t.Errorf("ParseCode(%q) = %q, %v; want %q", c.In, got, err, *c.Out)
		}
	}
	if Code("g4vrq3zq5").Valid() || !Code("G4VRQ3ZQ5").Valid() {
		t.Fatal("Valid accepts only the canonical form")
	}
}

func TestParseVersionCodeVectors(t *testing.T) {
	for _, c := range loadVectors(t).VersionCodes {
		got, err := ParseVersionCode(c.In)
		if c.Out == nil {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("ParseVersionCode(%q) = %q, %v; want ErrInvalid", c.In, got, err)
			}
			continue
		}
		if err != nil || string(got) != *c.Out {
			t.Errorf("ParseVersionCode(%q) = %q, %v; want %q", c.In, got, err, *c.Out)
		}
	}
	if got := Versioned("/en/g/G4VRQ3ZQ5/a", "K3F9"); got != "/en/g/G4VRQ3ZQ5/a?v=K3F9" {
		t.Fatal(got)
	}
	if got := Versioned("/en/g/G4VRQ3ZQ5/a?p=2", "K3F9"); got != "/en/g/G4VRQ3ZQ5/a?p=2&v=K3F9" {
		t.Fatal(got)
	}
}

func TestParseCodeAlphabet(t *testing.T) {
	if len(Alphabet) != 32 || strings.ContainsAny(Alphabet, "ILOU") {
		t.Fatalf("Alphabet %q", Alphabet)
	}
	for i := 0; i < len(Alphabet); i++ {
		c := strings.Repeat(Alphabet[i:i+1], CodeLength)
		got, err := ParseCode(strings.ToLower(c))
		if digit := Alphabet[i] <= '9'; digit != (err != nil) || !digit && string(got) != c {
			t.Errorf("ParseCode(%q) = %q, %v", strings.ToLower(c), got, err)
		}
	}
}

func TestPathVectors(t *testing.T) {
	v := loadVectors(t).Paths
	if err := v.Routes.Validate(v.Languages); err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			p, ok := ParsePath(c.Path, v.Routes, v.Languages)
			if c.Link == nil {
				if ok {
					t.Fatalf("ParsePath(%q) = %+v, want no match", c.Path, p)
				}
				return
			}
			if !ok {
				t.Fatalf("ParsePath(%q) did not match", c.Path)
			}
			language := p.Language
			if language == "" {
				language = c.Negotiated
			}
			path, ok := Canonical(v.Routes, v.Languages, language, *c.Link)
			if ok != c.Matched {
				t.Fatalf("Canonical matched=%v, want %v", ok, c.Matched)
			}
			if !ok {
				return
			}
			location := path
			if c.Query != "" {
				location += "?" + c.Query
			}
			if redirect := c.Path != path; redirect != c.Redirect || location != c.Location {
				t.Fatalf("redirect=%v location=%q, want %v %q", redirect, location, c.Redirect, c.Location)
			}
		})
	}
}

func TestSlugify(t *testing.T) {
	long := strings.Repeat("abcdefghi ", 10)
	for in, want := range map[string]string{
		"Night Before the Counteroffensive": "night-before-the-counteroffensive",
		"  Café — crème brûlée!  ":          "cafe-creme-brulee",
		"Don't Stop Me Now":                 "dont-stop-me-now",
		"It’s Over 9000":                    "its-over-9000",
		"Straße & Øresund Æble":             "strasse-oresund-aeble",
		"ＦＵＬＬ　ＷＩＤＴＨ ｶﾀｶﾅ":                   "full-width",
		"東方Project":                         "project",
		"ドキドキ":                              "",
		"!!!":                               "",
		"A--B__C":                           "a-b-c",
		"ﬁnal ﬂight":                        "final-flight",
		long:                                "abcdefghi-abcdefghi-abcdefghi-abcdefghi-abcdefghi-abcdefghi-abcdefghi-abcdefghi",
		strings.Repeat("x", 100):            strings.Repeat("x", 80),
	} {
		got := Slugify(in)
		if got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
		if !ValidSlug(got) || Slugify(got) != got {
			t.Errorf("Slugify(%q) = %q is not a stable valid slug", in, got)
		}
	}
	for _, bad := range []string{"Upper", "-lead", "trail-", "a--b", "a_b", strings.Repeat("a", 81)} {
		if ValidSlug(bad) {
			t.Errorf("ValidSlug(%q) = true", bad)
		}
	}
}

func TestRoutesValidate(t *testing.T) {
	for _, c := range []struct {
		routes    Routes
		languages []string
	}{
		{Routes{}, nil},
		{Routes{"video": "Watch"}, nil},
		{Routes{"video": "watch/now"}, nil},
		{Routes{"": "watch"}, nil},
		{Routes{"video": "watch"}, []string{"watch"}},
		{Routes{"video": "watch"}, []string{"ES"}},
	} {
		if err := c.routes.Validate(c.languages); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%v, %v) = %v", c.routes, c.languages, err)
		}
	}
}
