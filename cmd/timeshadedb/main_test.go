package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
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
