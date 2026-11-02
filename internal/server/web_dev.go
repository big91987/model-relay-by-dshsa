//go:build !embed

package server

import "io/fs"

func webFS() (fs.FS, bool) { return nil, false }
