package movers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/v3/core"
)

// FormatVersion is the movers meta.json format_version this package reads and writes.
const FormatVersion = 1

var (
	encOnce sync.Once
	enc     *zstd.Encoder
	dec     *zstd.Decoder
	codecErr error
)

// codec returns the shared zstd encoder and decoder. EncodeAll and DecodeAll are safe
// for concurrent use. The window is capped at 4 MiB so browsers (which must support
// 8 MB windows for Content-Encoding: zstd) can decode every file sent unchanged.
func codec() (*zstd.Encoder, *zstd.Decoder, error) {
	encOnce.Do(func() {
		enc, codecErr = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedBestCompression),
			zstd.WithEncoderConcurrency(1),
			zstd.WithLowerEncoderMem(true),
			zstd.WithWindowSize(1<<22),
		)
		if codecErr != nil {
			return
		}
		dec, codecErr = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(256<<20))
	})
	return enc, dec, codecErr
}

func timeName(t core.TimeMs) string { return strconv.FormatInt(int64(t), 10) }

func (s *surface) windowPath(start core.TimeMs) string {
	return filepath.Join(s.dir, "windows", core.UTCDay(start), timeName(start)+".mvw")
}

func (s *surface) keyframePath(t core.TimeMs) string {
	return filepath.Join(s.dir, "keyframes", core.UTCDay(t), timeName(t)+".mvw")
}

func (s *surface) dayPath(date string) string {
	return filepath.Join(s.dir, "days", date+".json")
}

// writeFileAtomic writes data to path through a temporary file and a rename, creating
// the directory. With sync set the file is fsynced before the rename.
func writeFileAtomic(path string, data []byte, sync bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, data, sync); err != nil {
		return err
	}
	return renameRetry(tmp, path)
}

// writeFileSync creates or truncates path and writes data, fsyncing when sync is set.
// On error the file is removed.
func writeFileSync(path string, data []byte, sync bool) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil && sync {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// renameRetry renames, retrying briefly on Windows, where a rename over a file that a
// reader has open fails until the reader closes it.
func renameRetry(from, to string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.Rename(from, to); err == nil || runtime.GOOS != "windows" {
			return err
		}
		time.Sleep(time.Duration(i+1) * 5 * time.Millisecond)
	}
	return err
}

// encodeWindowFile encodes w as a .mvw file: one zstd frame holding the MVW3 body.
func encodeWindowFile(body []byte) ([]byte, error) {
	e, _, err := codec()
	if err != nil {
		return nil, err
	}
	return e.EncodeAll(body, make([]byte, 0, len(body)/4+64)), nil
}

// readWindowFile reads a .mvw file and returns its compressed bytes, or ErrNoVersion
// when it does not exist.
func readWindowFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", core.ErrNoVersion, filepath.Base(path))
	}
	return b, err
}

// decodeWindowFile decompresses a .mvw file to its MVW3 body.
func decodeWindowFile(b []byte) ([]byte, error) {
	_, d, err := codec()
	if err != nil {
		return nil, err
	}
	return d.DecodeAll(b, nil)
}
