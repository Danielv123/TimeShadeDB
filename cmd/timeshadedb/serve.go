package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"timeshadedb"
)

type webServer struct {
	db     *timeshadedb.DB
	static http.Handler
}

const (
	maxChunkIngestRows = 16384
	maxChunkIngestBody = maxChunkIngestRows * 4608
)

type metadataResponse struct {
	CanvasWidth  int    `json:"canvasWidth"`
	CanvasHeight int    `json:"canvasHeight"`
	TileSize     int    `json:"tileSize"`
	TileCols     int    `json:"tileCols"`
	TileRows     int    `json:"tileRows"`
	MinZoom      int    `json:"minZoom"`
	FromSec      uint32 `json:"fromSec"`
	ToSec        uint32 `json:"toSec"`
}

const viteDevURL = "http://127.0.0.1:5173"

func serveHTTP(ctx context.Context, addr, dbPath string, cacheSize int64, dev bool) error {
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: dbPath, CacheSize: cacheSize, Format: timeshadedb.FormatChunks})
	if err != nil {
		return err
	}
	defer db.Close()

	static, cleanup, err := newStaticHandler(ctx, dev)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := &http.Server{
		Addr:              addr,
		Handler:           newWebServer(db, static).routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if dev {
			fmt.Fprintf(os.Stderr, "serving timeShadeDB on http://%s with Vite at %s\n", addr, viteDevURL)
		} else {
			fmt.Fprintf(os.Stderr, "serving timeShadeDB on http://%s\n", addr)
		}
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

func newStaticHandler(ctx context.Context, dev bool) (http.Handler, func(), error) {
	if dev {
		cmd, err := startViteDevServer(ctx)
		if err != nil {
			return nil, func() {}, err
		}
		cleanup := func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
		}
		if err := waitForHTTP(ctx, viteDevURL, 10*time.Second); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		proxy, err := newViteDevProxy(viteDevURL)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		return proxy, cleanup, nil
	}
	fsys, err := timeshadedb.WebDistFS()
	if err != nil {
		return nil, func() {}, err
	}
	return newSPAFileServer(fsys), func() {}, nil
}

func startViteDevServer(ctx context.Context) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "npm", "run", "dev", "--", "--host", "127.0.0.1", "--port", "5173", "--strictPort")
	cmd.Dir = "web"
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func waitForHTTP(ctx context.Context, rawURL string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := http.Client{Timeout: time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vite dev server did not become ready at %s: %w", rawURL, ctx.Err())
		case <-ticker.C:
		}
	}
}

func newViteDevProxy(rawURL string) (http.Handler, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	baseDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		baseDirector(r)
		r.Host = target.Host
	}
	return proxy, nil
}

func newSPAFileServer(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if rel == "" || rel == "." {
			rel = "index.html"
		}
		if stat, err := fs.Stat(fsys, rel); err != nil || stat.IsDir() {
			rel = "index.html"
		}
		http.ServeFileFS(w, r, fsys, rel)
	})
}

func newWebServer(db *timeshadedb.DB, static http.Handler) *webServer {
	if static == nil {
		static = http.NotFoundHandler()
	}
	return &webServer{db: db, static: static}
}

func (s *webServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/meta", s.handleMetadata)
	mux.HandleFunc("/api/tiles/", s.handleTile)
	mux.HandleFunc("/api/chunk/saves", s.handleChunkSaves)
	mux.HandleFunc("/api/chunk/saves/", s.handleChunkSaves)
	mux.HandleFunc("/api/chunk/tiles/", s.handleChunkTile)
	mux.HandleFunc("/api/ingest/chunk/", s.handleChunkIngest)
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
		MinZoom:      minTileZoom(timeshadedb.TileCols, timeshadedb.TileRows),
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
	if z > 0 || !tileInZoomBounds(z, x, y) {
		writeAPIError(w, http.StatusNotFound, "tile out of bounds")
		return
	}
	ts, err := parseTimestampSec(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.tileAtZoom(r.Context(), time.Unix(int64(ts), 0).UTC(), z, x, y)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var buf bytes.Buffer
	if err := encodeTilePNG(&buf, res, timeshadedb.TileSize, timeshadedb.TileSize, png.BestSpeed); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writePNGResponse(w, buf.Bytes())
}

func (s *webServer) handleChunkSaves(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	catalog, err := s.db.ChunkSaveCatalog()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rel := strings.TrimPrefix(path.Clean(r.URL.Path), "/api/chunk/saves")
	if rel == "" || rel == "." || rel == "/" {
		writeAPIJSON(w, catalog)
		return
	}
	savefileUUID, err := url.PathUnescape(strings.TrimPrefix(rel, "/"))
	if err != nil || strings.TrimSpace(savefileUUID) == "" || strings.Contains(savefileUUID, "/") {
		writeAPIError(w, http.StatusNotFound, "save not found")
		return
	}
	for _, save := range catalog.Saves {
		if save.SavefileUUID == savefileUUID {
			writeAPIJSON(w, save)
			return
		}
	}
	writeAPIError(w, http.StatusNotFound, "save not found")
}

func (s *webServer) handleChunkTile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	key, z, x, y, err := parseChunkTilePath(r.URL.Path)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	if z > 0 {
		writeAPIError(w, http.StatusNotFound, "tile out of bounds")
		return
	}
	tick, err := parseGameTick(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	img, err := s.chunkTileAtZoom(r.Context(), key, tick, z, x, y)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writePNGResponse(w, buf.Bytes())
}

func (s *webServer) handleChunkIngest(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "database is not open")
		return
	}
	savefileUUID, isMeta, err := parseChunkIngestPath(r.URL.Path)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, err.Error())
		return
	}
	if isMeta {
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		meta, err := s.db.IngestMetadata(savefileUUID)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAPIJSON(w, meta)
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxChunkIngestBody)
	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 0, 8192), 1024*1024)
	rows, err := timeshadedb.ParseChunkTSV(savefileUUID, scanner)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(rows) == 0 {
		writeAPIError(w, http.StatusBadRequest, "request body contains no rows")
		return
	}
	if len(rows) > maxChunkIngestRows {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "too many rows in request")
		return
	}
	result, err := s.db.IngestChunkRows(r.Context(), rows)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeAPIJSON(w, result)
}

func (s *webServer) tileAtZoom(ctx context.Context, ts time.Time, z, x, y int) (*timeshadedb.TileResult, error) {
	if z == 0 {
		return s.db.TileAt(ctx, timeshadedb.TileAtOptions{
			Timestamp: ts,
			Tile:      timeshadedb.TileCoord{X: x, Y: y},
		})
	}
	return s.downsampledTileAt(ctx, ts, z, x, y)
}

func (s *webServer) chunkTileAtZoom(ctx context.Context, key timeshadedb.DatastoreKey, tick uint64, z, x, y int) (*image.RGBA, error) {
	if z == 0 {
		return s.renderChunkTile(ctx, key, tick, int32(x), int32(y)), nil
	}
	return s.downsampledChunkTileAt(ctx, key, tick, z, x, y)
}

func (s *webServer) renderChunkTile(ctx context.Context, key timeshadedb.DatastoreKey, tick uint64, tileX, tileY int32) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, timeshadedb.TileSize, timeshadedb.TileSize))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xff
	}
	chunksPerTile := timeshadedb.TileSize / timeshadedb.ChunkSize
	baseChunkX := tileX * int32(chunksPerTile)
	baseChunkY := tileY * int32(chunksPerTile)
	for cy := 0; cy < chunksPerTile; cy++ {
		for cx := 0; cx < chunksPerTile; cx++ {
			coord := timeshadedb.ChunkCoord{X: baseChunkX + int32(cx), Y: baseChunkY + int32(cy)}
			chunk, err := s.db.ChunkAt(ctx, timeshadedb.ChunkAtOptions{Key: key, Tick: tick, Chunk: coord})
			if err != nil {
				continue
			}
			for py := 0; py < timeshadedb.ChunkSize; py++ {
				for px := 0; px < timeshadedb.ChunkSize; px++ {
					color565 := chunk.Pixels[py*timeshadedb.ChunkSize+px]
					offset := img.PixOffset(cx*timeshadedb.ChunkSize+px, cy*timeshadedb.ChunkSize+py)
					r, g, b := rgb565ToRGB(color565)
					img.Pix[offset] = r
					img.Pix[offset+1] = g
					img.Pix[offset+2] = b
					img.Pix[offset+3] = 0xff
				}
			}
		}
	}
	return img
}

func (s *webServer) downsampledChunkTileAt(ctx context.Context, key timeshadedb.DatastoreKey, tick uint64, z, x, y int) (*image.RGBA, error) {
	scale, err := zoomScale(z)
	if err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, timeshadedb.TileSize, timeshadedb.TileSize))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xff
	}
	worldX0 := int64(x) * int64(timeshadedb.TileSize) * scale
	worldY0 := int64(y) * int64(timeshadedb.TileSize) * scale
	sampleOffset := scale / 2
	chunks := map[timeshadedb.ChunkCoord]*timeshadedb.ChunkResult{}
	for outY := 0; outY < timeshadedb.TileSize; outY++ {
		srcY := worldY0 + int64(outY)*scale + sampleOffset
		chunkY := int32(floorDivInt64(srcY, int64(timeshadedb.ChunkSize)))
		localY := int(srcY - int64(chunkY)*int64(timeshadedb.ChunkSize))
		for outX := 0; outX < timeshadedb.TileSize; outX++ {
			srcX := worldX0 + int64(outX)*scale + sampleOffset
			chunkX := int32(floorDivInt64(srcX, int64(timeshadedb.ChunkSize)))
			localX := int(srcX - int64(chunkX)*int64(timeshadedb.ChunkSize))
			coord := timeshadedb.ChunkCoord{X: chunkX, Y: chunkY}
			chunk, ok := chunks[coord]
			if !ok {
				chunk, _ = s.db.ChunkAt(ctx, timeshadedb.ChunkAtOptions{Key: key, Tick: tick, Chunk: coord})
				chunks[coord] = chunk
			}
			if chunk == nil {
				continue
			}
			color565 := chunk.Pixels[localY*timeshadedb.ChunkSize+localX]
			offset := img.PixOffset(outX, outY)
			r, g, b := rgb565ToRGB(color565)
			img.Pix[offset] = r
			img.Pix[offset+1] = g
			img.Pix[offset+2] = b
		}
	}
	return img, nil
}

func (s *webServer) downsampledTileAt(ctx context.Context, ts time.Time, z, x, y int) (*timeshadedb.TileResult, error) {
	scale, err := zoomScale(z)
	if err != nil {
		return nil, err
	}
	worldX0 := int64(x) * int64(timeshadedb.TileSize) * scale
	worldY0 := int64(y) * int64(timeshadedb.TileSize) * scale
	validWidth := downsampledTileSpan(timeshadedb.CanvasWidth, worldX0, scale)
	validHeight := downsampledTileSpan(timeshadedb.CanvasHeight, worldY0, scale)
	if validWidth <= 0 || validHeight <= 0 {
		return nil, fmt.Errorf("tile out of bounds")
	}

	type nativeKey struct {
		x int
		y int
	}
	nativeTiles := make(map[nativeKey]*timeshadedb.TileResult)
	var palette []timeshadedb.RGB
	loadNative := func(tileX, tileY int) (*timeshadedb.TileResult, error) {
		key := nativeKey{x: tileX, y: tileY}
		if res := nativeTiles[key]; res != nil {
			return res, nil
		}
		res, err := s.db.TileAt(ctx, timeshadedb.TileAtOptions{
			Timestamp: ts,
			Tile:      timeshadedb.TileCoord{X: tileX, Y: tileY},
		})
		if err != nil {
			return nil, err
		}
		nativeTiles[key] = res
		if palette == nil {
			palette = res.Palette
		}
		return res, nil
	}

	pixels := make([]uint8, validWidth*validHeight)
	sampleOffset := scale / 2
	for outY := 0; outY < validHeight; outY++ {
		srcY := worldY0 + int64(outY)*scale + sampleOffset
		if srcY >= int64(timeshadedb.CanvasHeight) {
			continue
		}
		tileY := int(srcY / int64(timeshadedb.TileSize))
		localY := int(srcY % int64(timeshadedb.TileSize))
		for outX := 0; outX < validWidth; outX++ {
			srcX := worldX0 + int64(outX)*scale + sampleOffset
			if srcX >= int64(timeshadedb.CanvasWidth) {
				continue
			}
			tileX := int(srcX / int64(timeshadedb.TileSize))
			localX := int(srcX % int64(timeshadedb.TileSize))
			native, err := loadNative(tileX, tileY)
			if err != nil {
				return nil, err
			}
			if localX < native.Width && localY < native.Height {
				pixels[outY*validWidth+outX] = native.Pixels[localY*native.Width+localX]
			}
		}
	}
	if palette == nil {
		palette = []timeshadedb.RGB{}
	}
	return &timeshadedb.TileResult{
		Tile:    timeshadedb.TileCoord{X: x, Y: y},
		Width:   validWidth,
		Height:  validHeight,
		Palette: palette,
		Pixels:  pixels,
	}, nil
}

func (s *webServer) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.static.ServeHTTP(w, r)
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

func parseChunkTilePath(urlPath string) (timeshadedb.DatastoreKey, int, int, int, error) {
	rel := strings.TrimPrefix(path.Clean(urlPath), "/api/chunk/tiles/")
	parts := strings.Split(rel, "/")
	if len(parts) != 6 {
		return timeshadedb.DatastoreKey{}, 0, 0, 0, fmt.Errorf("expected /api/chunk/tiles/{savefile_uuid}/{force}/{surface}/{z}/{x}/{y}.png")
	}
	savefileUUID, err := url.PathUnescape(parts[0])
	if err != nil {
		return timeshadedb.DatastoreKey{}, 0, 0, 0, fmt.Errorf("invalid savefile UUID")
	}
	force, err := url.PathUnescape(parts[1])
	if err != nil {
		return timeshadedb.DatastoreKey{}, 0, 0, 0, fmt.Errorf("invalid force")
	}
	surface, err := url.PathUnescape(parts[2])
	if err != nil {
		return timeshadedb.DatastoreKey{}, 0, 0, 0, fmt.Errorf("invalid surface")
	}
	yText := strings.TrimSuffix(parts[5], ".png")
	values := []string{parts[3], parts[4], yText}
	parsed := [3]int{}
	for i, value := range values {
		n, err := strconv.Atoi(value)
		if err != nil {
			return timeshadedb.DatastoreKey{}, 0, 0, 0, fmt.Errorf("invalid tile coordinate %q", value)
		}
		parsed[i] = n
	}
	return timeshadedb.DatastoreKey{SavefileUUID: savefileUUID, Surface: surface, Force: force}, parsed[0], parsed[1], parsed[2], nil
}

func parseChunkIngestPath(urlPath string) (string, bool, error) {
	rel := strings.TrimPrefix(path.Clean(urlPath), "/api/ingest/chunk/")
	if rel == "" || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false, fmt.Errorf("expected /api/ingest/chunk/{savefile_uuid}")
	}
	parts := strings.Split(rel, "/")
	if len(parts) == 1 {
		return parts[0], false, nil
	}
	if len(parts) == 2 && parts[1] == "meta" {
		return parts[0], true, nil
	}
	return "", false, fmt.Errorf("expected /api/ingest/chunk/{savefile_uuid} or /api/ingest/chunk/{savefile_uuid}/meta")
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

func parseGameTick(r *http.Request) (uint64, error) {
	tickText := r.URL.Query().Get("tick")
	if tickText == "" {
		return 0, fmt.Errorf("missing tick query parameter")
	}
	tick, err := strconv.ParseUint(tickText, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid tick query parameter")
	}
	return tick, nil
}

func rgb565ToRGB(v uint16) (uint8, uint8, uint8) {
	r := uint8((uint32(v>>11) & 0x1f) * 255 / 31)
	g := uint8((uint32(v>>5) & 0x3f) * 255 / 63)
	b := uint8((uint32(v) & 0x1f) * 255 / 31)
	return r, g, b
}

func minTileZoom(cols, rows int) int {
	maxTiles := cols
	if rows > maxTiles {
		maxTiles = rows
	}
	zoom := 0
	for tiles := 1; tiles < maxTiles; tiles <<= 1 {
		zoom--
	}
	return zoom
}

func tileInZoomBounds(z, x, y int) bool {
	scale, err := zoomScale(z)
	if err != nil {
		return false
	}
	span := int64(timeshadedb.TileSize) * scale
	cols := ceilDivInt64(int64(timeshadedb.CanvasWidth), span)
	rows := ceilDivInt64(int64(timeshadedb.CanvasHeight), span)
	return x >= 0 && int64(x) < cols && y >= 0 && int64(y) < rows
}

func zoomScale(z int) (int64, error) {
	if z > 0 {
		return 0, fmt.Errorf("positive tile zoom %d has no native data", z)
	}
	shift := -z
	if shift >= 31 {
		return 0, fmt.Errorf("tile zoom %d is too small", z)
	}
	return int64(1) << uint(shift), nil
}

func floorDivInt64(v, d int64) int64 {
	q := v / d
	if v < 0 && v%d != 0 {
		q--
	}
	return q
}

func downsampledTileSpan(canvasSize int, worldStart, scale int64) int {
	remaining := int64(canvasSize) - worldStart
	if remaining <= 0 {
		return 0
	}
	span := ceilDivInt64(remaining, scale)
	if span > int64(timeshadedb.TileSize) {
		return timeshadedb.TileSize
	}
	return int(span)
}

func ceilDivInt64(n, d int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + d - 1) / d
}

func encodeTilePNG(w io.Writer, res *timeshadedb.TileResult, width, height int, level png.CompressionLevel) error {
	if len(res.Palette) > 255 {
		return fmt.Errorf("palette has %d colors, maximum PNG palette size is 255 plus transparency", len(res.Palette))
	}
	pal := make(color.Palette, 0, len(res.Palette)+1)
	pal = append(pal, color.RGBA{})
	for _, c := range res.Palette {
		pal = append(pal, color.RGBA{R: c.R, G: c.G, B: c.B, A: 255})
	}
	img := image.NewPaletted(image.Rect(0, 0, width, height), pal)
	for y := 0; y < res.Height; y++ {
		src := res.Pixels[y*res.Width : (y+1)*res.Width]
		for _, id := range src {
			if int(id) >= len(pal) {
				return fmt.Errorf("palette id %d out of range", id)
			}
		}
		copy(img.Pix[y*img.Stride:y*img.Stride+res.Width], src)
	}
	enc := png.Encoder{CompressionLevel: level}
	return enc.Encode(w, img)
}

func writePNGResponse(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
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
