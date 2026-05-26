package timeshadedb

import (
	"embed"
	"io/fs"
)

//go:embed web/dist
var webDist embed.FS

func WebDistFS() (fs.FS, error) {
	return fs.Sub(webDist, "web/dist")
}
