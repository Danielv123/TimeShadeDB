package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"timeshadedb"
)

// TestChunkTileCacheHeaders checks B4: a chunk tile is cached as immutable only when
// it was read successfully at a tick below the datastore's latest tick.
func TestChunkTileCacheHeaders(t *testing.T) {
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: filepath.Join(t.TempDir(), "db.tshd"), Format: timeshadedb.FormatChunks})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler := newWebServer(db, http.NotFoundHandler()).routes()
	body := "10\tnauvis\t-7,-6\tplayer\t" + testRGB565Hex(0xf800) + "\n" +
		"20\tnauvis\t-7,-6\tplayer\t" + testRGB565Hex(0x07e0) + "\n"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/ingest/chunk/save-1", strings.NewReader(body)))
	if resp.Code != http.StatusOK {
		t.Fatalf("ingest status = %d, body %s", resp.Code, resp.Body.String())
	}

	cases := []struct {
		path   string
		status int
		cache  string
	}{
		{"/api/chunk/tiles/save-1/player/nauvis/0/-1/-1.png?tick=10", http.StatusOK, "public, max-age=31536000, immutable"},
		{"/api/chunk/tiles/save-1/player/nauvis/0/-1/-1.png?tick=19", http.StatusOK, "public, max-age=31536000, immutable"},
		{"/api/chunk/tiles/save-1/player/nauvis/0/-1/-1.png?tick=20", http.StatusOK, "no-cache"},
		{"/api/chunk/tiles/save-1/player/nauvis/0/-1/-1.png?tick=999", http.StatusOK, "no-cache"},
		{"/api/chunk/tiles/save-1/player/nauvis/-3/-1/-1.png?tick=5", http.StatusOK, "public, max-age=31536000, immutable"},
		// Unknown datastores used to render as cached black tiles.
		{"/api/chunk/tiles/save-2/player/nauvis/0/-1/-1.png?tick=5", http.StatusNotFound, "no-store"},
		{"/api/chunk/tiles/save-1/enemy/nauvis/0/-1/-1.png?tick=5", http.StatusNotFound, "no-store"},
		{"/api/chunk/tiles/save-1/player/nauvis/0/-1/-1.png", http.StatusBadRequest, "no-store"},
		// Coordinates outside the world limit are rejected before any work.
		{"/api/chunk/tiles/save-1/player/nauvis/1/0/0.png?tick=5", http.StatusNotFound, "no-store"},
		{"/api/chunk/tiles/save-1/player/nauvis/0/8192/0.png?tick=5", http.StatusNotFound, "no-store"},
		{"/api/chunk/tiles/save-1/player/nauvis/0/0/-8193.png?tick=5", http.StatusNotFound, "no-store"},
		{"/api/chunk/tiles/save-1/player/nauvis/0/4294967296/0.png?tick=5", http.StatusNotFound, "no-store"},
		{"/api/chunk/tiles/save-1/player/nauvis/-13/1/0.png?tick=5", http.StatusNotFound, "no-store"},
		{"/api/chunk/tiles/save-1/player/nauvis/-14/0/0.png?tick=5", http.StatusNotFound, "no-store"},
		{"/api/chunk/tiles/save-1/player/nauvis/-30/0/0.png?tick=5", http.StatusNotFound, "no-store"},
	}
	for _, tc := range cases {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if resp.Code != tc.status {
			t.Errorf("%s: status = %d, want %d (body %s)", tc.path, resp.Code, tc.status, resp.Body.String())
			continue
		}
		if got := resp.Header().Get("Cache-Control"); got != tc.cache {
			t.Errorf("%s: Cache-Control = %q, want %q", tc.path, got, tc.cache)
		}
	}
}

func TestChunkTileInBounds(t *testing.T) {
	cases := []struct {
		z, x, y int
		ok      bool
	}{
		{0, 0, 0, true},
		{0, -8192, 8191, true},
		{0, 8192, 0, false},
		{0, 0, -8193, false},
		{-1, -4096, 4095, true},
		{-1, 4096, 0, false},
		{-12, -2, 1, true},
		{-12, 2, 0, false},
		{-13, -1, 0, true},
		{-13, 1, 0, false},
		{-14, 0, 0, false},
		{1, 0, 0, false},
	}
	for _, tc := range cases {
		if got := chunkTileInBounds(tc.z, tc.x, tc.y); got != tc.ok {
			t.Errorf("chunkTileInBounds(%d, %d, %d) = %v, want %v", tc.z, tc.x, tc.y, got, tc.ok)
		}
	}
}
