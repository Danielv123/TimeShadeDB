package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"timeshadedb/internal/core"
)

// writeIcons puts a spritesheet and metadata named after hash into dir and names them
// in icons.json, files first (as the Clusterio plugin does).
func writeIcons(t *testing.T, dir, hash string, mtime time.Time) {
	t.Helper()
	png, meta := "spritesheet."+hash+".png", "metadata."+hash+".json"
	if err := os.WriteFile(filepath.Join(dir, png), []byte("\x89PNG "+hash), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, meta), []byte(`{"item":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(core.CatalogIcons{PNG: "icons/" + png, JSON: "icons/" + meta})
	p := filepath.Join(dir, IconsFile)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	// Distinct times: the names have the same length, so only the time tells the files apart.
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// TestIconsReloaded: icons.json written or replaced while the server runs reaches the
// catalog at its next build, and only the files it names are served.
func TestIconsReloaded(t *testing.T) {
	icons := t.TempDir()
	f := newFixture(t, fixtureOpt{cfg: func(c *Config) { c.IconsDir = icons }})
	catalog := func() *core.CatalogIcons {
		t.Helper()
		f.srv.invalidateCatalog() // as after catalogTTL
		var cat core.Catalog
		r := f.get("/api/v3/catalog")
		r.wantStatus(t, 200, "catalog")
		r.json(t, &cat)
		return cat.Icons
	}
	if ic := catalog(); ic != nil {
		t.Fatalf("icons %+v before icons.json", ic)
	}
	f.get("/api/v3/icons/spritesheet.aaaa.png").wantStatus(t, 404, "no icons yet")

	t0 := time.Now().Add(-time.Minute)
	writeIcons(t, icons, "aaaa", t0)
	if ic := catalog(); ic == nil || ic.PNG != "icons/spritesheet.aaaa.png" || ic.JSON != "icons/metadata.aaaa.json" {
		t.Fatalf("icons %+v after the first icons.json", ic)
	}
	r := f.get("/api/v3/icons/spritesheet.aaaa.png")
	r.wantStatus(t, 200, "first spritesheet")
	if string(r.body) != "\x89PNG aaaa" {
		t.Fatalf("spritesheet %q", r.body)
	}

	writeIcons(t, icons, "bbbb", t0.Add(time.Second))
	if ic := catalog(); ic == nil || ic.PNG != "icons/spritesheet.bbbb.png" {
		t.Fatalf("icons %+v after the replacement", ic)
	}
	f.get("/api/v3/icons/spritesheet.bbbb.png").wantStatus(t, 200, "new spritesheet")
	f.get("/api/v3/icons/spritesheet.aaaa.png").wantStatus(t, 404, "replaced spritesheet")

	// An icons.json naming a missing file is no icons, and is read again once it is fixed.
	b, _ := json.Marshal(core.CatalogIcons{PNG: "icons/spritesheet.cccc.png", JSON: "icons/metadata.bbbb.json"})
	p := filepath.Join(icons, IconsFile)
	os.WriteFile(p, b, 0o644)
	os.Chtimes(p, t0.Add(2*time.Second), t0.Add(2*time.Second))
	if ic := catalog(); ic != nil {
		t.Fatalf("icons %+v naming a missing file", ic)
	}
	os.WriteFile(filepath.Join(icons, "spritesheet.cccc.png"), []byte("\x89PNG cccc"), 0o644)
	if ic := catalog(); ic == nil || ic.PNG != "icons/spritesheet.cccc.png" {
		t.Fatalf("icons %+v once the file is there", ic)
	}

	os.Remove(p)
	if ic := catalog(); ic != nil {
		t.Fatalf("icons %+v after icons.json was removed", ic)
	}
}
