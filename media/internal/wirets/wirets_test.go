package wirets

import (
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite sdk/upload/src/wire.gen.ts")

const path = "../../../sdk/upload/src/wire.gen.ts"

func TestWireTypesCurrent(t *testing.T) {
	want := Render()
	if *update {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatal("sdk/upload/src/wire.gen.ts is stale; run go test ./media/internal/wirets -update")
	}
}
