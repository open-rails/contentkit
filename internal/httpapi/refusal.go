package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
)

// Rule is the rule a refused request broke, for people: what the innermost
// site that wrapped a sentinel put after it (fmt.Errorf("%w: rule",
// ErrInvalid)), else code's Message. The sentinel's text never reaches it,
// nor a wrapped error chain: those stay in the logs.
func Rule(err error, code string) string {
	if r := rule(err); r != "" {
		return r
	}
	return Message(code)
}

func rule(err error) string {
	inner := unwrap(err)
	if len(inner) == 0 {
		return ""
	}
	for _, e := range inner {
		if len(unwrap(e)) > 0 {
			return rule(e)
		}
	}
	// err wraps sentinels only: its text after the first, the others' without
	// their package prefix.
	text, ok := strings.CutPrefix(err.Error(), inner[0].Error()+": ")
	if !ok {
		return ""
	}
	for _, e := range inner[1:] {
		text = strings.ReplaceAll(text, e.Error(), pkgPrefix.ReplaceAllString(e.Error(), ""))
	}
	return text
}

// pkgPrefix is a sentinel's "package: " prefix.
var pkgPrefix = regexp.MustCompile(`^[a-z][a-z0-9_/]*: `)

func unwrap(err error) []error {
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		if e := u.Unwrap(); e != nil {
			return []error{e}
		}
	case interface{ Unwrap() []error }:
		return u.Unwrap()
	}
	return nil
}

// BodyError says what is wrong with a JSON request body that failed to
// decode, for people.
func BodyError(err error) string {
	var tooLarge *http.MaxBytesError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooLarge):
		return fmt.Sprintf("the body is larger than %d bytes", tooLarge.Limit)
	case errors.Is(err, io.EOF):
		return "the body is empty"
	case errors.As(err, &typ) && typ.Field != "":
		return fmt.Sprintf("%s must be %s", typ.Field, jsonKind(typ.Type))
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return strings.TrimPrefix(err.Error(), "json: ")
	}
	return "the body is not valid JSON"
}

func jsonKind(t reflect.Type) string {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return "another type"
	}
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
		reflect.Uint64, reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "a list"
	case reflect.Map, reflect.Struct:
		return "an object"
	}
	return "another type"
}
