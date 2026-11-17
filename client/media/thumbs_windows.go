//go:build windows && cgo

package media

/*
#include <stdlib.h>
#include "media_windows.h"
*/
import "C"

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"runtime/cgo"
	"sync"
	"time"
)

// Picker thumbnails on Windows: Windows.Graphics.Capture has no one-shot
// screenshot, and starting a capture per thumbnail would flash Windows'
// capture border on every source twice a second. So each source the
// picker asks about gets a low-rate capture (2 fps, no cursor, borderless
// where Windows 11 allows it) that keeps its latest picture, scaled down;
// ScreenThumbnail hands that out as a JPEG. A source nobody has asked
// about for 3 s stops, and StopThumbnails (the picker closed) stops all.
const (
	thumbFPS       = 2
	thumbIdle      = 3 * time.Second
	thumbFirstWait = 800 * time.Millisecond
	thumbQuality   = 70
)

// ErrNoThumbnail: the source has shown nothing yet (a minimized window).
var ErrNoThumbnail = errors.New("media: no picture of that source yet")

type thumbCap struct {
	src   *winSource
	first chan struct{}
	once  sync.Once

	mu    sync.Mutex
	img   *image.RGBA
	jpg   []byte // img as JPEG, made on demand
	asked time.Time
}

var thumbs struct {
	mu       sync.Mutex
	caps     map[[2]uint32]*thumbCap
	sweeping bool
}

// ScreenThumbnail is a small JPEG (at most maxW wide) of a display or a
// window, from its thumbnail capture (started on the first call).
func ScreenThumbnail(displayID, windowID uint32, maxW int) ([]byte, error) {
	key := [2]uint32{displayID, windowID}
	if windowID != 0 {
		key[0] = 0
	}
	thumbs.mu.Lock()
	tc := thumbs.caps[key]
	if tc == nil {
		var err error
		if tc, err = startThumbCap(displayID, windowID, maxW); err != nil {
			thumbs.mu.Unlock()
			return nil, err
		}
		if thumbs.caps == nil {
			thumbs.caps = map[[2]uint32]*thumbCap{}
		}
		thumbs.caps[key] = tc
		if !thumbs.sweeping {
			thumbs.sweeping = true
			go sweepThumbs()
		}
	}
	thumbs.mu.Unlock()

	tc.mu.Lock()
	tc.asked = time.Now()
	tc.mu.Unlock()
	select {
	case <-tc.first:
	case <-time.After(thumbFirstWait):
		return nil, ErrNoThumbnail
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.jpg == nil {
		var b bytes.Buffer
		if err := jpeg.Encode(&b, tc.img, &jpeg.Options{Quality: thumbQuality}); err != nil {
			return nil, err
		}
		tc.jpg = b.Bytes()
	}
	return tc.jpg, nil
}

func startThumbCap(displayID, windowID uint32, maxW int) (*thumbCap, error) {
	if C.qv_wgc_supported() == 0 {
		return nil, ErrUnsupported
	}
	if maxW <= 0 {
		maxW = 320
	}
	tc := &thumbCap{first: make(chan struct{}), asked: time.Now()}
	tc.src = &winSource{fps: thumbFPS, push: func(f CapturedFrame) {
		img, err := ToRGBA(f.Picture, maxW)
		if err != nil {
			return
		}
		tc.mu.Lock()
		tc.img, tc.jpg = img, nil
		tc.mu.Unlock()
		tc.once.Do(func() { close(tc.first) })
	}}
	tc.src.handle = cgo.NewHandle(tc.src)
	monitor := 0
	if displayID != 0 {
		monitor = int(displayID) - 1
	}
	var errp *C.char
	c := C.qv_wgc_start(C.int(monitor), C.uintptr_t(windowID), C.int(thumbFPS), 0, 1, C.uintptr_t(tc.src.handle), &errp)
	if c == nil {
		tc.src.handle.Delete()
		return nil, cerr(errp)
	}
	tc.src.stopFn = func() { C.qv_wgc_stop(c) }
	return tc, nil
}

// sweepThumbs stops captures nobody asks about any more.
func sweepThumbs() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		var idle []*thumbCap
		thumbs.mu.Lock()
		for k, tc := range thumbs.caps {
			tc.mu.Lock()
			old := time.Since(tc.asked) > thumbIdle
			tc.mu.Unlock()
			if old {
				idle = append(idle, tc)
				delete(thumbs.caps, k)
			}
		}
		done := len(thumbs.caps) == 0
		if done {
			thumbs.sweeping = false
		}
		thumbs.mu.Unlock()
		for _, tc := range idle {
			tc.src.Stop()
		}
		if done {
			return
		}
	}
}

// StopThumbnails stops every thumbnail capture (the picker closed).
func StopThumbnails() {
	thumbs.mu.Lock()
	caps := thumbs.caps
	thumbs.caps = nil
	thumbs.mu.Unlock()
	for _, tc := range caps {
		tc.src.Stop()
	}
}

// thumbCaptures is how many thumbnail captures run (tests).
func thumbCaptures() int {
	thumbs.mu.Lock()
	defer thumbs.mu.Unlock()
	return len(thumbs.caps)
}
