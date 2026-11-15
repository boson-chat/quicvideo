// Package web embeds the built browser client and demo page (web/dist).
// Rebuild with `make web` (needs Node).
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
