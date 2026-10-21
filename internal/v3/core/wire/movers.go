package wire

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"timeshadedb/internal/v3/core"
)

// MVW3 header flags.
const (
	// MoverHasTicks: the tick column holds data (at least one row has a tick).
	MoverHasTicks uint16 = 1 << 0
	// MoverHasRail: the speed and rail path columns follow the eight base columns (at
	// least one row has core.FlagRail).
	MoverHasRail uint16 = 1 << 1
)

// wireHasCarriages is the row-flag bit that marks a carriage block in the carriage
// column. It exists only on the wire; core.MoverFlags uses bits 0-5 and 7.
const wireHasCarriages = 1 << 6

// MoverColumns is the number of base row columns after the mover table; windows with
// MoverHasRail have MoverRailColumns more.
const (
	MoverColumns     = 8
	MoverRailColumns = 2
)

// MaxCarriages is the largest carriage count of one row that DecodeMoverWindow accepts.
// Stores must reject samples with more, or they write windows they cannot read back.
const MaxCarriages = 10000

func zigzag(v int64) uint64   { return uint64(v<<1) ^ uint64(v>>63) }
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

type colWriter struct{ b []byte }

func (c *colWriter) uvarint(v uint64) { c.b = binary.AppendUvarint(c.b, v) }
func (c *colWriter) varint(v int64)   { c.uvarint(zigzag(v)) }

type colReader struct {
	b   []byte
	off int
	err error
}

func (c *colReader) uvarint() uint64 {
	if c.err != nil {
		return 0
	}
	v, n := binary.Uvarint(c.b[c.off:])
	if n <= 0 {
		c.err = fmt.Errorf("%w: bad varint", ErrFormat)
		return 0
	}
	c.off += n
	return v
}

func (c *colReader) varint() int64 { return unzigzag(c.uvarint()) }

func (c *colReader) byte() byte {
	if c.err != nil {
		return 0
	}
	if c.off >= len(c.b) {
		c.err = fmt.Errorf("%w: column too short", ErrFormat)
		return 0
	}
	v := c.b[c.off]
	c.off++
	return v
}

// EncodeMoverWindow encodes an MVW3 body. Rows must be sorted by (Mover, T) and Mover
// must index w.Movers. Values are quantized (core.PosUnits, core.OrientUnits).
func EncodeMoverWindow(w *core.MoverWindow) ([]byte, error) {
	if w.Kind < core.WindowClosed || w.Kind > core.WindowLive {
		return nil, fmt.Errorf("wire: window kind %d", w.Kind)
	}
	meta, err := json.Marshal(w.Movers)
	if err != nil {
		return nil, err
	}
	if w.Movers == nil {
		meta = []byte("[]")
	}
	counts := make([]uint64, len(w.Movers))
	var flags uint16
	for i, r := range w.Rows {
		if r.Mover < 0 || r.Mover >= len(w.Movers) {
			return nil, fmt.Errorf("wire: row %d mover %d out of range", i, r.Mover)
		}
		if i > 0 {
			p := w.Rows[i-1]
			if r.Mover < p.Mover || (r.Mover == p.Mover && r.T <= p.T) {
				return nil, fmt.Errorf("wire: rows not sorted by (mover, t) at %d", i)
			}
		}
		if r.Flags&wireHasCarriages != 0 {
			return nil, fmt.Errorf("wire: row %d uses reserved flag bits", i)
		}
		if r.Flags&core.FlagRail != 0 && len(r.Rail) > core.MaxRailPieces {
			return nil, fmt.Errorf("wire: row %d has %d rail pieces", i, len(r.Rail))
		}
		counts[r.Mover]++
		if r.Flags&core.FlagNoTick == 0 {
			flags |= MoverHasTicks
		}
		if r.Flags&(core.FlagRail|core.FlagGone) == core.FlagRail {
			flags |= MoverHasRail
		}
	}
	var cols [MoverColumns + MoverRailColumns]colWriter
	cCount, cDT, cTick, cX, cY, cO, cFlags, cCar := &cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5], &cols[6], &cols[7]
	cSpeed, cRail := &cols[8], &cols[9]
	for _, n := range counts {
		cCount.uvarint(n)
	}
	var prevT core.TimeMs
	var prevTick, prevX, prevY, prevSpeed int64
	var prevO uint16
	for i, r := range w.Rows {
		if i == 0 || r.Mover != w.Rows[i-1].Mover {
			prevT, prevTick, prevX, prevY, prevO, prevSpeed = w.Start, 0, 0, 0, 0, 0
		}
		cDT.varint(int64(r.T - prevT))
		prevT = r.T
		if flags&MoverHasTicks != 0 {
			if r.Flags&core.FlagNoTick != 0 {
				cTick.varint(0)
			} else {
				cTick.varint(r.Tick - prevTick)
				prevTick = r.Tick
			}
		}
		f := byte(r.Flags)
		if r.Flags&core.FlagGone != 0 {
			cX.varint(0)
			cY.varint(0)
			cO.varint(0)
			cFlags.b = append(cFlags.b, f&^byte(core.FlagRail))
			continue
		}
		x, y := core.QuantizePos(r.X), core.QuantizePos(r.Y)
		o := core.QuantizeOrientation(r.Orientation)
		cX.varint(x - prevX)
		cY.varint(y - prevY)
		cO.varint(int64(int16(o - prevO)))
		prevX, prevY, prevO = x, y, o
		if len(r.Carriages) > 0 {
			f |= wireHasCarriages
			cCar.uvarint(uint64(len(r.Carriages)))
			for _, c := range r.Carriages {
				cCar.varint(core.QuantizePos(c.X) - x)
				cCar.varint(core.QuantizePos(c.Y) - y)
				cCar.uvarint(uint64(core.QuantizeOrientation(c.Orientation)))
			}
		}
		if r.Flags&core.FlagRail != 0 {
			// The speed as a difference from the previous rail row of the entry; the rail
			// entry as uvarint (pieces << 1 | rail start), then each piece's tile position
			// as a difference from the previous piece (the first from the row's tile) and
			// a byte kind << 4 | direction.
			sp := int64(core.QuantizeSpeed(r.Speed))
			cSpeed.varint(sp - prevSpeed)
			prevSpeed = sp
			n := uint64(len(r.Rail)) << 1
			if r.RailStart {
				n |= 1
			}
			cRail.uvarint(n)
			px, py := x>>4, y>>4
			for _, p := range r.Rail {
				cRail.varint(int64(p.X) - px)
				cRail.varint(int64(p.Y) - py)
				cRail.b = append(cRail.b, p.Kind<<4|p.Dir&15)
				px, py = int64(p.X), int64(p.Y)
			}
		}
		cFlags.b = append(cFlags.b, f)
	}
	ncols := MoverColumns
	if flags&MoverHasRail != 0 {
		ncols += MoverRailColumns
	}
	size := 32 + len(meta)
	for i := range ncols {
		size += 4 + len(cols[i].b)
	}
	out := make([]byte, 32, size)
	copy(out, MagicMovers)
	out[4] = byte(w.Kind)
	out[5] = 1 // format version
	binary.LittleEndian.PutUint16(out[6:], flags)
	binary.LittleEndian.PutUint64(out[8:], uint64(w.Start))
	binary.LittleEndian.PutUint32(out[16:], uint32(w.Len))
	binary.LittleEndian.PutUint32(out[20:], uint32(len(w.Movers)))
	binary.LittleEndian.PutUint32(out[24:], uint32(len(w.Rows)))
	binary.LittleEndian.PutUint32(out[28:], uint32(len(meta)))
	out = append(out, meta...)
	for i := range ncols {
		out = binary.LittleEndian.AppendUint32(out, uint32(len(cols[i].b)))
		out = append(out, cols[i].b...)
	}
	return out, nil
}

// DecodeMoverWindow decodes an MVW3 body. The surface is not part of the body; the
// caller passes it from the URL.
func DecodeMoverWindow(b []byte, sfc core.SurfaceKey) (*core.MoverWindow, error) {
	if len(b) < 32 || string(b[:4]) != MagicMovers {
		return nil, fmt.Errorf("%w: not an MVW3 body", ErrFormat)
	}
	if b[5] != 1 {
		return nil, fmt.Errorf("%w: MVW3 version %d", ErrFormat, b[5])
	}
	w := &core.MoverWindow{
		Surface: sfc,
		Kind:    core.WindowKind(b[4]),
		Start:   core.TimeMs(int64(binary.LittleEndian.Uint64(b[8:]))),
		Len:     core.TimeMs(binary.LittleEndian.Uint32(b[16:])),
	}
	if w.Kind < core.WindowClosed || w.Kind > core.WindowLive {
		return nil, fmt.Errorf("%w: window kind %d", ErrFormat, w.Kind)
	}
	flags := binary.LittleEndian.Uint16(b[6:])
	nMovers := int(binary.LittleEndian.Uint32(b[20:]))
	nRows := int(binary.LittleEndian.Uint32(b[24:]))
	metaLen := int(binary.LittleEndian.Uint32(b[28:]))
	off := 32
	if metaLen > len(b)-off {
		return nil, fmt.Errorf("%w: mover table length", ErrFormat)
	}
	if err := json.Unmarshal(b[off:off+metaLen], &w.Movers); err != nil {
		return nil, fmt.Errorf("%w: mover table: %v", ErrFormat, err)
	}
	if len(w.Movers) != nMovers {
		return nil, fmt.Errorf("%w: %d movers in table, header says %d", ErrFormat, len(w.Movers), nMovers)
	}
	off += metaLen
	ncols := MoverColumns
	if flags&MoverHasRail != 0 {
		ncols += MoverRailColumns
	}
	var cols [MoverColumns + MoverRailColumns]colReader
	for i := range ncols {
		if len(b)-off < 4 {
			return nil, fmt.Errorf("%w: missing column %d", ErrFormat, i)
		}
		n := int(binary.LittleEndian.Uint32(b[off:]))
		off += 4
		if n > len(b)-off {
			return nil, fmt.Errorf("%w: column %d length", ErrFormat, i)
		}
		cols[i].b = b[off : off+n]
		off += n
	}
	if off != len(b) {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrFormat, len(b)-off)
	}
	cCount, cDT, cTick, cX, cY, cO, cFlags, cCar := &cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5], &cols[6], &cols[7]
	cSpeed, cRail := &cols[8], &cols[9]
	w.Rows = make([]core.MoverRow, 0, nRows)
	for m := 0; m < nMovers; m++ {
		n := int(cCount.uvarint())
		if n > nRows-len(w.Rows) {
			return nil, fmt.Errorf("%w: row counts exceed %d rows", ErrFormat, nRows)
		}
		prevT, prevTick, prevX, prevY, prevO, prevSpeed := w.Start, int64(0), int64(0), int64(0), uint16(0), int64(0)
		for k := 0; k < n; k++ {
			r := core.MoverRow{Mover: m}
			prevT += core.TimeMs(cDT.varint())
			r.T = prevT
			var tickDelta int64
			if flags&MoverHasTicks != 0 {
				tickDelta = cTick.varint()
			}
			dx, dy, do := cX.varint(), cY.varint(), cO.varint()
			f := cFlags.byte()
			r.Flags = core.MoverFlags(f &^ wireHasCarriages)
			if r.Flags&core.FlagNoTick == 0 {
				prevTick += tickDelta
				r.Tick = prevTick
			}
			if r.Flags&core.FlagGone == 0 {
				prevX += dx
				prevY += dy
				prevO += uint16(do)
				r.X = float64(prevX) / core.PosUnits
				r.Y = float64(prevY) / core.PosUnits
				r.Orientation = float64(prevO) / core.OrientUnits
			}
			if f&wireHasCarriages != 0 {
				nc := int(cCar.uvarint())
				if nc > MaxCarriages {
					return nil, fmt.Errorf("%w: %d carriages", ErrFormat, nc)
				}
				r.Carriages = make([]core.Carriage, nc)
				for i := range r.Carriages {
					cx := prevX + cCar.varint()
					cy := prevY + cCar.varint()
					co := cCar.uvarint()
					r.Carriages[i] = core.Carriage{X: float64(cx) / core.PosUnits, Y: float64(cy) / core.PosUnits, Orientation: float64(co) / core.OrientUnits}
				}
			}
			if r.Flags&core.FlagRail != 0 && r.Flags&core.FlagGone == 0 {
				if flags&MoverHasRail == 0 {
					return nil, fmt.Errorf("%w: rail row without rail columns", ErrFormat)
				}
				prevSpeed += cSpeed.varint()
				r.Speed = float64(prevSpeed) / core.SpeedUnits
				head := cRail.uvarint()
				np := head >> 1
				if np > core.MaxRailPieces {
					return nil, fmt.Errorf("%w: %d rail pieces", ErrFormat, np)
				}
				r.RailStart = head&1 != 0
				if np > 0 {
					r.Rail = make([]core.RailPiece, np)
					px, py := prevX>>4, prevY>>4
					for i := range r.Rail {
						px += cRail.varint()
						py += cRail.varint()
						kd := cRail.byte()
						r.Rail[i] = core.RailPiece{X: int32(px), Y: int32(py), Dir: kd & 15, Kind: kd >> 4}
					}
				}
			} else {
				r.Flags &^= core.FlagRail
			}
			w.Rows = append(w.Rows, r)
		}
	}
	for i := range ncols {
		if cols[i].err != nil {
			return nil, cols[i].err
		}
		if cols[i].off != len(cols[i].b) {
			return nil, fmt.Errorf("%w: column %d has %d unread bytes", ErrFormat, i, len(cols[i].b)-cols[i].off)
		}
	}
	if len(w.Rows) != nRows {
		return nil, fmt.Errorf("%w: decoded %d rows, header says %d", ErrFormat, len(w.Rows), nRows)
	}
	return w, nil
}
