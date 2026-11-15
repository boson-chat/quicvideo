//go:build darwin

package main

// Info.plist (NSCameraUsageDescription) is embedded into the binary so
// macOS lets an unbundled build open the camera.

/*
#cgo LDFLAGS: -Wl,-sectcreate,__TEXT,__info_plist,${SRCDIR}/Info.plist -framework CoreGraphics -framework CoreFoundation
#include <CoreGraphics/CoreGraphics.h>
#include <unistd.h>

// windowNumber finds this process's largest on-screen window (for
// screencapture -l).
static int windowNumber(void) {
	CFArrayRef list = CGWindowListCopyWindowInfo(kCGWindowListOptionOnScreenOnly, kCGNullWindowID);
	if (!list) return 0;
	int best = 0;
	double bestArea = 0;
	for (CFIndex i = 0; i < CFArrayGetCount(list); i++) {
		CFDictionaryRef w = CFArrayGetValueAtIndex(list, i);
		int pid = 0, num = 0;
		CFNumberGetValue(CFDictionaryGetValue(w, kCGWindowOwnerPID), kCFNumberIntType, &pid);
		if (pid != getpid()) continue;
		CFNumberGetValue(CFDictionaryGetValue(w, kCGWindowNumber), kCFNumberIntType, &num);
		CGRect r;
		CGRectMakeWithDictionaryRepresentation(CFDictionaryGetValue(w, kCGWindowBounds), &r);
		if (r.size.width * r.size.height > bestArea) {
			bestArea = r.size.width * r.size.height;
			best = num;
		}
	}
	CFRelease(list);
	return best;
}
*/
import "C"

import (
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/boson-chat/quicvideo/client/media"
)

// windowScreenshot saves a real capture of the viewer window (needs the
// Screen Recording permission).
func windowScreenshot(path string) {
	if media.ScreenPermission() != media.PermissionGranted {
		log.Print("no window screenshot: Screen Recording permission missing")
		return
	}
	win := strings.TrimSuffix(path, ".png") + "-window.png"
	id := int(C.windowNumber())
	if id == 0 {
		log.Print("no window screenshot: window not found")
		return
	}
	if out, err := exec.Command("screencapture", "-x", "-o", fmt.Sprintf("-l%d", id), win).CombinedOutput(); err != nil {
		log.Printf("window screenshot failed: %v %s", err, out)
		return
	}
	log.Printf("window screenshot written to %s", win)
}
