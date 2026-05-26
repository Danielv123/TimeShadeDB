package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"timeshadedb"
)

func TestCLISmoke(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "sample.csv.gzip")
	dbPath := filepath.Join(dir, "db.tshd")
	outPath := filepath.Join(dir, "tile.raw")
	if err := writeSample(input); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"import-csv", "--input", input, "--db", dbPath}); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"stats", "--db", dbPath}); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"query-tile", "--db", dbPath, "--timestamp", "2022-04-01T12:02:00Z", "--tile", "0,0", "--out", outPath}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Size(), int64(512*512); got != want {
		t.Fatalf("raw tile size = %d, want %d", got, want)
	}
	if err := run(context.Background(), []string{"verify", "--input", input, "--db", dbPath}); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"inspect-index", "--db", dbPath, "--tile", "0,0"}); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"benchmark", "--db", dbPath, "--queries", "1", "--seed", "7"}); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"compact", "--db", dbPath}); err != nil {
		t.Fatal(err)
	}
	pngPath := filepath.Join(dir, "tile.png")
	if err := run(context.Background(), []string{"export-tile", "--db", dbPath, "--timestamp", "2022-04-01T12:02:00Z", "--tile", "0,0", "--format", "png", "--out", pngPath}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(pngPath); err != nil {
		t.Fatal(err)
	} else if info.Size() == 0 {
		t.Fatal("exported PNG is empty")
	}
}

func TestWebAPI(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "sample.csv.gzip")
	dbPath := filepath.Join(dir, "db.tshd")
	if err := writeSample(input); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"import-csv", "--input", input, "--db", dbPath}); err != nil {
		t.Fatal(err)
	}
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	handler := newWebServer(db, http.NotFoundHandler()).routes()
	metaReq := httptest.NewRequest(http.MethodGet, "/api/meta", nil)
	metaResp := httptest.NewRecorder()
	handler.ServeHTTP(metaResp, metaReq)
	if metaResp.Code != http.StatusOK {
		t.Fatalf("metadata status = %d, body %s", metaResp.Code, metaResp.Body.String())
	}
	var meta metadataResponse
	if err := json.Unmarshal(metaResp.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if got, want := meta.CanvasWidth, 2000; got != want {
		t.Fatalf("canvas width = %d, want %d", got, want)
	}
	if got, want := meta.MinZoom, -2; got != want {
		t.Fatalf("min zoom = %d, want %d", got, want)
	}
	if meta.FromSec == 0 || meta.ToSec == 0 || meta.FromSec > meta.ToSec {
		t.Fatalf("unexpected time range: %d..%d", meta.FromSec, meta.ToSec)
	}

	tileReq := httptest.NewRequest(http.MethodGet, "/api/tiles/0/0/0.png?ts=1648814520", nil)
	tileResp := httptest.NewRecorder()
	handler.ServeHTTP(tileResp, tileReq)
	if tileResp.Code != http.StatusOK {
		t.Fatalf("tile status = %d, body %s", tileResp.Code, tileResp.Body.String())
	}
	img, err := png.Decode(tileResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := img.Bounds().Dx(), timeshadedb.TileSize; got != want {
		t.Fatalf("tile png width = %d, want %d", got, want)
	}
	r, g, b, a := img.At(1, 1).RGBA()
	if r>>8 != 4 || g>>8 != 5 || b>>8 != 6 || a>>8 != 255 {
		t.Fatalf("pixel = rgba(%d,%d,%d,%d), want rgba(4,5,6,255)", r>>8, g>>8, b>>8, a>>8)
	}

	downsampledReq := httptest.NewRequest(http.MethodGet, "/api/tiles/-1/0/0.png?ts=1648814520", nil)
	downsampledResp := httptest.NewRecorder()
	handler.ServeHTTP(downsampledResp, downsampledReq)
	if downsampledResp.Code != http.StatusOK {
		t.Fatalf("downsampled tile status = %d, body %s", downsampledResp.Code, downsampledResp.Body.String())
	}
	downsampledImg, err := png.Decode(downsampledResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := downsampledImg.Bounds().Dx(), timeshadedb.TileSize; got != want {
		t.Fatalf("downsampled tile png width = %d, want %d", got, want)
	}
	r, g, b, a = downsampledImg.At(0, 0).RGBA()
	if r>>8 != 4 || g>>8 != 5 || b>>8 != 6 || a>>8 != 255 {
		t.Fatalf("downsampled pixel = rgba(%d,%d,%d,%d), want rgba(4,5,6,255)", r>>8, g>>8, b>>8, a>>8)
	}
}

func TestWebChunkIngestAPI(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.tshd")
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	handler := newWebServer(db, http.NotFoundHandler()).routes()
	saveID := "save-1"
	firstPayload := testRGB565Hex(0x2462)
	body := "10\tnauvis\t-7,-6\tplayer\t" + firstPayload + "\n"
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/chunk/"+saveID, strings.NewReader(body))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("chunk ingest status = %d, body %s", resp.Code, resp.Body.String())
	}
	var result timeshadedb.IngestChunkResult
	if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.AcceptedRows != 1 || result.ChangedPixels != timeshadedb.ChunkPixelCount || result.DatastoresTouched != 1 {
		t.Fatalf("chunk ingest result = %+v", result)
	}

	dupReq := httptest.NewRequest(http.MethodPost, "/api/ingest/chunk/"+saveID, strings.NewReader(body))
	dupResp := httptest.NewRecorder()
	handler.ServeHTTP(dupResp, dupReq)
	if dupResp.Code != http.StatusOK {
		t.Fatalf("duplicate ingest status = %d, body %s", dupResp.Code, dupResp.Body.String())
	}
	var duplicate timeshadedb.IngestChunkResult
	if err := json.Unmarshal(dupResp.Body.Bytes(), &duplicate); err != nil {
		t.Fatal(err)
	}
	if duplicate.ChangedPixels != 0 || duplicate.UnchangedPixels != timeshadedb.ChunkPixelCount {
		t.Fatalf("duplicate result = %+v", duplicate)
	}

	metaReq := httptest.NewRequest(http.MethodGet, "/api/ingest/chunk/"+saveID+"/meta", nil)
	metaResp := httptest.NewRecorder()
	handler.ServeHTTP(metaResp, metaReq)
	if metaResp.Code != http.StatusOK {
		t.Fatalf("chunk metadata status = %d, body %s", metaResp.Code, metaResp.Body.String())
	}
	var meta timeshadedb.IngestMetadata
	if err := json.Unmarshal(metaResp.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Datastores) != 1 || meta.Datastores[0].Surface != "nauvis" || meta.Datastores[0].LatestTick != 10 {
		t.Fatalf("chunk metadata = %+v", meta)
	}

	chunk, err := db.ChunkAt(context.Background(), timeshadedb.ChunkAtOptions{
		Key:   timeshadedb.DatastoreKey{SavefileUUID: saveID, Surface: "nauvis", Force: "player"},
		Tick:  10,
		Chunk: timeshadedb.ChunkCoord{X: -7, Y: -6},
	})
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Pixels[0] != 0x2462 || chunk.Pixels[timeshadedb.ChunkPixelCount-1] != 0x2462 {
		t.Fatalf("chunk pixels were not ingested")
	}
}

func TestTailChunkTSVOnceResumesFromServerMetadata(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.tshd")
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	handler := newWebServer(db, http.NotFoundHandler()).routes()
	server := httptest.NewServer(handler)
	defer server.Close()

	saveID := "save-1"
	firstRow := "10\tnauvis\t-7,-6\tplayer\t" + testRGB565Hex(0x2462)
	secondRow := "11\tnauvis\t-7,-6\tplayer\t" + testRGB565Hex(0xcdac)

	req := httptest.NewRequest(http.MethodPost, "/api/ingest/chunk/"+saveID, strings.NewReader(firstRow+"\n"))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("pre-ingest status = %d, body %s", resp.Code, resp.Body.String())
	}

	input := filepath.Join(dir, "chunk-charted.tsv")
	if err := os.WriteFile(input, []byte(firstRow+"\n"+secondRow+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{
		"tail-chunk-tsv",
		"--input", input,
		"--savefile", saveID,
		"--base-url", server.URL,
		"--once",
	}); err != nil {
		t.Fatal(err)
	}

	chunk, err := db.ChunkAt(context.Background(), timeshadedb.ChunkAtOptions{
		Key:   timeshadedb.DatastoreKey{SavefileUUID: saveID, Surface: "nauvis", Force: "player"},
		Tick:  11,
		Chunk: timeshadedb.ChunkCoord{X: -7, Y: -6},
	})
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Pixels[0] != 0xcdac {
		t.Fatalf("tail sender did not resume and send second row, pixel = %#04x", chunk.Pixels[0])
	}
}

func TestStaticWebAppServesEmbeddedIndex(t *testing.T) {
	fsys, err := timeshadedb.WebDistFS()
	if err != nil {
		t.Fatal(err)
	}
	handler := newWebServer(nil, newSPAFileServer(fsys)).routes()

	for _, target := range []string{"/", "/timeline/1648814520"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body %s", target, resp.Code, resp.Body.String())
		}
		if !bytes.Contains(resp.Body.Bytes(), []byte(`<div id="root"></div>`)) {
			t.Fatalf("%s did not serve embedded index.html", target)
		}
	}
}

func TestStaticWebAppServesExternalAssets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("index"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log('ok');"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := newWebServer(nil, newSPAFileServer(os.DirFS(dir))).routes()

	assetReq := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	assetResp := httptest.NewRecorder()
	handler.ServeHTTP(assetResp, assetReq)
	if assetResp.Code != http.StatusOK {
		t.Fatalf("asset status = %d, body %s", assetResp.Code, assetResp.Body.String())
	}
	if got, want := assetResp.Body.String(), "console.log('ok');"; got != want {
		t.Fatalf("asset body = %q, want %q", got, want)
	}

	fallbackReq := httptest.NewRequest(http.MethodGet, "/missing/route", nil)
	fallbackResp := httptest.NewRecorder()
	handler.ServeHTTP(fallbackResp, fallbackReq)
	if fallbackResp.Code != http.StatusOK {
		t.Fatalf("fallback status = %d, body %s", fallbackResp.Code, fallbackResp.Body.String())
	}
	if got, want := fallbackResp.Body.String(), "index"; got != want {
		t.Fatalf("fallback body = %q, want %q", got, want)
	}
}

func BenchmarkTileEndpointFullDB(b *testing.B) {
	dbPath := filepath.Join("..", "..", "full.tshd")
	if _, err := os.Stat(dbPath); err != nil {
		b.Skipf("full test database not found: %v", err)
	}
	db, err := timeshadedb.Open(timeshadedb.OpenOptions{Path: dbPath, ReadOnly: true, CacheSize: 64 * 1024 * 1024})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	handler := newWebServer(db, http.NotFoundHandler()).routes()
	req := httptest.NewRequest(http.MethodGet, "/api/tiles/0/0/1.png?ts=1649016052", nil)
	b.ReportAllocs()
	b.ResetTimer()
	var responseBytes int
	for i := 0; i < b.N; i++ {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			b.Fatalf("tile status = %d, body %s", resp.Code, resp.Body.String())
		}
		responseBytes = resp.Body.Len()
	}
	b.ReportMetric(float64(responseBytes), "bytes/tile")
}

func writeSample(path string) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte("timestamp,user_id,pixel_color,coordinate\n" +
		"2022-04-01 12:00:00.123 UTC,u1,#010203,\"1,1\"\n" +
		"2022-04-01 12:01:00.456 UTC,u2,#040506,\"1,1\"\n")); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func testRGB565Hex(color uint16) string {
	return strings.Repeat(fmt.Sprintf("%04x", color), timeshadedb.ChunkPixelCount)
}
