// Command raildisk reports what the mover store of a v3 database holds on disk, for the
// real-map train benchmark (bench/railbench): per surface the closed window files and
// keyframe files as written (zstd), their rows by mover kind, the rows carrying carriage
// geometry (`car`), and the size the same windows would have after zstd without the
// carriage column and without non-train rows, so the share of `car` and of keyframes in
// the bytes per train-hour can be given; for rail path rows (core.FlagRail) also their
// count, their rail pieces and the size without the pieces; and every file of the surface's
// mover directory (windows, keyframes, day indexes, meta) as it lies on disk.
//
//	raildisk -db <v3 db> [-out disk.json]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/wire"
)

type surfaceDisk struct {
	Surface string `json:"surface"`
	// Window and keyframe files as stored (zstd frames), and their count.
	WindowBytes   int64 `json:"window_bytes"`
	Windows       int   `json:"windows"`
	KeyframeBytes int64 `json:"keyframe_bytes"`
	Keyframes     int   `json:"keyframes"`
	// Every file under the surface's mover directory, and the bytes of those that are
	// neither windows nor keyframes (day indexes, meta.json).
	DiskBytes  int64 `json:"disk_bytes"`
	IndexBytes int64 `json:"index_bytes"`
	// Rows of the closed windows.
	Rows      int `json:"rows"`
	TrainRows int `json:"train_rows"`
	CarRows   int `json:"car_rows"`
	Carriages int `json:"carriages"`
	// The windows re-encoded with the store's zstd level: only train rows; only train
	// rows without the carriage column.
	TrainOnlyBytes  int64 `json:"train_only_bytes"`
	TrainNoCarBytes int64 `json:"train_nocar_bytes"`
	// Rail path rows, their rail pieces, and the train-only windows without the pieces.
	RailRows         int   `json:"rail_rows"`
	RailPieces       int   `json:"rail_pieces"`
	TrainNoRailBytes int64 `json:"train_norail_bytes"`
	RawBodyBytes     int64 `json:"raw_body_bytes"`
	RawTrainNoCarRaw int64 `json:"raw_train_nocar_body_bytes"`
}

func main() {
	db := flag.String("db", "", "v3 database directory")
	out := flag.String("out", "", "JSON output (default stdout)")
	flag.Parse()
	if *db == "" {
		fmt.Fprintln(os.Stderr, "usage: raildisk -db <v3 db> [-out disk.json]")
		os.Exit(2)
	}
	// The server's store writes with Options.LightEncoder: zstd's better level, 4 MiB window.
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(true), zstd.WithWindowSize(1<<22))
	check(err)
	dec, err := zstd.NewReader(nil)
	check(err)
	root := filepath.Join(*db, "movers")
	ents, err := os.ReadDir(root)
	check(err)
	var res []surfaceDisk
	for _, e := range ents {
		if !e.IsDir() || e.Name() == "wal" {
			continue
		}
		sd := surfaceDisk{Surface: e.Name()}
		filepath.WalkDir(filepath.Join(root, e.Name()), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if info, err := d.Info(); err == nil {
					sd.DiskBytes += info.Size()
				}
			}
			return nil
		})
		for _, kind := range []string{"windows", "keyframes"} {
			var files []string
			filepath.WalkDir(filepath.Join(root, e.Name(), kind), func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(p, ".mvw") {
					files = append(files, p)
				}
				return nil
			})
			sort.Strings(files)
			for _, p := range files {
				z, err := os.ReadFile(p)
				check(err)
				if kind == "keyframes" {
					sd.KeyframeBytes += int64(len(z))
					sd.Keyframes++
					continue
				}
				sd.WindowBytes += int64(len(z))
				sd.Windows++
				b, err := dec.DecodeAll(z, nil)
				check(err)
				sd.RawBodyBytes += int64(len(b))
				w, err := wire.DecodeMoverWindow(b, core.SurfaceKey{})
				check(err)
				sd.Rows += len(w.Rows)
				trains := &core.MoverWindow{Surface: w.Surface, Kind: w.Kind, Start: w.Start, Len: w.Len}
				remap := map[int]int{}
				for _, r := range w.Rows {
					if w.Movers[r.Mover].Kind != core.KindTrain {
						continue
					}
					sd.TrainRows++
					if len(r.Carriages) > 0 {
						sd.CarRows++
						sd.Carriages += len(r.Carriages)
					}
					if r.Flags&core.FlagRail != 0 {
						sd.RailRows++
						sd.RailPieces += len(r.Rail)
					}
					mi, ok := remap[r.Mover]
					if !ok {
						mi = len(trains.Movers)
						remap[r.Mover] = mi
						trains.Movers = append(trains.Movers, w.Movers[r.Mover])
					}
					r.Mover = mi
					trains.Rows = append(trains.Rows, r)
				}
				body, err := wire.EncodeMoverWindow(trains)
				check(err)
				sd.TrainOnlyBytes += int64(len(enc.EncodeAll(body, nil)))
				pieces := make([][]core.RailPiece, len(trains.Rows))
				for i := range trains.Rows {
					pieces[i], trains.Rows[i].Rail = trains.Rows[i].Rail, nil
				}
				body, err = wire.EncodeMoverWindow(trains)
				check(err)
				sd.TrainNoRailBytes += int64(len(enc.EncodeAll(body, nil)))
				for i := range trains.Rows {
					trains.Rows[i].Rail = pieces[i]
					trains.Rows[i].Carriages = nil
				}
				body, err = wire.EncodeMoverWindow(trains)
				check(err)
				sd.RawTrainNoCarRaw += int64(len(body))
				sd.TrainNoCarBytes += int64(len(enc.EncodeAll(body, nil)))
			}
		}
		sd.IndexBytes = sd.DiskBytes - sd.WindowBytes - sd.KeyframeBytes
		res = append(res, sd)
	}
	b, err := json.MarshalIndent(res, "", "  ")
	check(err)
	if *out == "" {
		os.Stdout.Write(append(b, '\n'))
		return
	}
	check(os.WriteFile(*out, b, 0o644))
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "raildisk:", err)
		os.Exit(1)
	}
}
