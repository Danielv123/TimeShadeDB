package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"

	"timeshadedb"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	return runCommand(ctx, os.Stdout, args)
}

func statsValue(db *timeshadedb.DB) (any, error) {
	if db.Format() == timeshadedb.FormatChunks {
		return db.ChunkStats()
	}
	return db.Stats(), nil
}

func parsePair(s string) (int, int, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected x,y")
	}
	x, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	y, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	return x, y, nil
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writePNG(path string, res *timeshadedb.TileResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return encodeTilePNG(f, res, res.Width, res.Height, png.DefaultCompression)
}

func startPprof(addr string) error {
	if addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		_ = http.Serve(ln, nil)
	}()
	return nil
}

type profileSession struct {
	cpuFile *os.File
	memPath string
}

func startProfiles(cpuPath, memPath string) (*profileSession, error) {
	s := &profileSession{memPath: memPath}
	if cpuPath == "" {
		return s, nil
	}
	f, err := os.Create(cpuPath)
	if err != nil {
		return nil, err
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	s.cpuFile = f
	return s, nil
}

func (s *profileSession) Stop() error {
	var errs []error
	if s.cpuFile != nil {
		pprof.StopCPUProfile()
		if err := s.cpuFile.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.memPath != "" {
		runtime.GC()
		f, err := os.Create(s.memPath)
		if err != nil {
			errs = append(errs, err)
		} else {
			if err := pprof.WriteHeapProfile(f); err != nil {
				errs = append(errs, err)
			}
			if err := f.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
