package core

import (
	"errors"
	"fmt"
	"strings"
)

// DatastoreKey names one terrain datastore: a (map, surface, force) triple. The fields
// are raw names as the game or the forwarder gives them.
//
// Map is the map key. Clusterio gridworld maps use "gw-<world name>" (all tiles and
// non-excluded planets stitched into one coordinate space), other Clusterio surfaces
// use "i-<instance id>", and the standalone tailer uses its --map flag.
type DatastoreKey struct {
	Map     string `json:"map"`
	Surface string `json:"surface"`
	Force   string `json:"force"`
}

// SurfaceKey names one map surface. Movers are stored per surface (all forces
// together; each sample carries its force).
type SurfaceKey struct {
	Map     string `json:"map"`
	Surface string `json:"surface"`
}

// ErrBadKey is returned for empty or malformed keys and ids.
var ErrBadKey = errors.New("bad key")

// MaxNameBytes bounds each raw name in a key, so ids stay short enough for paths.
const MaxNameBytes = 200

// SurfaceKey returns the surface the datastore belongs to.
func (k DatastoreKey) SurfaceKey() SurfaceKey {
	return SurfaceKey{Map: k.Map, Surface: k.Surface}
}

// Validate checks that every field is non-empty and at most MaxNameBytes long.
func (k DatastoreKey) Validate() error {
	for _, f := range [...]struct{ name, v string }{{"map", k.Map}, {"surface", k.Surface}, {"force", k.Force}} {
		if f.v == "" || len(f.v) > MaxNameBytes {
			return fmt.Errorf("%w: %s %q", ErrBadKey, f.name, f.v)
		}
	}
	return nil
}

// Validate checks that every field is non-empty and at most MaxNameBytes long.
func (k SurfaceKey) Validate() error {
	return DatastoreKey{Map: k.Map, Surface: k.Surface, Force: "-"}.Validate()
}

// ID returns the datastore id used in directory names and URLs:
// EscapeName(map) "." EscapeName(surface) "." EscapeName(force).
// Example: {"gw-bc41437d.1", "nauvis", "player"} -> "gw-bc41437d~2E1.nauvis.player".
func (k DatastoreKey) ID() string {
	return EscapeName(k.Map) + "." + EscapeName(k.Surface) + "." + EscapeName(k.Force)
}

// ID returns the surface id used in directory names and URLs:
// EscapeName(map) "." EscapeName(surface).
func (k SurfaceKey) ID() string {
	return EscapeName(k.Map) + "." + EscapeName(k.Surface)
}

func (k DatastoreKey) String() string { return k.ID() }
func (k SurfaceKey) String() string   { return k.ID() }

// ParseDatastoreID is the inverse of DatastoreKey.ID.
func ParseDatastoreID(id string) (DatastoreKey, error) {
	parts, err := parseID(id, 3)
	if err != nil {
		return DatastoreKey{}, err
	}
	k := DatastoreKey{Map: parts[0], Surface: parts[1], Force: parts[2]}
	return k, k.Validate()
}

// ParseSurfaceID is the inverse of SurfaceKey.ID.
func ParseSurfaceID(id string) (SurfaceKey, error) {
	parts, err := parseID(id, 2)
	if err != nil {
		return SurfaceKey{}, err
	}
	k := SurfaceKey{Map: parts[0], Surface: parts[1]}
	return k, k.Validate()
}

func parseID(id string, n int) ([]string, error) {
	raw := strings.Split(id, ".")
	if len(raw) != n {
		return nil, fmt.Errorf("%w: id %q has %d parts, want %d", ErrBadKey, id, len(raw), n)
	}
	out := make([]string, n)
	for i, p := range raw {
		s, err := UnescapeName(p)
		if err != nil {
			return nil, err
		}
		if EscapeName(s) != p {
			return nil, fmt.Errorf("%w: id %q is not in canonical form", ErrBadKey, id)
		}
		out[i] = s
	}
	return out, nil
}

// EscapeName escapes one raw name for ids. Bytes in [a-z0-9_-] are kept; every other
// byte of the UTF-8 encoding, upper-case letters included, is written as "~XX" with
// upper-case hex. Escaping upper case keeps ids distinct on case-insensitive file
// systems (Windows, macOS). Escaping "." makes it usable as the part separator.
func EscapeName(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('~')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// UnescapeName is the inverse of EscapeName. It rejects characters EscapeName never
// writes and lower-case hex digits.
func UnescapeName(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%w: empty name", ErrBadKey)
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-':
			out = append(out, c)
		case c == '~':
			if i+2 >= len(s) {
				return "", fmt.Errorf("%w: truncated escape in %q", ErrBadKey, s)
			}
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if !ok1 || !ok2 {
				return "", fmt.Errorf("%w: bad escape in %q", ErrBadKey, s)
			}
			out = append(out, hi<<4|lo)
			i += 2
		default:
			return "", fmt.Errorf("%w: character %q in %q", ErrBadKey, c, s)
		}
	}
	return string(out), nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
