//go:build embed

package server

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embeddedWeb embed.FS

func webFS() (fs.FS, bool) {
	sub, err := fs.Sub(embeddedWeb, "dist")
	if err != nil {
		return nil, false
	}
	return sub, true
}
