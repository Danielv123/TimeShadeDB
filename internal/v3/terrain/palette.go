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
// The colours, the RGB565 -> index LUT and the presence bitmap are published together
// as an immutable snapshot (copy-on-write). Readers, commits and the Apply fast path
// load it without a lock; only ensure, when it adds colours, takes mu, writes the file
// and swaps in a new snapshot.
type palette struct {
	path   string
	noSync bool

	mu     sync.Mutex // writers: ensure, close
	f      *os.File
	colors []uint16 // backing array of the snapshots; only appended under mu
	snap   atomic.Pointer[palSnap]
}

// palSnap is one immutable palette state.
type palSnap struct {
	colors  []uint16 // len == cap: appends never show through
	lut     lut
	present *[1 << 10]uint64 // bit c set when colour c is in the palette
}

func (ps *palSnap) has(c uint16) bool { return ps.present[c>>6]&(1<<(c&63)) != 0 }

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
	p := &palette{path: path, noSync: noSync, f: f}
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
	ps := &palSnap{lut: newLUT(), present: new([1 << 10]uint64)}
	for i := range p.colors {
		c := binary.LittleEndian.Uint16(body[2*i:])
		p.colors[i] = c
		if ps.lut[c] == 0 {
			ps.lut[c] = uint32(i + 1)
		}
		ps.present[c>>6] |= 1 << (c & 63)
	}
	if p.colors[0] != 0 {
		f.Close()
		return nil, fmt.Errorf("terrain: %s: entry 0 is %#04x, not 0", path, p.colors[0])
	}
	ps.colors = p.colors[:n:n]
	p.snap.Store(ps)
	return p, nil
}

// snapshot returns the current palette colours. The slice must not be modified.
func (p *palette) snapshot() []uint16 { return p.snap.Load().colors }

// state returns the current immutable palette state (colours and LUT).
func (p *palette) state() *palSnap { return p.snap.Load() }

// ensure makes every colour of px part of the palette, appending and fsyncing new
// ones before it returns. It is safe for concurrent use; the common case (all colours
// known) takes no lock.
func (p *palette) ensure(px []uint16) error {
	if p.covers(p.snap.Load(), px) {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f == nil {
		return errClosedPalette
	}
	old := p.snap.Load()
	if p.covers(old, px) {
		return nil // added by a concurrent ensure
	}
	first := len(p.colors)
	nl := make(lut, len(old.lut))
	copy(nl, old.lut)
	for _, c := range px {
		if nl[c] != 0 {
			continue
		}
		if len(p.colors) >= 1<<16 {
			p.colors = p.colors[:first]
			return fmt.Errorf("terrain: palette full")
		}
		p.colors = append(p.colors, c)
		nl[c] = uint32(len(p.colors))
	}
	buf := make([]byte, 2*(len(p.colors)-first))
	for i, c := range p.colors[first:] {
		binary.LittleEndian.PutUint16(buf[2*i:], c)
	}
	if err := p.write(buf, first); err != nil {
		p.colors = p.colors[:first]
		_ = p.f.Truncate(int64(8 + 2*first))
		return err
	}
	np := &palSnap{lut: nl, present: new([1 << 10]uint64)}
	*np.present = *old.present
	for _, c := range p.colors[first:] {
		np.present[c>>6] |= 1 << (c & 63)
	}
	n := len(p.colors)
	np.colors = p.colors[:n:n]
	p.snap.Store(np)
	return nil
}

func (p *palette) covers(ps *palSnap, px []uint16) bool {
	for _, c := range px {
		if !ps.has(c) {
			return false
		}
	}
	return true
}

func (p *palette) write(buf []byte, first int) error {
	if _, err := p.f.WriteAt(buf, int64(8+2*first)); err != nil {
		return err
	}
	if !p.noSync {
		return p.f.Sync()
	}
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
