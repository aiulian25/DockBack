// Package web embeds the built React frontend into the Go binary so the final
// image is a single binary with no separate asset files (PLAN §1.3).
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the embedded frontend file system rooted at the build output.
func FS() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
