//go:build !darwin

// macviewer is macOS-only (it uses AppKit, ScreenCaptureKit and
// VideoToolbox); this keeps `go build ./...` working elsewhere.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "macviewer only runs on macOS")
	os.Exit(1)
}
