// Package versionid carries a version id between Go, where a content key's
// is "" for the work itself (contentref.ContentKey), and Postgres, where no
// version is NULL.
package versionid

import (
	"database/sql"
	"fmt"
)

// Arg is v as a query argument: NULL for the work itself.
func Arg(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// Into scans a nullable version id column into *v.
func Into(v *string) sql.Scanner { return into{v} }

type into struct{ v *string }

func (i into) Scan(src any) error {
	switch x := src.(type) {
	case nil:
		*i.v = ""
	case string:
		*i.v = x
	case []byte:
		*i.v = string(x)
	default:
		return fmt.Errorf("versionid: cannot scan %T", src)
	}
	return nil
}
