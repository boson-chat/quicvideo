//go:build !darwin

package main

import "log"

func windowScreenshot(string) { log.Print("window screenshot: not implemented on this platform") }
