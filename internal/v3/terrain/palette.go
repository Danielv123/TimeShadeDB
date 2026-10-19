package terrain

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

const paletteMagic = "TPL3"

// palette is a datastore's append-only colour table (FORMAT.md 5.2).
//
// Readers use snapshot(), which never blocks: entries are never changed, so an old
// slice stays valid. The LUT and appends are used only by the datastore's commit,
// which is serialized by dsState.commitMu.
type palette struct {
	path   string
	noSync bool

	mu      sync.Mutex
	f       *os.File
	lut     lut
	colors  []uint16 // owned by the writer
	pending int      // entries appended but not yet written
	snap    atomic.Pointer[[]uint16]
}

func openPalette(path string, noSync bool) (*palette, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	p := &palette{path: path, noSync: noSync, f: f, lut: newLUT()}
	switch {
	case len(raw) < 8:
		// New (or torn before the header was complete): write the header and entry 0.
		hdr := make([]byte, 10)
		copy(hdr, paletteMagic)
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := f.WriteAt(hdr, 0); err != nil {
			f.Close()
			return nil, err
		}
		if !noSync {
			f.Sync()
		}
		raw = hdr
	case string(raw[:4]) != paletteMagic:
		f.Close()
		return nil, fmt.Errorf("terrain: %s is not a palette file", path)
	}
	body := raw[8:]
	if len(body)%2 == 1 {
		// Torn write of the last entry.
		body = body[:len(body)-1]
		if err := f.Truncate(int64(8 + len(body))); err != nil {
			f.Close()
			return nil, err
		}
	}
	if len(body) < 2 {
		body = []byte{0, 0}
		if _, err := f.WriteAt(body, 8); err != nil {
			f.Close()
			return nil, err
		}
	}
	n := len(body) / 2
	p.colors = make([]uint16, n, max(n, 64))
	for i := range p.colors {
		c := binary.LittleEndian.Uint16(body[2*i:])
		p.colors[i] = c
		if p.lut[c] == 0 {
			p.lut[c] = uint32(i + 1)
		}
	}
	if p.colors[0] != 0 {
		f.Close()
		return nil, fmt.Errorf("terrain: %s: entry 0 is %#04x, not 0", path, p.colors[0])
	}
	p.publish()
	return p, nil
}

func (p *palette) publish() {
	s := p.colors[:len(p.colors):len(p.colors)]
	p.snap.Store(&s)
}

// snapshot returns the current palette. The slice must not be modified.
func (p *palette) snapshot() []uint16 { return *p.snap.Load() }

// add puts every colour of px in the palette (in memory). It reports an error when the
// palette is full. Caller holds the commit lock.
func (p *palette) add(px []uint16) error {
	for _, c := range px {
		if p.lut[c] != 0 {
			continue
		}
		if len(p.colors) >= 1<<16 {
			return fmt.Errorf("terrain: palette full")
		}
		p.colors = append(p.colors, c)
		p.lut[c] = uint32(len(p.colors))
		p.pending++
	}
	return nil
}

// flush appends the new entries to palette.bin and fsyncs, then publishes them. It
// must run before any block that uses them is written.
func (p *palette) flush() error {
	if p.pending == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.colors)
	first := n - p.pending
	buf := make([]byte, 2*p.pending)
	for i, c := range p.colors[first:] {
		binary.LittleEndian.PutUint16(buf[2*i:], c)
	}
	if _, err := p.f.WriteAt(buf, int64(8+2*first)); err != nil {
		return err
	}
	if !p.noSync {
		if err := p.f.Sync(); err != nil {
			return err
		}
	}
	p.pending = 0
	p.publish()
	return nil
}

func (p *palette) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f == nil {
		return nil
	}
	err := p.f.Close()
	p.f = nil
	return err
}
