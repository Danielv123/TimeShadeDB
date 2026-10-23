package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"timeshadedb/internal/core"
	"timeshadedb/internal/import/minimap/minimaptest"
)

func TestPublishFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--dir", "x"},
		{"--db", "a", "--from", "http://h/", "--dir", "x"},
		{"--db", "a"},
		{"--db", "a", "--dir", "x", "--s3-endpoint", "http://h"},
		{"--db", "a", "--s3-endpoint", "http://h"},
		{"--from", "http://h/", "--s3-endpoint", "http://h", "--s3-bucket", "b"},
		{"--db", "a", "--dir", "x", "--every", "1m"},
		{"--from", "http://h/", "--dir", "x", "--every", "10ms"},
		{"--db", "a", "--dir", "x", "--keyframe-every", "0"},
		{"--db", "a", "--dir", "x", "extra"},
	} {
		if _, err := parsePublish(io.Discard, args); err == nil {
			t.Errorf("parsePublish(%q) accepted", args)
		}
	}
	o, err := parsePublish(io.Discard, []string{"--from", "http://h/api/v3/", "--dir", "x", "--every", "30s"})
	if err != nil || o.liveMs != 30000 {
		t.Fatalf("%+v %v", o, err)
	}
}

// readExport reads a file of a gzip export.
func readExport(t *testing.T, out, key string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(key)))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if b, err = io.ReadAll(r); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

var statsLine = regexp.MustCompile(`publish: (\d+) objects \((\d+) resumed\): (\d+) stored \((\d+) immutable`)

func publishStats(t *testing.T, out string) (planned, stored, immutable int) {
	t.Helper()
	m := statsLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no stats line in %q", out)
	}
	planned, _ = strconv.Atoi(m[1])
	stored, _ = strconv.Atoi(m[3])
	immutable, _ = strconv.Atoi(m[4])
	return
}

// exportKeys lists the files of an export (without the publish state).
func exportKeys(t *testing.T, out string) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	err := filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".tsdb-publish" {
			return fs.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(out, p)
			keys[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// TestPublishDBToDir exports an imported database into a directory and checks that the
// export holds everything the static viewer reads: the static catalog, the palette,
// every timeline day, every tile version the timelines name, the consecutive deltas, the
// mover days, windows and keyframes, and the viewer page. A second run stores no
// immutable object. Publishing from the running server gives the same immutable files.
func TestPublishDBToDir(t *testing.T) {
	exp := minimaptest.Write(t, t.TempDir())
	db := filepath.Join(t.TempDir(), "db")
	importIntoDB(t, exp.Root, db)
	web := t.TempDir()
	os.MkdirAll(filepath.Join(web, "assets"), 0o755)
	os.WriteFile(filepath.Join(web, "index.html"), []byte(`<head><meta name="tsdb-source" content="" /><script src="/assets/a-1.js"></script></head>`), 0o644)
	os.WriteFile(filepath.Join(web, "assets", "a-1.js"), []byte(strings.Repeat("let a = 1;\n", 100)), 0o644)

	out := filepath.Join(t.TempDir(), "out")
	var buf bytes.Buffer
	if err := runPublish(context.Background(), &buf, io.Discard, []string{"--db", db, "--dir", out, "--web-dir", web}); err != nil {
		t.Fatalf("publish: %v\n%s", err, buf.String())
	}
	planned, stored, immutable := publishStats(t, buf.String())
	if planned == 0 || stored != planned || immutable == 0 {
		t.Fatalf("first run: %s", buf.String())
	}
	keys := exportKeys(t, out)

	var cat core.Catalog
	if err := json.Unmarshal(readExport(t, out, "catalog.json"), &cat); err != nil || !cat.Static || len(cat.Maps) == 0 {
		t.Fatalf("catalog: %+v %v", cat, err)
	}
	if !strings.Contains(string(readExport(t, out, "index.html")), `content="static"`) || !keys["assets/a-1.js"] {
		t.Fatal("viewer files")
	}
	tiles, timelines := 0, 0
	for _, m := range cat.Maps {
		for _, s := range m.Surfaces {
			for _, d := range s.Datastores {
				if !keys["palette/"+d.ID+"/"+strconv.Itoa(int(d.PaletteVersion))+".bin"] {
					t.Errorf("%s: palette %d missing", d.ID, d.PaletteVersion)
				}
				for _, z := range d.Levels {
					zs := strconv.Itoa(int(z))
					for t0 := d.First - d.First%core.Day; t0 <= d.Last; t0 += core.Day {
						k := "timeline/" + d.ID + "/" + zs + "/" + core.UTCDay(t0) + ".json"
						if !keys[k] {
							t.Errorf("%s missing", k)
							continue
						}
						timelines++
						var tl core.TimelineResponse
						if err := json.Unmarshal(readExport(t, out, k), &tl); err != nil {
							t.Fatal(err)
						}
						for _, tt := range tl.Tiles {
							vs := []int64{int64(tt.V0)}
							for _, p := range tt.Points {
								vs = append(vs, p[1])
							}
							prefix := "tile/" + d.ID + "/" + zs + "/" + strconv.Itoa(int(tt.X)) + "/" + strconv.Itoa(int(tt.Y)) + "/"
							for _, v := range vs {
								if !keys[prefix+strconv.FormatInt(v, 10)+".bin"] {
									t.Errorf("%s%d.bin missing", prefix, v)
								}
								tiles++
								if z == 0 && v > 1 {
									dk := "tiledelta/" + d.ID + "/" + strconv.Itoa(int(tt.X)) + "/" + strconv.Itoa(int(tt.Y)) + "/" + strconv.FormatInt(v-1, 10) + "-" + strconv.FormatInt(v, 10) + ".bin"
									if !keys[dk] {
										t.Errorf("%s missing", dk)
									}
								}
							}
						}
					}
				}
			}
			if s.Movers != nil {
				for _, day := range s.Movers.Days {
					k := "movers/" + s.ID + "/day/" + day + ".json"
					var di core.DayIndex
					if err := json.Unmarshal(readExport(t, out, k), &di); err != nil {
						t.Fatalf("%s: %v", k, err)
					}
					for _, w := range di.Windows {
						if !keys["movers/"+s.ID+"/window/"+strconv.FormatInt(int64(w.Start), 10)+".bin"] {
							t.Errorf("window %d of %s missing", w.Start, s.ID)
						}
					}
					for _, kt := range di.Keyframes {
						if !keys["movers/"+s.ID+"/keyframe/"+strconv.FormatInt(int64(kt), 10)+".bin"] {
							t.Errorf("keyframe %d of %s missing", kt, s.ID)
						}
					}
				}
				if !keys["movers/"+s.ID+"/live.bin"] {
					t.Errorf("live.bin of %s missing", s.ID)
				}
			}
		}
	}
	if tiles == 0 || timelines == 0 {
		t.Fatalf("checked %d tiles, %d timelines", tiles, timelines)
	}
	// The tile bodies are the API's TSV3 bodies.
	for k := range keys {
		if strings.HasPrefix(k, "tile/") {
			if b := readExport(t, out, k); len(b) != 32+512*512 && len(b) != 32+2*512*512 || string(b[:4]) != "TSV3" {
				t.Fatalf("%s: %d bytes %q", k, len(b), b[:4])
			}
			break
		}
	}

	// Again: no immutable object is stored a second time.
	buf.Reset()
	if err := runPublish(context.Background(), &buf, io.Discard, []string{"--db", db, "--dir", out, "--web-dir", web}); err != nil {
		t.Fatalf("second publish: %v\n%s", err, buf.String())
	}
	if _, _, immutable := publishStats(t, buf.String()); immutable != 0 {
		t.Fatalf("second run stored immutable objects: %s", buf.String())
	}

	// From the running server: the same immutable files.
	base, stop := startServe(t, "--db", db)
	out2 := filepath.Join(t.TempDir(), "out2")
	buf.Reset()
	if err := runPublish(context.Background(), &buf, io.Discard, []string{"--from", base + "/api/v3/", "--dir", out2, "--web-dir", web}); err != nil {
		t.Fatalf("publish --from: %v\n%s", err, buf.String())
	}
	stop()
	keys2 := exportKeys(t, out2)
	for k := range keys {
		if (strings.HasPrefix(k, "tile/") || strings.HasPrefix(k, "tiledelta/") || strings.Contains(k, "/window/")) && !keys2[k] {
			t.Errorf("--from export lacks %s", k)
		}
		if strings.HasPrefix(k, "tile/") && !bytes.Equal(readExport(t, out, k), readExport(t, out2, k)) {
			t.Errorf("%s differs between --db and --from", k)
		}
	}
}

// TestPublishDBToS3 publishes to a fake S3 endpoint with credentials from the
// environment, twice: the second run uploads no immutable object.
func TestPublishDBToS3(t *testing.T) {
	exp := minimaptest.Write(t, t.TempDir())
	db := filepath.Join(t.TempDir(), "db")
	importIntoDB(t, exp.Root, db)
	var mu sync.Mutex
	puts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Method != http.MethodPut || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDTEST/") || !strings.HasPrefix(r.URL.Path, "/map/pub/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mu.Lock()
		puts[strings.TrimPrefix(r.URL.Path, "/map/pub/")]++
		mu.Unlock()
	}))
	defer srv.Close()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	args := []string{"--db", db, "--s3-endpoint", srv.URL, "--s3-bucket", "map", "--s3-prefix", "pub", "--no-viewer"}
	var buf bytes.Buffer
	if err := runPublish(context.Background(), &buf, io.Discard, args); err != nil {
		t.Fatalf("publish: %v\n%s", err, buf.String())
	}
	planned, stored, _ := publishStats(t, buf.String())
	if stored != planned || puts["catalog.json"] != 1 {
		t.Fatalf("first run: %s %v", buf.String(), puts["catalog.json"])
	}
	if _, err := os.Stat(filepath.Join(db, "publish")); err != nil {
		t.Fatalf("state not under the database: %v", err)
	}
	buf.Reset()
	if err := runPublish(context.Background(), &buf, io.Discard, args); err != nil {
		t.Fatalf("second publish: %v\n%s", err, buf.String())
	}
	if _, _, immutable := publishStats(t, buf.String()); immutable != 0 {
		t.Fatalf("second run: %s", buf.String())
	}
	for k, n := range puts {
		if (strings.HasPrefix(k, "tile/") || strings.HasPrefix(k, "tiledelta/") || strings.HasPrefix(k, "palette/")) && n != 1 {
			t.Errorf("%s uploaded %d times", k, n)
		}
	}
	// Wrong credentials: the command fails and says so.
	t.Setenv("AWS_ACCESS_KEY_ID", "OTHER")
	buf.Reset()
	err := runPublish(context.Background(), &buf, io.Discard, append(args, "--reset"))
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("bad credentials: %v", err)
	}
}
