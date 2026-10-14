// Command gen writes the files generated from the route catalog
// (go generate ./internal/contract).
package main

import (
	"flag"
	"log"

	"github.com/open-rails/contentkit/internal/contract"
)

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if err := contract.Write(*root); err != nil {
		log.Fatal(err)
	}
}
