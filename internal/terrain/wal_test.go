package terrain

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"

	"timeshadedb/internal/core"
	"timeshadedb/internal/core/coretest"
)

// TestWALLargeBatch is the regression test for a batch larger than one WAL frame: it
// used to be written as one frame that the decoder refused at replay, and the replay
// then truncated the segment, silently dropping acknowledged updates. Pixels are random,
// so the frames barely compress and are close to the frame size limit.
func TestWALLargeBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("writes about 70 MB")
	}
	ctx := context.Background()
	dir := t.TempDir()
	opt := Options{NoSync: true, OpenWindowBytes: -1}
	s, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(3))
	const n = 34_000
	ups := make([]core.ChunkUpdate, n)
	for i := range ups {
		px := &core.ChunkPixels{}
		for j := range px {
			px[j] = uint16(r.Intn(64)) // 64 colours: random, but a small palette
		}
		ups[i] = core.ChunkUpdate{DS: coretest.DS, Chunk: core.ChunkCoord{X: int32(i%200 - 100), Y: int32(i/200 - 85)}, T: 1000, Pixels: px}
	}
	if res, err := s.Apply(ctx, ups); err != nil || res.Accepted != n {
		t.Fatalf("apply: %+v %v", res, err)
	}
	small := core.ChunkUpdate{DS: coretest.DS, Chunk: core.ChunkCoord{X: 500, Y: 500}, T: 2000, Pixels: coretest.Fill(7)}
	if _, err := s.Apply(ctx, []core.ChunkUpdate{small}); err != nil {
		t.Fatal(err)
	}
	before := s.Stats().OpenWindowBytes
	if want := int64(n+1) * openChunkBytes; before != want {
		t.Fatalf("open bytes %d, want %d", before, want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var logs []string
	var mu sync.Mutex
	opt.Logf = func(f string, a ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	if s, err = Open(dir, opt); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if after := s.Stats().OpenWindowBytes; after != before {
		t.Fatalf("open bytes after reopen %d, before %d (logs %q)", after, before, logs)
	}
	if len(logs) != 0 {
		t.Fatalf("clean reopen logged %q", logs)
	}
	for _, i := range []int{0, 1023, 1024, 20_000, n - 1} {
		tc, b := ups[i].Chunk.Tile()
		im, st, err := s.ReadTile(ctx, coretest.DS, tc, 5000, true)
		if err != nil || !st.Live || im.Block(b) != *ups[i].Pixels {
			t.Fatalf("update %d after reopen: %v %+v", i, err, st)
		}
	}
	tc, b := small.Chunk.Tile()
	if im, _, err := s.ReadTile(ctx, coretest.DS, tc, 5000, true); err != nil || im.Block(b) != *small.Pixels {
		t.Fatalf("small batch after reopen: %v", err)
	}
}

// walSegments returns the WAL segment paths of the test datastore.
func walSegments(t *testing.T, dir string) []string {
	t.Helper()
	segs, err := filepath.Glob(filepath.Join(dir, "datastores", coretest.DS.ID(), "wal", "*.wal"))
	if err != nil {
		t.Fatal(err)
	}
	return segs
}

// writeOpenWindow creates a database with one open window in the WAL and returns the
// segment that holds it.
func writeOpenWindow(t *testing.T, dir string) string {
	t.Helper()
	s, err := Open(dir, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Apply(context.Background(), []core.ChunkUpdate{{DS: coretest.DS, Chunk: core.ChunkCoord{X: int32(i)}, T: 10, Pixels: coretest.Fill(uint16(i + 1))}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	segs := walSegments(t, dir)
	if len(segs) != 1 {
		t.Fatalf("segments %v", segs)
	}
	return segs[0]
}

// TestWALUndecodableFrame: a frame whose checksum is valid but whose payload does not
// decode is not a torn write. Open must fail and leave the segment untouched.
func TestWALUndecodableFrame(t *testing.T) {
	dir := t.TempDir()
	seg := writeOpenWindow(t, dir)
	payload := []byte("not a zstd frame at all")
	frame := make([]byte, walFrameHeader, walFrameHeader+len(payload))
	binary.LittleEndian.PutUint32(frame[0:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(frame[4:], crcC(payload))
	frame = append(frame, payload...)
	// The bad frame sits between good frames.
	data, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	first := walFrameHeader + int(binary.LittleEndian.Uint32(data))
	bad := append(append(append([]byte(nil), data[:first]...), frame...), data[first:]...)
	if err := os.WriteFile(seg, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, Options{NoSync: true})
	if err == nil {
		s.Close()
		t.Fatal("open succeeded over an undecodable frame")
	}
	if !strings.Contains(err.Error(), "does not decode") {
		t.Fatalf("error %v", err)
	}
	after, _ := os.ReadFile(seg)
	if !bytes.Equal(after, bad) {
		t.Fatal("segment was changed")
	}
}

// TestWALTornTailLogged: a torn tail is truncated, the frames before it are kept, and
// the truncation is logged.
func TestWALTornTailLogged(t *testing.T) {
	for _, tail := range [][]byte{
		{1, 2, 3},                            // incomplete header
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},    // zero-filled
		{200, 0, 0, 0, 1, 2, 3, 4, 5},        // incomplete frame
		{4, 0, 0, 0, 1, 2, 3, 4, 9, 9, 9, 9}, // checksum mismatch
	} {
		dir := t.TempDir()
		seg := writeOpenWindow(t, dir)
		f, err := os.OpenFile(seg, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(tail)
		f.Close()
		var logs []string
		s, err := Open(dir, Options{NoSync: true, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Stats().OpenWindowBytes; got != 3*openChunkBytes {
			t.Fatalf("tail %v: open bytes %d", tail, got)
		}
		s.Close()
		if len(logs) != 1 || !strings.Contains(logs[0], "truncating") {
			t.Fatalf("tail %v: logs %q", tail, logs)
		}
	}
}

// TestWALFrameLimit checks that the largest frame Apply writes decodes with the
// store's decoder limit.
func TestWALFrameLimit(t *testing.T) {
	if walMaxRaw > decoderMaxMemory {
		t.Fatalf("walMaxRaw %d over the decoder limit %d", walMaxRaw, decoderMaxMemory)
	}
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	defer enc.Close()
	dec, _ := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(decoderMaxMemory))
	defer dec.Close()
	r := rand.New(rand.NewSource(1))
	ups := make([]*core.ChunkUpdate, walFrameUpdates)
	for i := range ups {
		px := &core.ChunkPixels{}
		for j := range px {
			px[j] = uint16(r.Uint32())
		}
		ups[i] = &core.ChunkUpdate{T: core.TimeMs(r.Int63()), Chunk: core.ChunkCoord{X: -1 << 20, Y: 1 << 20}, Pixels: px}
	}
	frame, raw := appendWALFrame(nil, enc, ups, nil)
	if len(raw) > walMaxRaw || len(frame)-walFrameHeader > walMaxPayload {
		t.Fatalf("raw %d (limit %d), payload %d (limit %d)", len(raw), walMaxRaw, len(frame)-walFrameHeader, walMaxPayload)
	}
	got, _, err := decodeWALFrame(dec, frame[walFrameHeader:], nil, nil)
	if err != nil || len(got) != walFrameUpdates || got[5].px != *ups[5].Pixels {
		t.Fatalf("decode: %v (%d updates)", err, len(got))
	}
	if _, _, err := decodeWALFrame(dec, frame[walFrameHeader:len(frame)-1], nil, nil); err == nil {
		t.Fatal("truncated frame decoded")
	}
}
