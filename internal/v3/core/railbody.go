package core

import "math"

// Train bodies on the rail path (docs/v3/RAIL-GEOMETRY.md 8.2). A rail path row lists only the
// curved pieces and ramps the leading joint entered since the row before, so a viewer that
// starts from one row (a keyframe's, or the first of a journey in the live payload) knows the
// head but not where the body behind it lies. RailBody follows a journey's rows the way the
// viewer builds its chain and tells, for each row, which pieces listed by earlier rows still
// lie under the train; stores put them in front of the row's own pieces in keyframes and live
// payloads (MoverStore, RailWithBody), as stop samples already carry them.

// Rail piece lengths in rail distance (RAIL-GEOMETRY.md 2.9).
var railPieceLen = [5]float64{0, 5.132284556, 5.077891568, 7.842081, 16.762233}

// railDirSteps are the integer steps of the 16 rail directions: even ones the compass
// directions, odd ones the 1:2 half-diagonals (RAIL-GEOMETRY.md 8.1).
var railDirSteps = [16][2]int32{
	{0, -1}, {1, -2}, {1, -1}, {2, -1}, {1, 0}, {2, 1}, {1, 1}, {1, 2},
	{0, 1}, {-1, 2}, {-1, 1}, {-2, 1}, {-1, 0}, {-2, -1}, {-1, -1}, {-1, -2},
}

// Carriage geometry of vanilla rolling stock (RAIL-GEOMETRY.md 3): a carriage's joints are 4
// apart, a carriage's front joint 7 behind the one before, and a row's leading joint 2 tiles
// ahead of its centre.
const (
	railJoint = 4
	railPitch = 7
)

// railBodySlack is added to a train's length when deciding which pieces lie under it: the
// leading joint on a ramp's slope, pitches measured a little longer than 7, rounding.
const railBodySlack = 4

// railMaxKept bounds the pieces a RailBody keeps.
const railMaxKept = 4096

// railSym is the 8-fold placement of RAIL-GEOMETRY.md 2.1 applied to an offset.
func railSym(i8 uint8, x, y int32) (int32, int32) {
	if i8&1 != 0 {
		x = -x
	}
	if i8&2 != 0 {
		x, y = -y, x
	}
	if i8&4 != 0 {
		x, y = -x, -y
	}
	return x, y
}

// railSymDir is the same placement applied to a 16-way direction.
func railSymDir(i8, d uint8) uint8 {
	if i8&1 != 0 {
		d = (16 - d) % 16
	}
	if i8&2 != 0 {
		d = (d + 4) % 16
	}
	if i8&4 != 0 {
		d = (d + 8) % 16
	}
	return d
}

// railEnd is one end of a placed piece: its position (whole tiles) and outward direction.
type railEnd struct {
	x, y int32
	d    uint8
}

// railPieceEnds returns a piece's front and back ends (RAIL-GEOMETRY.md 8.1 end table).
func railPieceEnds(p RailPiece) (front, back railEnd) {
	var c [6]int32
	switch p.Kind {
	case RailKindCurveA:
		c = [6]int32{0, 2, 8, -1, -3, 15}
	case RailKindCurveB:
		c = [6]int32{1, 2, 7, -2, -2, 14}
	case RailKindLegacyCurve:
		c = [6]int32{1, 4, 8, -2, -3, 14}
	case RailKindRamp:
		// turned a quarter clockwise per 4 of the direction
		fx, fy := int32(0), int32(8)
		for q := 0; q < int(p.Dir>>2)&3; q++ {
			fx, fy = -fy, fx
		}
		fd := (8 + p.Dir&12) % 16
		return railEnd{p.X + fx, p.Y + fy, fd}, railEnd{p.X - fx, p.Y - fy, (fd + 8) % 16}
	default:
		return railEnd{p.X, p.Y, 0}, railEnd{p.X, p.Y, 8}
	}
	i8 := (p.Dir >> 1) & 7
	fx, fy := railSym(i8, c[0], c[1])
	bx, by := railSym(i8, c[3], c[4])
	return railEnd{p.X + fx, p.Y + fy, railSymDir(i8, uint8(c[2]))}, railEnd{p.X + bx, p.Y + by, railSymDir(i8, uint8(c[5]))}
}

// RailPieceEnds returns a piece's front and back ends as (x, y, outward 16-way direction):
// RAIL-GEOMETRY.md 8.1's end table under 2.1's placement. An unknown kind gives its position.
func RailPieceEnds(p RailPiece) (front, back [3]int32) {
	f, k := railPieceEnds(p)
	return [3]int32{f.x, f.y, int32(f.d)}, [3]int32{k.x, k.y, int32(k.d)}
}

// RailPieceLength returns a piece kind's rail distance from end to end (RAIL-GEOMETRY.md
// 2.9), 0 for an unknown kind.
func RailPieceLength(kind uint8) float64 {
	if int(kind) >= len(railPieceLen) {
		return 0
	}
	return railPieceLen[kind]
}

// RailDirStep returns the integer step of a 16-way rail direction (RAIL-GEOMETRY.md 8.1).
func RailDirStep(d uint8) (int32, int32) {
	st := railDirSteps[d%16]
	return st[0], st[1]
}

// railConnects reports whether end s continues the path that leaves through end e: they
// face each other and s lies straight ahead of e (equal, or joined by straight rails).
func railConnects(e, s railEnd) bool {
	if s.d != (e.d+8)%16 {
		return false
	}
	st := railDirSteps[e.d]
	px, py := int64(s.x-e.x), int64(s.y-e.y)
	dx, dy := int64(st[0]), int64(st[1])
	return px*dy-py*dx == 0 && px*dx+py*dy >= 0
}

// railDirVector is the unit vector of a 16-way rail direction.
func railDirVector(d uint8) (float64, float64) {
	st := railDirSteps[d%16]
	l := math.Hypot(float64(st[0]), float64(st[1]))
	return float64(st[0]) / l, float64(st[1]) / l
}

// RailRow is what RailBody needs of a journey's row.
type RailRow struct {
	T TimeMs
	// X, Y (tiles) and Orientation as stored (MoverRow).
	X, Y, Orientation float64
	Flags             MoverFlags
	Rail              []RailPiece
	RailStart         bool
	// Carriages is the train's carriage count (MoverMeta.Carriages); 0 gives no bodies.
	Carriages int
}

// railState says what a RailBody knows.
type railState uint8

const (
	railNone    railState = iota // no rail run (no row yet, or a gone or a row without FlagRail)
	railAnchor                   // a run without pieces yet: the line through its first row
	railPieces                   // a run with pieces
	railUnknown                  // lost track (a piece that neither continues nor resends): wait for a row that lists the whole train
)

// RailBody follows the rail path rows of one journey, in time order, and gives each row's
// body: the curved pieces and ramps that earlier rows of its rail run listed and that still
// lie under the train behind its leading joint, rear first. It follows the viewer's chain
// (RAIL-GEOMETRY.md 8.2, web/src/movers/track.ts railRuns): a run begins at a row with
// RailStart or an end switch, or after a row without FlagRail or a gone; each later row's
// pieces continue it, and a piece that does not but is already on it is a resend. A piece
// that does neither loses the path until a row that lists the whole train (a run start, a
// stop sample, or the sample where a stopped train starts); rows meanwhile get no body.
//
// The zero value is ready. Bodies share storage with the RailBody and with each other and
// must not be modified; they stay valid after later calls. Clone gives an independent copy.
type RailBody struct {
	state railState
	// the anchor line of a run without pieces: the first row's leading joint and heading
	ax, ay, aux, auy float64
	// the run's pieces in travel order (the oldest dropped once far behind the train) and
	// the rail distance of each one's exit end
	pieces []RailPiece
	uExit  []float64
	// the exit end of the last piece and the rail distance there
	ex   railEnd
	uEnd float64
	// the previous row: its time, whether it was a stop sample, and its body
	has      bool
	lastT    TimeMs
	stopped  bool
	lastBody []RailPiece
}

// Clone returns an independent copy of b (later calls on either do not affect the other).
func (b *RailBody) Clone() *RailBody {
	c := *b
	c.pieces = append([]RailPiece(nil), b.pieces...)
	c.uExit = append([]float64(nil), b.uExit...)
	return &c
}

// Add takes the journey's next row and returns its body (nil when there is none to give: no
// pieces under the train from earlier rows, the path unknown, or a row that lists the whole
// train itself: a run start, a stop sample, the sample after a stop). A row at the previous
// row's time is a duplicate and gets the previous body; an older row loses the path.
func (b *RailBody) Add(r RailRow) []RailPiece {
	if b.has && r.T <= b.lastT {
		if r.T == b.lastT {
			return b.lastBody
		}
		b.lost()
		return nil
	}
	body := b.add(r)
	b.has, b.lastT, b.lastBody = true, r.T, body
	b.stopped = r.Flags&(FlagRail|FlagStop|FlagGone) == FlagRail|FlagStop
	return body
}

// Last returns the time of the last row given to Add or Seed (false before any).
func (b *RailBody) Last() (TimeMs, bool) { return b.lastT, b.has }

// Seed begins a run at r as if it started one, whatever its flags: for a store that
// restarts from a keyframe, whose rows list the pieces under the train.
func (b *RailBody) Seed(r RailRow) {
	b.reset()
	if r.Flags&FlagRail != 0 && r.Flags&FlagGone == 0 {
		b.start(r)
	}
	b.has, b.lastT, b.lastBody = true, r.T, nil
	b.stopped = r.Flags&FlagStop != 0
}

func (b *RailBody) reset() {
	b.state = railNone
	b.pieces, b.uExit = nil, nil
}

func (b *RailBody) lost() {
	b.reset()
	b.state = railUnknown
}

// leadingJoint returns a row's leading joint and unit heading.
func leadingJoint(r *RailRow) (x, y, ux, uy float64) {
	s, c := math.Sincos(2 * math.Pi * r.Orientation)
	return r.X + railJoint/2*s, r.Y - railJoint/2*c, s, -c
}

func (b *RailBody) add(r RailRow) []RailPiece {
	if r.Flags&FlagGone != 0 || r.Flags&FlagRail == 0 {
		b.reset()
		return nil
	}
	whole := r.Flags&FlagStop != 0 || b.stopped // the row lists the pieces under the train
	if b.state == railNone || r.RailStart || r.Flags&FlagEndSwitch != 0 || (b.state == railUnknown && whole) {
		b.start(r)
		return nil
	}
	if b.state == railUnknown {
		return nil
	}
	before := len(b.pieces)
	for _, p := range r.Rail {
		if b.appendNew(p) {
			continue
		}
		if b.holds(p) {
			continue // a resend
		}
		if whole {
			b.start(r)
		} else {
			b.lost()
		}
		return nil
	}
	if b.state != railPieces {
		return nil
	}
	jx, jy, _, _ := leadingJoint(&r)
	ux, uy := railDirVector(b.ex.d)
	px, py := jx-float64(b.ex.x), jy-float64(b.ex.y)
	uHead := b.uEnd + px*ux + py*uy
	if px*ux+py*uy < 0 {
		// still on the last piece: its exit is at most this far ahead along it
		uHead = b.uEnd - math.Hypot(px, py)
	} else if math.Abs(px*uy-py*ux) > 1 {
		// the leading joint is off the lead-out line: this is not the path the viewer builds
		b.lost()
		return nil
	}
	n := r.Carriages
	if n <= 0 {
		n = railCarriagesHint // only for trimming: a row without a carriage count gets no body
	}
	reach := float64(railPitch*n-railPitch+railJoint) + railBodySlack
	// drop the pieces far behind the train, keeping enough to recognise resends
	before -= b.trim(uHead, 2*reach+64)
	if whole || r.Carriages <= 0 || before <= 0 {
		return nil
	}
	k := before
	for k > 0 && uHead-b.uExit[k-1] <= reach {
		k--
	}
	if k == before {
		return nil
	}
	return b.pieces[k:before:before]
}

// trim drops the pieces whose exit lies more than keep behind uHead (and the oldest beyond
// railMaxKept) and returns how many it dropped.
func (b *RailBody) trim(uHead, keep float64) int {
	n := 0
	for n < len(b.pieces) && (uHead-b.uExit[n] > keep || len(b.pieces)-n > railMaxKept) {
		n++
	}
	if n > 0 {
		b.pieces, b.uExit = b.pieces[n:], b.uExit[n:]
	}
	return n
}

// railCarriagesHint is the train length RailBody keeps pieces for when a row has no carriage
// count; such rows get no body.
const railCarriagesHint = 256

// start begins a run at r: its pieces from the rear (the first one run the way that connects
// to the second, a lone one the way whose exit faces r's heading), or the line through its
// leading joint along its heading when it lists none. A piece that does not connect ends the
// run's pieces there, as the viewer's chain does.
func (b *RailBody) start(r RailRow) {
	b.reset()
	jx, jy, ux, uy := leadingJoint(&r)
	b.state = railAnchor
	b.ax, b.ay, b.aux, b.auy = jx, jy, ux, uy
	if len(r.Rail) == 0 {
		return
	}
	p0 := r.Rail[0]
	f, k := railPieceEnds(p0)
	var rev bool
	if len(r.Rail) > 1 {
		f1, k1 := railPieceEnds(r.Rail[1])
		fits := func(e railEnd) bool { return railConnects(e, f1) || railConnects(e, k1) }
		// run back to front when only the front end connects on
		rev = !fits(k) && fits(f)
	} else {
		bx, by := railDirVector(k.d)
		fx, fy := railDirVector(f.d)
		rev = fx*ux+fy*uy > bx*ux+by*uy
	}
	b.first(p0, rev)
	for _, p := range r.Rail[1:] {
		if !b.appendNew(p) && !b.holds(p) {
			break
		}
	}
}

// first makes p (run back to front when rev) the run's first piece.
func (b *RailBody) first(p RailPiece, rev bool) {
	f, k := railPieceEnds(p)
	if rev {
		f, k = k, f
	}
	l := railPieceLen[p.Kind]
	b.pieces = []RailPiece{p}
	b.uExit = []float64{l}
	b.ex, b.uEnd = k, l
	b.state = railPieces
}

// appendNew appends p when it continues the run and reports whether it did.
func (b *RailBody) appendNew(p RailPiece) bool {
	if p.Kind < RailKindCurveA || p.Kind > RailKindRamp {
		return false
	}
	f, k := railPieceEnds(p)
	if b.state == railAnchor {
		// the anchor line: the piece's entry must lie on it, ahead, facing back
		onLine := func(e railEnd) bool {
			px, py := float64(e.x)-b.ax, float64(e.y)-b.ay
			ox, oy := railDirVector(e.d)
			return math.Abs(px*b.auy-py*b.aux) < 0.3 && ox*b.aux+oy*b.auy < -0.9 && px*b.aux+py*b.auy > -8
		}
		switch {
		case onLine(f):
			b.first(p, false)
		case onLine(k):
			b.first(p, true)
		default:
			return false
		}
		return true
	}
	if b.state != railPieces {
		return false
	}
	switch {
	case railConnects(b.ex, f):
	case railConnects(b.ex, k):
		f, k = k, f
	default:
		return false
	}
	u := b.uEnd + math.Hypot(float64(f.x-b.ex.x), float64(f.y-b.ex.y)) + railPieceLen[p.Kind]
	b.pieces = append(b.pieces, p)
	b.uExit = append(b.uExit, u)
	b.ex, b.uEnd = k, u
	return true
}

// holds reports whether p is one of the run's pieces.
func (b *RailBody) holds(p RailPiece) bool {
	for i := len(b.pieces) - 1; i >= 0; i-- {
		if b.pieces[i] == p {
			return true
		}
	}
	return false
}

// RailWithBody returns the rail pieces a keyframe or live payload row carries: body (from
// RailBody.Add) in front of the row's own pieces, at most MaxRailPieces (the rearmost body
// pieces are left out first). It returns rail itself when body is empty.
func RailWithBody(body, rail []RailPiece) []RailPiece {
	if len(body) == 0 {
		return rail
	}
	if n := len(body) + len(rail) - MaxRailPieces; n > 0 {
		if n >= len(body) {
			return rail
		}
		body = body[n:]
	}
	out := make([]RailPiece, 0, len(body)+len(rail))
	return append(append(out, body...), rail...)
}
