package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var assets embed.FS

func Files() fs.FS {
	f, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	return f
}
