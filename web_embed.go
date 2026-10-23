package timeshadedb

import (
	"embed"
	"io/fs"
)

// webDist holds the Vite bundle. The "all:" prefix embeds web/dist/.keep, which is
// committed so that `go build ./...` works on a clean checkout before `npm run build`.
//
//go:embed all:web/dist
var webDist embed.FS

// webPlaceholder is served when the binary was built without a web bundle.
//
//go:embed web_placeholder/index.html
var webPlaceholder embed.FS

// WebDistFS returns the embedded web bundle. When the bundle was not built
// (web/dist holds only .keep), it returns a placeholder page that explains how
// to build it, so the server still starts and serves an index.html.
func WebDistFS() (fs.FS, error) {
	dist, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(dist, "index.html"); err == nil {
		return dist, nil
	}
	return fs.Sub(webPlaceholder, "web_placeholder")
}

// WebBundleBuilt reports whether a real web bundle is embedded.
func WebBundleBuilt() bool {
	dist, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		return false
	}
	_, err = fs.Stat(dist, "index.html")
	return err == nil
}
