package core

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Capture protocol types (docs/v3/PROTOCOL.md). The capture Lua (WP9) writes
// envelopes; forwarders (the Clusterio plugin, WP10, and `timeshadedb tail`, WP6) add
// the source fields and POST them to /api/v3/ingest as NDJSON; the ingest decoder (WP6)
// turns them into ChunkUpdate, MoverSample and OverlayEvent values.

// ProtocolVersion is Envelope.V.
const ProtocolVersion = 1

// Envelope is one capture batch: one line of NDJSON.
type Envelope struct {
	// Set by Lua.
	V    int   `json:"v"`
	Tick int64 `json:"tick"`
	// Speed is game.speed when it is not 1 (ticks per second = 60 * Speed).
	Speed float64           `json:"speed,omitempty"`
	Msgs  []json.RawMessage `json:"msgs"`

	// Set by the forwarder.
	Src      string `json:"src,omitempty"`
	Map      string `json:"map,omitempty"`
	Instance int64  `json:"instance,omitempty"`
	RecvMs   TimeMs `json:"recv_ms,omitempty"`
	Seq      *int64 `json:"seq,omitempty"`
}

// Message type names (the "type" field of every message).
const (
	MsgChunk    = "chunk"
	MsgMove     = "move"
	MsgAdopt    = "adopt"
	MsgTag      = "tag"
	MsgRecipe   = "recipe"
	MsgRail     = "rail"
	MsgSurfaces = "surfaces"
	MsgResynced = "resynced"
	MsgTrace    = "trace"
	MsgMap      = "map"
)

// MsgHeader decodes only the type of a message.
type MsgHeader struct {
	Type string `json:"type"`
}

// ChunkMsg is a chunk's chart. D is helpers.encode_string of the 2,048-byte chart
// (zlib deflate, then standard base64); absent means the chunk was deleted.
type ChunkMsg struct {
	Type string  `json:"type"`
	S    string  `json:"s"`
	F    string  `json:"f"`
	X    int32   `json:"x"`
	Y    int32   `json:"y"`
	D    *string `json:"d,omitempty"`
	Tick *int64  `json:"tick,omitempty"`
}

// MoveEntry is one mover sample or gone inside a MoveMsg.
type MoveEntry struct {
	J    string  `json:"j"`
	Gone bool    `json:"gone,omitempty"`
	Tick *int64  `json:"tick,omitempty"`
	K    string  `json:"k,omitempty"`
	F    string  `json:"f,omitempty"`
	Name string  `json:"name,omitempty"`
	X    float64 `json:"x,omitempty"`
	Y    float64 `json:"y,omitempty"`
	// H is the orientation (0..1); absent means unknown.
	H *float64 `json:"h,omitempty"`
	N int      `json:"n,omitempty"`
	// O is the kinds string (train carriages or vehicle kind), C the colour "rrggbb".
	O string `json:"o,omitempty"`
	C string `json:"c,omitempty"`
	// Stop, ES: at rest; leading end switched.
	Stop bool `json:"stop,omitempty"`
	ES   bool `json:"es,omitempty"`
	// Car lists every carriage from the leading end as [x, y, orientation].
	Car [][3]float64 `json:"car,omitempty"`
	// V is a train's speed in tiles a tick. A train sample with V is a rail path sample
	// (docs/v3/RAIL-GEOMETRY.md 8): H is then the leading carriage's own orientation.
	V *float64 `json:"v,omitempty"`
	// RP lists the curved rail pieces and ramps the leading joint entered since the
	// previous sample as [x, y, direction, kind]; with RU a new rail path begins at this
	// sample and RP lists the pieces under the whole train, rear first.
	RP [][4]int64 `json:"rp,omitempty"`
	RU bool       `json:"ru,omitempty"`
}

// MoveMsg carries samples of movers on surface S.
type MoveMsg struct {
	Type string      `json:"type"`
	S    string      `json:"s"`
	M    []MoveEntry `json:"m"`
}

// AdoptMsg: a crossing's destination took journey J.
type AdoptMsg struct {
	Type  string `json:"type"`
	J     string `json:"j"`
	Trace string `json:"trace"`
}

// TagMsg is a chart tag's new state; no X means removed.
type TagMsg struct {
	Type string   `json:"type"`
	S    string   `json:"s"`
	F    string   `json:"f"`
	N    int64    `json:"n"`
	X    *float64 `json:"x,omitempty"`
	Y    *float64 `json:"y,omitempty"`
	Icon string   `json:"icon,omitempty"`
	Text string   `json:"text,omitempty"`
}

// RecipeMsg is a machine's recipe; no R means removed (no recipe, or machine gone).
type RecipeMsg struct {
	Type string   `json:"type"`
	S    string   `json:"s"`
	F    string   `json:"f"`
	U    int64    `json:"u"`
	X    *float64 `json:"x,omitempty"`
	Y    *float64 `json:"y,omitempty"`
	R    string   `json:"r,omitempty"`
	Q    string   `json:"q,omitempty"`
}

// RailMsg is a rail entity; no Name means removed.
type RailMsg struct {
	Type string   `json:"type"`
	S    string   `json:"s"`
	F    string   `json:"f"`
	U    int64    `json:"u"`
	Name string   `json:"name,omitempty"`
	X    *float64 `json:"x,omitempty"`
	Y    *float64 `json:"y,omitempty"`
	Dir  uint8    `json:"dir,omitempty"`
}

// SurfaceEntry is one surface in a SurfacesMsg.
type SurfaceEntry struct {
	Name string `json:"name"`
	Tile bool   `json:"tile"`
}

// SurfacesMsg lists the game's surfaces and whether each belongs to the capture clip.
type SurfacesMsg struct {
	Type    string         `json:"type"`
	List    []SurfaceEntry `json:"list"`
	Created bool           `json:"created,omitempty"`
}

// ResyncedMsg marks the end of a resync.
type ResyncedMsg struct {
	Type   string           `json:"type"`
	Trace  string           `json:"trace"`
	Counts map[string]int64 `json:"counts"`
}

// TraceMsg is a log line; forwarders log it and may drop it.
type TraceMsg struct {
	Type  string `json:"type"`
	Op    string `json:"op"`
	Phase string `json:"phase"`
	Level string `json:"level,omitempty"`
}

// MapMsg (forwarder only) sets the map metadata shown in the catalog.
type MapMsg struct {
	Type string  `json:"type"`
	Info MapInfo `json:"info"`
}

// EncodeChart encodes a chunk chart as Lua's helpers.encode_string does: the 2,048
// little-endian bytes, zlib-deflated, then standard base64.
func EncodeChart(px *ChunkPixels) string {
	raw := make([]byte, 2*ChunkPixelCount)
	for i, v := range px {
		binary.LittleEndian.PutUint16(raw[2*i:], v)
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// DecodeChart is the inverse of EncodeChart. It rejects charts that do not inflate to
// exactly 2,048 bytes, and reads at most 2,049 bytes of inflated data.
func DecodeChart(d string) (*ChunkPixels, error) {
	comp, err := base64.StdEncoding.DecodeString(d)
	if err != nil {
		return nil, fmt.Errorf("chart base64: %w", err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(comp))
	if err != nil {
		return nil, fmt.Errorf("chart zlib: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, 2*ChunkPixelCount+1))
	if err != nil {
		return nil, fmt.Errorf("chart inflate: %w", err)
	}
	if len(raw) != 2*ChunkPixelCount {
		return nil, fmt.Errorf("chart inflates to %d bytes, want %d", len(raw), 2*ChunkPixelCount)
	}
	var px ChunkPixels
	for i := range px {
		px[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}
	return &px, nil
}
