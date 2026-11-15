//go:build !darwin

// vtrace records encoder traces; it needs a platform encoder (macOS today).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "vtrace needs a platform encoder (macOS for now)")
	os.Exit(1)
}
