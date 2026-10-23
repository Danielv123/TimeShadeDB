package ingest

import (
	"strconv"
	"unicode/utf8"

	"timeshadedb/internal/core"
)

// A validating JSON decoder for the hot paths of ingest: envelope headers and the chunk
// and move messages that make up nearly every capture line. encoding/json decodes them
// by reflection and validates every byte twice (once per line, once per message); this
// decoder validates and decodes in one pass and allocates only the strings it keeps.
//
// It is an accelerator, not a second definition of the protocol: whenever the input is
// anything but the plain case (an unknown or differently cased key, an escape or
// non-ASCII byte in a kept string, a number encoding/json would reject for the field, a
// type mismatch, deep nesting, invalid JSON) it reports !ok and the caller decodes the
// same bytes with encoding/json, which then decides. When it reports ok, the result is
// what encoding/json would have produced (TestFastJSONMatchesEncodingJSON and
// FuzzFastJSON check this).

// fastMaxDepth bounds the nesting the fast path skips over; deeper values take the
// encoding/json path (which allows 10,000 levels).
const fastMaxDepth = 64

type lexer struct {
	b []byte
	i int
}

func (l *lexer) ws() {
	for l.i < len(l.b) {
		switch l.b[l.i] {
		case ' ', '\t', '\r', '\n':
			l.i++
		default:
			return
		}
	}
}

// eat consumes c if it is the next byte.
func (l *lexer) eat(c byte) bool {
	if l.i < len(l.b) && l.b[l.i] == c {
		l.i++
		return true
	}
	return false
}

func (l *lexer) peek() byte {
	if l.i < len(l.b) {
		return l.b[l.i]
	}
	return 0
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// str reads the string at l.i (which must hold '"'). ok reports whether it is a valid
// JSON string; plain whether its content is exactly the bytes between the quotes (no
// escapes, valid UTF-8), which is what encoding/json would decode it to.
func (l *lexer) str() (s []byte, plain, ok bool) {
	b := l.b
	start := l.i + 1
	plain = true
	ascii := true
	for i := start; i < len(b); {
		c := b[i]
		switch {
		case c == '"':
			l.i = i + 1
			s = b[start:i]
			if plain && !ascii {
				plain = utf8.Valid(s)
			}
			return s, plain, true
		case c == '\\':
			plain = false
			if i+1 >= len(b) {
				return nil, false, false
			}
			switch b[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				if i+5 >= len(b) || !isHex(b[i+2]) || !isHex(b[i+3]) || !isHex(b[i+4]) || !isHex(b[i+5]) {
					return nil, false, false
				}
				i += 6
			default:
				return nil, false, false
			}
		case c < 0x20:
			return nil, false, false
		default:
			if c >= 0x80 {
				ascii = false
			}
			i++
		}
	}
	return nil, false, false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// num reads a number with JSON's grammar and returns its text.
func (l *lexer) num() ([]byte, bool) {
	b, i := l.b, l.i
	start := i
	if i < len(b) && b[i] == '-' {
		i++
	}
	switch {
	case i < len(b) && b[i] == '0':
		i++
	case i < len(b) && b[i] >= '1' && b[i] <= '9':
		for i < len(b) && isDigit(b[i]) {
			i++
		}
	default:
		return nil, false
	}
	if i < len(b) && b[i] == '.' {
		i++
		if i >= len(b) || !isDigit(b[i]) {
			return nil, false
		}
		for i < len(b) && isDigit(b[i]) {
			i++
		}
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		i++
		if i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		if i >= len(b) || !isDigit(b[i]) {
			return nil, false
		}
		for i < len(b) && isDigit(b[i]) {
			i++
		}
	}
	l.i = i
	return b[start:i], true
}

// lit consumes the literal word (true, false, null).
func (l *lexer) lit(word string) bool {
	if len(l.b)-l.i < len(word) || string(l.b[l.i:l.i+len(word)]) != word {
		return false
	}
	l.i += len(word)
	return true
}

// skip validates and skips one value (after leading whitespace).
func (l *lexer) skip(depth int) bool {
	l.ws()
	switch c := l.peek(); c {
	case '"':
		_, _, ok := l.str()
		return ok
	case '{', '[':
		if depth >= fastMaxDepth {
			return false
		}
		closer := byte('}')
		if c == '[' {
			closer = ']'
		}
		l.i++
		l.ws()
		if l.eat(closer) {
			return true
		}
		for {
			if c == '{' {
				l.ws()
				if l.peek() != '"' {
					return false
				}
				if _, _, ok := l.str(); !ok {
					return false
				}
				l.ws()
				if !l.eat(':') {
					return false
				}
			}
			if !l.skip(depth + 1) {
				return false
			}
			l.ws()
			if l.eat(',') {
				continue
			}
			return l.eat(closer)
		}
	case 't':
		return l.lit("true")
	case 'f':
		return l.lit("false")
	case 'n':
		return l.lit("null")
	default:
		_, ok := l.num()
		return ok
	}
}

// isNull consumes a null literal if it is next.
func (l *lexer) isNull() bool {
	return l.peek() == 'n' && l.lit("null")
}

// The scalar readers below decode one value into its field as encoding/json would; a
// null leaves a non-pointer field unchanged, as encoding/json does.

func (l *lexer) int64Val(dst *int64, bits int) bool {
	if l.isNull() {
		return true
	}
	raw, ok := l.num()
	if !ok {
		return false
	}
	v, err := strconv.ParseInt(string(raw), 10, bits)
	if err != nil {
		return false
	}
	*dst = v
	return true
}

func (l *lexer) int64Ptr(dst **int64) bool {
	if l.isNull() {
		*dst = nil
		return true
	}
	var v int64
	if !l.int64Val(&v, 64) {
		return false
	}
	*dst = &v
	return true
}

func (l *lexer) floatVal(dst *float64) bool {
	if l.isNull() {
		return true
	}
	raw, ok := l.num()
	if !ok {
		return false
	}
	v, err := strconv.ParseFloat(string(raw), 64)
	if err != nil {
		return false
	}
	*dst = v
	return true
}

func (l *lexer) floatPtr(dst **float64) bool {
	if l.isNull() {
		*dst = nil
		return true
	}
	var v float64
	if !l.floatVal(&v) {
		return false
	}
	*dst = &v
	return true
}

func (l *lexer) boolVal(dst *bool) bool {
	switch l.peek() {
	case 't':
		*dst = true
		return l.lit("true")
	case 'f':
		*dst = false
		return l.lit("false")
	case 'n':
		return l.lit("null")
	}
	return false
}

// strBytes reads a plain string's content (nil, true for null: unchanged).
func (l *lexer) strBytes() (s []byte, null, ok bool) {
	if l.isNull() {
		return nil, true, true
	}
	if l.peek() != '"' {
		return nil, false, false
	}
	s, plain, ok := l.str()
	return s, false, ok && plain
}

func (l *lexer) strVal(dst *string, in *interner) bool {
	s, null, ok := l.strBytes()
	if ok && !null {
		*dst = in.get(s)
	}
	return ok
}

// key reads an object key and the colon after it; it must be plain.
func (l *lexer) key() ([]byte, bool) {
	l.ws()
	if l.peek() != '"' {
		return nil, false
	}
	k, plain, ok := l.str()
	if !ok || !plain {
		return nil, false
	}
	l.ws()
	if !l.eat(':') {
		return nil, false
	}
	l.ws()
	return k, true
}

// object calls member for each member of the object at l.i; member reads the value.
func (l *lexer) object(member func(k []byte) bool) bool {
	l.ws()
	if !l.eat('{') {
		return false
	}
	l.ws()
	if l.eat('}') {
		return true
	}
	for {
		k, ok := l.key()
		if !ok || !member(k) {
			return false
		}
		l.ws()
		if l.eat(',') {
			continue
		}
		return l.eat('}')
	}
}

// end reports whether only whitespace is left.
func (l *lexer) end() bool {
	l.ws()
	return l.i == len(l.b)
}

// foldEq reports whether k matches key case-insensitively (ASCII; the fast path only
// accepts exact keys and leaves other spellings encoding/json would match to it).
func foldEq(k []byte, key string) bool {
	if len(k) != len(key) {
		return false
	}
	for i := range k {
		a, b := k[i], key[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

func foldAny(k []byte, keys []string) bool {
	for _, key := range keys {
		if foldEq(k, key) {
			return true
		}
	}
	return false
}

// encoding/json also folds 'k' and 's' with the Kelvin and long-s signs; keys with
// non-ASCII bytes never reach the exact-match cases and fall back (see unknownKey).

// unknownKey reports whether a key the fast path does not decode can be skipped: it
// must not be another spelling of a field (encoding/json matches keys
// case-insensitively), and must be ASCII, so no Unicode case folding applies.
func unknownKey(k []byte, fields []string) bool {
	for _, c := range k {
		if c >= 0x80 {
			return false
		}
	}
	return !foldAny(k, fields)
}

var envFields = []string{"v", "tick", "speed", "src", "map", "instance", "recv_ms", "seq", "msgs", "caught_up", "ended"}

// parseEnvelopeFast decodes the envelope header of line into env and validates the
// whole line. It returns the msgs value (nil when absent) and ok=false when the line
// must take the encoding/json path. env must be zero.
func parseEnvelopeFast(line []byte, env *envHeader, in *interner) (msgs []byte, ok bool) {
	l := lexer{b: line}
	ok = l.object(func(k []byte) bool {
		switch string(k) {
		case "v":
			v := int64(env.V)
			if !l.int64Val(&v, strconv.IntSize) {
				return false
			}
			env.V = int(v)
			return true
		case "tick":
			return l.int64Val(&env.Tick, 64)
		case "speed":
			return l.floatVal(&env.Speed)
		case "src":
			return l.strVal(&env.Src, in)
		case "map":
			return l.strVal(&env.Map, in)
		case "instance":
			return l.int64Val(&env.Instance, 64)
		case "recv_ms":
			v := int64(env.RecvMs)
			if !l.int64Val(&v, 64) {
				return false
			}
			env.RecvMs = core.TimeMs(v)
			return true
		case "seq":
			return l.int64Ptr(&env.Seq)
		case "caught_up":
			return l.boolVal(&env.CaughtUp)
		case "ended":
			return l.boolVal(&env.Ended)
		case "msgs":
			v0 := l.i
			if !l.skip(0) {
				return false
			}
			msgs = line[v0:l.i]
			return true
		}
		return unknownKey(k, envFields) && l.skip(0)
	})
	if !ok || !l.end() {
		return nil, false
	}
	return msgs, true
}

var msgFields = []string{"type", "s", "f", "x", "y", "d", "tick", "m", "j", "trace", "n", "u", "icon", "text", "r", "q", "name", "dir"}

// decodeMsgFast decodes a message of the hot types (chunk, move, adopt, tag, recipe,
// rail) into m. A chunk's chart is left as text in m.dRaw (m.D stays nil). ok=false:
// decode raw with encoding/json instead (m is then garbage).
func decodeMsgFast(raw []byte, m *wireMsg, in *interner) bool {
	l := lexer{b: raw}
	seenM := false
	ok := l.object(func(k []byte) bool {
		switch string(k) {
		case "type":
			return l.strVal(&m.Type, in)
		case "s":
			return l.strVal(&m.S, in)
		case "f":
			return l.strVal(&m.F, in)
		case "x":
			return l.floatPtr(&m.X)
		case "y":
			return l.floatPtr(&m.Y)
		case "d":
			if l.isNull() {
				m.dRaw, m.hasD = nil, false
				return true
			}
			s, null, ok := l.strBytes()
			if !ok || null {
				return false
			}
			m.dRaw, m.hasD = s, true
			return true
		case "tick":
			return l.int64Ptr(&m.Tick)
		case "m":
			if seenM {
				return false // encoding/json reuses the first array's elements
			}
			seenM = true
			return l.moveEntries(&m.M, in)
		case "j":
			return l.strVal(&m.J, in)
		case "trace":
			return l.strVal(&m.Trace, in)
		case "n":
			return l.int64Val(&m.N, 64)
		case "u":
			return l.int64Val(&m.U, 64)
		case "icon":
			return l.strVal(&m.Icon, in)
		case "text":
			return l.strVal(&m.Text, in)
		case "r":
			return l.strVal(&m.R, in)
		case "q":
			return l.strVal(&m.Q, in)
		case "name":
			return l.strVal(&m.Name, in)
		case "dir":
			v := int64(m.Dir)
			if l.peek() == '-' || !l.int64Val(&v, 64) || v > 255 { // unsigned: encoding/json rejects even -0
				return false
			}
			m.Dir = uint8(v)
			return true
		}
		return false // anything else (other types, other spellings): encoding/json
	})
	return ok && l.end()
}

var moveFields = []string{"j", "gone", "tick", "k", "f", "name", "x", "y", "h", "n", "o", "c", "stop", "es", "car"}

// moveEntries decodes a move message's entry array.
func (l *lexer) moveEntries(dst *moveEntries, in *interner) bool {
	if l.isNull() {
		*dst = nil
		return true
	}
	if l.peek() != '[' {
		return false
	}
	n := countArray(l.b[l.i:], MaxMoveEntries)
	if n > MaxMoveEntries {
		return false // encoding/json reports the limit
	}
	out := make([]core.MoveEntry, 0, n)
	l.i++
	l.ws()
	if !l.eat(']') {
		for {
			if len(out) == n {
				return false
			}
			out = append(out, core.MoveEntry{})
			if !l.moveEntry(&out[len(out)-1], in) {
				return false
			}
			l.ws()
			if l.eat(',') {
				continue
			}
			if !l.eat(']') {
				return false
			}
			break
		}
	}
	*dst = out
	return true
}

func (l *lexer) moveEntry(e *core.MoveEntry, in *interner) bool {
	l.ws()
	return l.object(func(k []byte) bool {
		switch string(k) {
		case "j":
			return l.strVal(&e.J, in)
		case "gone":
			return l.boolVal(&e.Gone)
		case "tick":
			return l.int64Ptr(&e.Tick)
		case "k":
			return l.strVal(&e.K, in)
		case "f":
			return l.strVal(&e.F, in)
		case "name":
			return l.strVal(&e.Name, in)
		case "x":
			return l.floatVal(&e.X)
		case "y":
			return l.floatVal(&e.Y)
		case "h":
			return l.floatPtr(&e.H)
		case "n":
			v := int64(e.N)
			if !l.int64Val(&v, strconv.IntSize) {
				return false
			}
			e.N = int(v)
			return true
		case "o":
			return l.strVal(&e.O, in)
		case "c":
			return l.strVal(&e.C, in)
		case "stop":
			return l.boolVal(&e.Stop)
		case "es":
			return l.boolVal(&e.ES)
		case "car":
			return l.carriages(&e.Car)
		case "v":
			return l.floatPtr(&e.V)
		case "ru":
			return l.boolVal(&e.RU)
		case "rp":
			return l.railPieces(&e.RP)
		}
		return false
	})
}

// railPieces decodes [[x, y, direction, kind], ...] of whole numbers; anything else
// takes the encoding/json path.
func (l *lexer) railPieces(dst *[][4]int64) bool {
	if l.isNull() {
		*dst = nil
		return true
	}
	if !l.eat('[') {
		return false
	}
	out := [][4]int64{}
	l.ws()
	if l.eat(']') {
		*dst = out
		return true
	}
	for {
		l.ws()
		if !l.eat('[') {
			return false
		}
		var c [4]int64
		for k := range 4 {
			l.ws()
			raw, ok := l.num()
			if !ok {
				return false
			}
			v, err := strconv.ParseInt(string(raw), 10, 64)
			if err != nil {
				return false
			}
			c[k] = v
			l.ws()
			sep := byte(',')
			if k == 3 {
				sep = ']'
			}
			if !l.eat(sep) {
				return false
			}
		}
		out = append(out, c)
		if len(out) > MaxMoveEntries {
			return false
		}
		l.ws()
		if l.eat(',') {
			continue
		}
		if !l.eat(']') {
			return false
		}
		*dst = out
		return true
	}
}

// carriages decodes [[x, y, o], ...]; inner arrays of another length take the
// encoding/json path.
func (l *lexer) carriages(dst *[][3]float64) bool {
	if l.isNull() {
		*dst = nil
		return true
	}
	if !l.eat('[') {
		return false
	}
	out := [][3]float64{}
	l.ws()
	if l.eat(']') {
		*dst = out
		return true
	}
	for {
		l.ws()
		if !l.eat('[') {
			return false
		}
		var c [3]float64
		for k := range 3 {
			l.ws()
			raw, ok := l.num()
			if !ok {
				return false
			}
			v, err := strconv.ParseFloat(string(raw), 64)
			if err != nil {
				return false
			}
			c[k] = v
			l.ws()
			sep := byte(',')
			if k == 2 {
				sep = ']'
			}
			if !l.eat(sep) {
				return false
			}
		}
		out = append(out, c)
		if len(out) > MaxMoveEntries {
			return false
		}
		l.ws()
		if l.eat(',') {
			continue
		}
		if !l.eat(']') {
			return false
		}
		*dst = out
		return true
	}
}

// interner shares the strings that repeat across messages (surface and force names,
// mover ids, kinds, colours), so decoding a message allocates only new ones. One per
// decoding goroutine; it is cleared when it reaches maxInterned entries.
type interner struct{ m map[string]string }

const (
	maxInterned    = 4096
	maxInternedLen = 64
)

func (in *interner) get(b []byte) string {
	if in == nil || len(b) > maxInternedLen {
		return string(b)
	}
	if s, ok := in.m[string(b)]; ok {
		return s
	}
	if in.m == nil || len(in.m) >= maxInterned {
		in.m = make(map[string]string, 64)
	}
	s := string(b)
	in.m[s] = s
	return s
}
