package http

import (
	"io/fs"

	"github.com/boson-chat/quicvideo/web"
)

// webFS is the built browser demo (web/dist, embedded).
func webFS() fs.FS {
	sub, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
