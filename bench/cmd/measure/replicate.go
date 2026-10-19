package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"timeshadedb/bench/internal/benchlib"
	"timeshadedb/internal/v3/core"
)

// cmdReplicate writes a database holding n copies of a v3 database, copy k > 0 under map
// keys suffixed "-r<k>": n times the datastores, tiles, history and bytes of the source,
// for the "startup on a 10x database" target (the startup time is O(tiles) by design, so
// a larger history alone does not test it).
//
//	measure replicate -db <v3 db> -out <new db> [-n 10]
func cmdReplicate(args []string) {
	fs := flag.NewFlagSet("replicate", flag.ExitOnError)
	src := fs.String("db", "", "source v3 database")
	dst := fs.String("out", "", "new database directory (must not exist)")
	n := fs.Int("n", 10, "copies")
	fs.Parse(args)
	if *src == "" || *dst == "" || *n < 1 {
		benchlib.Fatal(errors.New("measure replicate: -db, -out and -n >= 1 are required"))
	}
	if _, err := os.Stat(*dst); err == nil {
		benchlib.Fatal(fmt.Errorf("measure replicate: %s exists", *dst))
	}
	if err := replicate(*src, *dst, *n); err != nil {
		benchlib.Fatal(err)
	}
}

func replicate(src, dst string, n int) error {
	// Top-level files (format marker, maps.json, icons) once.
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range ents {
		switch e.Name() {
		case "datastores", "movers", "overlays":
			continue
		}
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	for k := 0; k < n; k++ {
		suffix := ""
		if k > 0 {
			suffix = "-r" + strconv.Itoa(k)
		}
		for _, store := range []string{"datastores", "overlays", "movers"} {
			dir := filepath.Join(src, store)
			ents, err := os.ReadDir(dir)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			for _, e := range ents {
				if !e.IsDir() {
					continue
				}
				name := e.Name()
				newName, ok := renameKeyed(store, name, suffix)
				if !ok {
					if k == 0 {
						// Not a keyed directory (the movers WAL): copied once.
						if err := copyTree(filepath.Join(dir, name), filepath.Join(dst, store, name)); err != nil {
							return err
						}
					}
					continue
				}
				to := filepath.Join(dst, store, newName)
				if err := copyTree(filepath.Join(dir, name), to); err != nil {
					return err
				}
				if suffix != "" {
					if err := suffixMetaKey(filepath.Join(to, "meta.json"), suffix); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// renameKeyed returns the directory name of a datastore or surface directory under a map
// key with suffix, and whether name is such a directory.
func renameKeyed(store, name, suffix string) (string, bool) {
	if store == "movers" {
		key, err := core.ParseSurfaceID(name)
		if err != nil {
			return "", false
		}
		key.Map += suffix
		return key.ID(), true
	}
	key, err := core.ParseDatastoreID(name)
	if err != nil {
		return "", false
	}
	key.Map += suffix
	return key.ID(), true
}

// suffixMetaKey appends suffix to the "map" of the "key" object in a meta.json.
func suffixMetaKey(path string, suffix string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if key, ok := m["key"].(map[string]any); ok {
		if mp, ok := key["map"].(string); ok {
			key["map"] = mp + suffix
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// cmdStartup starts a server on a database several times and records the median time
// until it answers (startup.ms), and the database size.
//
//	measure startup -server-bin <timeshadedb> -db <v3 db> [-restarts 3] -label v3 -scenario synth10x -out r.json
func cmdStartup(args []string) {
	fs := flag.NewFlagSet("startup", flag.ExitOnError)
	var c common
	c.register(fs, "v3")
	bin := fs.String("server-bin", "", "timeshadedb binary")
	db := fs.String("db", "", "database")
	restarts := fs.Int("restarts", 3, "server starts")
	fs.Parse(args)
	if *bin == "" || *db == "" {
		benchlib.Fatal(errors.New("measure startup: -server-bin and -db are required"))
	}
	res := c.result("measure-startup")
	total, _, err := benchlib.DirSize(*db)
	if err != nil {
		benchlib.Fatal(err)
	}
	res.Set("startup.db_mib", float64(total)/(1<<20))
	var ms []float64
	for i := 0; i < *restarts; i++ {
		s, err := benchlib.StartServer(benchlib.ServerSpec{Binary: *bin, Args: []string{"serve-v3", "--db", "{db}", "--listen", "{listen}"}, DB: *db, Timeout: 2 * time.Minute})
		if err != nil {
			benchlib.Fatal(err)
		}
		ms = append(ms, benchlib.Ms(s.Startup))
		if err := s.Stop(); err != nil {
			res.Note("stop: %v", err)
		}
	}
	res.Set("startup.ms", benchlib.Median(ms))
	res.Param("restarts", *restarts)
	if err := res.Write(c.out); err != nil {
		benchlib.Fatal(err)
	}
}
