package media

import (
	"errors"
	"fmt"
	"strings"
)

// ErrFolderNotEmpty: a new item's folder already holds objects. Item ids are
// never reused, so leftovers mean a host bug (a reset id, a restored
// database); they are never adopted. Purge them deliberately with Jobs.Purge.
var ErrFolderNotEmpty = errors.New("media: new item's folder is not empty")

// FolderNotEmptyError names the folder and a few of its keys.
type FolderNotEmptyError struct {
	Prefix string
	Keys   []string
}

func (e *FolderNotEmptyError) Error() string {
	return fmt.Sprintf("%v: %s holds %s", ErrFolderNotEmpty, e.Prefix, strings.Join(e.Keys, ", "))
}

func (e *FolderNotEmptyError) Unwrap() error { return ErrFolderNotEmpty }
