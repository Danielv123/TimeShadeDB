// Package capturetest checks that what the capture Lua writes decodes into the Go
// contract types (internal/core/protocol.go) the ingest decoder (WP6) uses.
//
// fixtures/game_envelopes.ndjson holds real lines from a headless Factorio run of the
// standalone mod (capture/test/run_game.mjs --mode test --keep): chunks, trains with car,
// stop, es and read ticks, a gone, a car, recipes, rails, surfaces and resynced.
package capturetest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"os"
	"testing"

	"timeshadedb/internal/core"
)

// strict decodes raw into v, failing on fields the contract type does not know: the
// capture must not send fields ingest would silently drop.
func strict(t *testing.T, raw []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %T: %v\n%s", v, err, raw)
	}
}

// headingOf is the orientation (0 north, 0.25 east) of the direction from a to b.
func headingOf(a, b [3]float64) float64 {
	o := math.Atan2(b[0]-a[0], a[1]-b[1]) / (2 * math.Pi)
	return o - math.Floor(o)
}

// turn is the smaller angle between two orientations, in turns.
func turn(a, b float64) float64 {
	d := math.Abs(a-b) - math.Floor(math.Abs(a-b))
	return math.Min(d, 1-d)
}

func TestGameEnvelopesDecode(t *testing.T) {
	f, err := os.Open("fixtures/game_envelopes.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	counts := map[string]int{}
	flags := map[string]int{}
	lines := 0
	for sc.Scan() {
		lines++
		var env core.Envelope
		strict(t, sc.Bytes(), &env)
		if env.V != core.ProtocolVersion || len(env.Msgs) == 0 {
			t.Fatalf("line %d: v %d, %d messages", lines, env.V, len(env.Msgs))
		}
		if env.Src != "" || env.RecvMs != 0 || env.Map != "" {
			t.Fatalf("line %d: forwarder fields set by Lua", lines)
		}
		for _, raw := range env.Msgs {
			var h core.MsgHeader
			if err := json.Unmarshal(raw, &h); err != nil {
				t.Fatal(err)
			}
			counts[h.Type]++
			switch h.Type {
			case core.MsgChunk:
				var m core.ChunkMsg
				strict(t, raw, &m)
				if m.D == nil {
					flags["deleted"]++
					continue
				}
				if _, err := core.DecodeChart(*m.D); err != nil {
					t.Fatalf("chunk %d,%d: %v", m.X, m.Y, err)
				}
			case core.MsgMove:
				var m core.MoveMsg
				strict(t, raw, &m)
				for _, e := range m.M {
					switch {
					case e.Gone:
						flags["gone"]++
					case e.K == "t":
						if e.H == nil || e.N == 0 || e.O == "" || e.Name == "" || e.F == "" {
							t.Fatalf("train sample without its fields: %+v", e)
						}
						if e.Car != nil {
							flags["car"]++
							if len(e.Car) != e.N {
								t.Fatalf("car lists %d carriages, n = %d", len(e.Car), e.N)
							}
							// h: from the second carriage towards the first (PROTOCOL.md 3.1)
							if len(e.Car) >= 2 {
								want := headingOf(e.Car[1], e.Car[0])
								if turn(*e.H, want) > 0.003 {
									t.Fatalf("train %s: h %.3f, but the second carriage points to the first at %.3f",
										e.J, *e.H, want)
								}
								flags["h"]++
							}
						}
						if e.ES {
							flags["es"]++
						}
						if e.Stop {
							flags["stop"]++
						}
						if e.Tick != nil {
							flags["tick"]++
							if *e.Tick >= env.Tick {
								t.Fatalf("a sample's own tick %d is not before the envelope's %d", *e.Tick, env.Tick)
							}
						}
					case e.K == "v":
						flags["vehicle"]++
						if e.H == nil || (e.O != "C" && e.O != "S") {
							t.Fatalf("vehicle sample: %+v", e)
						}
					}
				}
			case core.MsgRecipe:
				var m core.RecipeMsg
				strict(t, raw, &m)
			case core.MsgRail:
				var m core.RailMsg
				strict(t, raw, &m)
			case core.MsgTag:
				var m core.TagMsg
				strict(t, raw, &m)
			case core.MsgSurfaces:
				var m core.SurfacesMsg
				strict(t, raw, &m)
			case core.MsgResynced:
				var m core.ResyncedMsg
				strict(t, raw, &m)
			case core.MsgAdopt:
				var m core.AdoptMsg
				strict(t, raw, &m)
			case core.MsgTrace:
				var m core.TraceMsg // traces carry extra context fields: decoded leniently
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unknown message type %q", h.Type)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{core.MsgChunk, core.MsgMove, core.MsgRecipe, core.MsgRail, core.MsgSurfaces,
		core.MsgResynced} {
		if counts[want] == 0 {
			t.Errorf("no %s message in the fixture", want)
		}
	}
	for _, want := range []string{"car", "h", "es", "stop", "tick", "gone", "vehicle"} {
		if flags[want] == 0 {
			t.Errorf("no sample with %s in the fixture", want)
		}
	}
	t.Logf("%d lines, messages %v, samples %v", lines, counts, flags)
}
