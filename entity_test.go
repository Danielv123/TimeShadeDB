package timeshadedb

import (
	"strings"
	"testing"
)

func TestParseEntityTSVRow(t *testing.T) {
	point, err := ParseEntityTSVRow("save-1", "123\tnauvis\tplayer\tplayer\tDaniel\t7\t12.5\t-8.25\t0.75\t0.125")
	if err != nil {
		t.Fatal(err)
	}
	if point.Tick != 123 || point.Surface != "nauvis" || point.Force != "player" || point.EntityType != "player" || point.EntityID != "Daniel" {
		t.Fatalf("unexpected point identity: %+v", point)
	}
	if !point.HasSegment || point.Segment != 7 {
		t.Fatalf("unexpected point segment: %+v", point)
	}
	if point.X != 12.5 || point.Y != -8.25 || !point.HasOrientation || point.Orientation != 0.75 || !point.HasSpeed || point.Speed != 0.125 {
		t.Fatalf("unexpected point values: %+v", point)
	}
}

func TestFormatEntityPrometheus(t *testing.T) {
	points := []EntityPoint{{
		SavefileUUID: `save"1`,
		Tick:         42,
		Surface:      "nauvis",
		Force:        "player",
		EntityType:   "car",
		EntityID:     `unit\7`,
		Segment:      3,
		HasSegment:   true,
		X:            1.25,
		Y:            -2.5,
	}}
	got := string(FormatEntityPrometheus(points))
	wantX := `timeshadedb_entity_x{savefile="save\"1",surface="nauvis",force="player",entity_type="car",entity_id="unit\\7",segment="3"} 1.25 42`
	wantY := `timeshadedb_entity_y{savefile="save\"1",surface="nauvis",force="player",entity_type="car",entity_id="unit\\7",segment="3"} -2.5 42`
	if !strings.Contains(got, wantX) || !strings.Contains(got, wantY) {
		t.Fatalf("formatted prometheus lines:\n%s", got)
	}
	if strings.Contains(got, "tile_xy") {
		t.Fatalf("entity metrics must not include tile_xy label:\n%s", got)
	}
}
