//go:build darwin

package media

// The Objective-C sources are compiled through this preamble (as
// Objective-C) rather than as .m files: cmd/go adds its own -lobjc for
// every package with .m files, so linking this package together with
// another Objective-C user (Gio's app adds -lobjc too) made the linker warn
// about a duplicate library. The frameworks linked here bring the
// Objective-C runtime with them.

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#include "vt_darwin.objc"
#include "capture_darwin.objc"
*/
import "C"
