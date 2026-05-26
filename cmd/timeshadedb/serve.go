package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"timeshadedb"
)

type webServer struct {
	db     *timeshadedb.DB
	webDir string
}

type metadataResponse struct {
	CanvasWidth  int    `json:"canvasWidth"`
	CanvasHeight int    `json:"canvasHeight"`
	TileSize     int    `json:"tileSize"`
	TileCols     int    `json:"tileCols"`
	TileRows     int    `json:"tileRows"`
	FromSec      uint32 `json:"fromSec"`
	ToSec        uint32 `json:"toSec"`
}

func serveHTTP(ctx context.Context, addr, dbPath, webDir string, cacheSize int64) error {
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: dbPath, ReadOnly: true, CacheSize: cacheSize})
	if err != nil {
		return err
	}
	defer db.Close()

	srv := &http.Server{
		Addr:              addr,
		Handler:           newWebServer(db, webDir).routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "serving timeShadeDB on http://%s\n", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return errors.Join(ctx.Err(), srv.Shutdown(shutdownCtx))
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func newWebServer(db *timeshadedb.DB, webDir string) *webServer {
	return &webServer{db: db, webDir: webDir}
}

func (s *webServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/meta", s.handleMetadata)
	mux.HandleFunc("/api/tiles/", s.handleTile)
	mux.HandleFunc("/", s.handleStatic)
	return mux
}

func (s *webServer) handleMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	from, to := s.db.TimeRange()
	writeAPIJSON(w, metadataResponse{
		CanvasWidth:  timeshadedb.CanvasWidth,
		CanvasHeight: timeshadedb.CanvasHeight,
		TileSize:     timeshadedb.TileSize,
		TileCols:     timeshadedb.TileCols,
		TileRows:     timeshadedb.TileRows,
		FromSec:      from,
		ToSec:        to,
	})
}

func (s *webServer) handleTile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	z, x, y, err := parseTilePath(r.URL.Path)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	if z != 0 || x < 0 || x >= timeshadedb.TileCols || y < 0 || y >= timeshadedb.TileRows {
		writeAPIError(w, http.StatusNotFound, "tile out of bounds")
		return
	}
	ts, err := parseTimestampSec(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.db.TileAt(r.Context(), timeshadedb.TileAtOptions{
		Timestamp: time.Unix(int64(ts), 0).UTC(),
		Tile:      timeshadedb.TileCoord{X: x, Y: y},
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	if err := encodeTilePNG(w, res, timeshadedb.TileSize, timeshadedb.TileSize); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *webServer) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.webDir == "" {
		http.Error(w, "web directory not configured", http.StatusNotFound)
		return
	}
	rel := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if rel == "" || rel == "." {
		rel = "index.html"
	}
	fullPath := filepath.Join(s.webDir, filepath.FromSlash(rel))
	if stat, err := os.Stat(fullPath); err != nil || stat.IsDir() {
		fullPath = filepath.Join(s.webDir, "index.html")
	}
	http.ServeFile(w, r, fullPath)
}

func parseTilePath(urlPath string) (int, int, int, error) {
	rel := strings.TrimPrefix(urlPath, "/api/tiles/")
	parts := strings.Split(rel, "/")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("expected /api/tiles/{z}/{x}/{y}.png")
	}
	yText := strings.TrimSuffix(parts[2], ".png")
	values := []string{parts[0], parts[1], yText}
	parsed := [3]int{}
	for i, value := range values {
		n, err := strconv.Atoi(value)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("invalid tile coordinate %q", value)
		}
		parsed[i] = n
	}
	return parsed[0], parsed[1], parsed[2], nil
}

func parseTimestampSec(r *http.Request) (uint32, error) {
	tsText := r.URL.Query().Get("ts")
	if tsText == "" {
		return 0, fmt.Errorf("missing ts query parameter")
	}
	ts, err := strconv.ParseUint(tsText, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid ts query parameter")
	}
	return uint32(ts), nil
}

func encodeTilePNG(w io.Writer, res *timeshadedb.TileResult, width, height int) error {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < res.Height; y++ {
		for x := 0; x < res.Width; x++ {
			id := res.Pixels[y*res.Width+x]
			if id == 0 {
				continue
			}
			palIdx := int(id) - 1
			if palIdx < 0 || palIdx >= len(res.Palette) {
				return fmt.Errorf("palette id %d out of range", id)
			}
			c := res.Palette[palIdx]
			img.SetRGBA(x, y, color.RGBA{R: c.R, G: c.G, B: c.B, A: 255})
		}
	}
	return png.Encode(w, img)
}

func writeAPIJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
