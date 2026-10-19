package terrain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

const paletteMagic = "TPL3"

var errClosedPalette = errors.New("terrain: palette closed")

// palette is a datastore's append-only colour table (FORMAT.md 5.2).
//
// Colours are added by Apply, durably (appended and fsynced) before the update that
// uses them becomes visible, so a live tile can always be encoded with the palette and
// a published palette entry never changes, even after a crash. New colours are rare
// after the first minutes of a map, so the fsync is rare too.
//
// Readers use snapshot(), which never blocks: entries are never changed, so an old
// slice stays valid. The presence bitmap is read and written only under the
// datastore's lock (the Apply path); the LUT is written under mu and read by commits
// under mu.RLock.
type palette struct {
	path   string
	noSync bool

	present [1 << 10]uint64 // bit c set when colour c is in the palette

	mu     sync.RWMutex
	f      *os.File
	lut    lut
	colors []uint16
	snap   atomic.Pointer[[]uint16]
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
		p.present[c>>6] |= 1 << (c & 63)
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

// ensure makes every colour of px part of the palette, appending and fsyncing new
// ones. Caller holds the datastore lock.
func (p *palette) ensure(px []uint16) error {
	missing := false
	for _, c := range px {
		if p.present[c>>6]&(1<<(c&63)) == 0 {
			missing = true
			break
		}
	}
	if !missing {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f == nil {
		return errClosedPalette
	}
	first := len(p.colors)
	for _, c := range px {
		if p.lut[c] != 0 {
			continue
		}
		if len(p.colors) >= 1<<16 {
			p.rollback(first)
			return fmt.Errorf("terrain: palette full")
		}
		p.colors = append(p.colors, c)
		p.lut[c] = uint32(len(p.colors))
	}
	buf := make([]byte, 2*(len(p.colors)-first))
	for i, c := range p.colors[first:] {
		binary.LittleEndian.PutUint16(buf[2*i:], c)
	}
	if _, err := p.f.WriteAt(buf, int64(8+2*first)); err != nil {
		p.rollback(first)
		return err
	}
	if !p.noSync {
		if err := p.f.Sync(); err != nil {
			p.rollback(first)
			return err
		}
	}
	for _, c := range p.colors[first:] {
		p.present[c>>6] |= 1 << (c & 63)
	}
	p.publish()
	return nil
}

func (p *palette) rollback(n int) {
	for _, c := range p.colors[n:] {
		p.lut[c] = 0
	}
	p.colors = p.colors[:n]
	_ = p.f.Truncate(int64(8 + 2*n))
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
