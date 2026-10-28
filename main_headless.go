//go:build headless

// The headless-only build (phase6-headless.md §2): no Wails, no cgo.
//
//	CGO_ENABLED=0 go build -tags headless -trimpath -o ssh-gete .
//
// It always runs headless; --headless is accepted and implied.
package main

import (
	"embed"
	"fmt"
	"os"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	opts, err := parseCLI(os.Args[1:], false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh-gete: %v\n\n%s", err, cliUsage())
		os.Exit(exitUsage)
	}
	os.Exit(runHeadless(opts, embeddedDist()))
}
