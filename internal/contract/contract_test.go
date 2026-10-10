package contract

import (
	"os"
	"testing"
)

func TestGeneratedContractIsFresh(t *testing.T) {
	if err := Verify(os.DirFS("../..")); err != nil {
		t.Fatal(err)
	}
}
