// LoadWAV reads a WAV file from disk. See DecodeWAV for supported formats.
package main

import (
	"fmt"
	"os"
)

// set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: callswarm version")
	os.Exit(2)
}
