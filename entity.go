package timeshadedb

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

type EntityPoint struct {
	SavefileUUID   string
	Tick           uint64
	Surface        string
	Force          string
	EntityType     string
	EntityID       string
	Segment        uint64
	X              float64
	Y              float64
	Orientation    float64
	Speed          float64
	HasSegment     bool
	HasOrientation bool
	HasSpeed       bool
}

func ParseEntityTSVRow(savefileUUID, line string) (EntityPoint, error) {
	fields := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
	if len(fields) < 7 || len(fields) > 10 {
		return EntityPoint{}, fmt.Errorf("timeshadedb: expected 7 to 10 entity TSV fields, got %d", len(fields))
	}
	tick, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 10, 64)
	if err != nil {
		return EntityPoint{}, fmt.Errorf("timeshadedb: invalid entity tick: %w", err)
	}
	point := EntityPoint{
		SavefileUUID: savefileUUID,
		Tick:         tick,
		Surface:      strings.TrimSpace(fields[1]),
		Force:        strings.TrimSpace(fields[2]),
		EntityType:   strings.TrimSpace(fields[3]),
		EntityID:     strings.TrimSpace(fields[4]),
	}
	if point.SavefileUUID == "" {
		return EntityPoint{}, fmt.Errorf("timeshadedb: savefile UUID is required")
	}
	if point.Surface == "" {
		return EntityPoint{}, fmt.Errorf("timeshadedb: entity surface is required")
	}
	if point.Force == "" {
		return EntityPoint{}, fmt.Errorf("timeshadedb: entity force is required")
	}
	if point.EntityType == "" {
		return EntityPoint{}, fmt.Errorf("timeshadedb: entity type is required")
	}
	if point.EntityID == "" {
		return EntityPoint{}, fmt.Errorf("timeshadedb: entity id is required")
	}
	xField := 5
	if len(fields) == 10 {
		point.Segment, err = strconv.ParseUint(strings.TrimSpace(fields[5]), 10, 64)
		if err != nil {
			return EntityPoint{}, fmt.Errorf("timeshadedb: invalid entity segment: %w", err)
		}
		if point.Segment == 0 {
			return EntityPoint{}, fmt.Errorf("timeshadedb: entity segment must be positive")
		}
		point.HasSegment = true
		xField = 6
	}
	point.X, err = strconv.ParseFloat(strings.TrimSpace(fields[xField]), 64)
	if err != nil {
		return EntityPoint{}, fmt.Errorf("timeshadedb: invalid entity x: %w", err)
	}
	point.Y, err = strconv.ParseFloat(strings.TrimSpace(fields[xField+1]), 64)
	if err != nil {
		return EntityPoint{}, fmt.Errorf("timeshadedb: invalid entity y: %w", err)
	}
	if len(fields) > xField+2 && strings.TrimSpace(fields[xField+2]) != "" {
		point.Orientation, err = strconv.ParseFloat(strings.TrimSpace(fields[xField+2]), 64)
		if err != nil {
			return EntityPoint{}, fmt.Errorf("timeshadedb: invalid entity orientation: %w", err)
		}
		point.HasOrientation = true
	}
	if len(fields) > xField+3 && strings.TrimSpace(fields[xField+3]) != "" {
		point.Speed, err = strconv.ParseFloat(strings.TrimSpace(fields[xField+3]), 64)
		if err != nil {
			return EntityPoint{}, fmt.Errorf("timeshadedb: invalid entity speed: %w", err)
		}
		point.HasSpeed = true
	}
	return point, nil
}

func ParseEntityTSV(savefileUUID string, scanner *bufio.Scanner) ([]EntityPoint, error) {
	var points []EntityPoint
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		point, err := ParseEntityTSVRow(savefileUUID, line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		points = append(points, point)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return points, nil
}

func FormatEntityPrometheus(points []EntityPoint) []byte {
	var buf bytes.Buffer
	for _, point := range points {
		labels := entityMetricLabels(point)
		fmt.Fprintf(&buf, "timeshadedb_entity_x{%s} %s %d\n", labels, strconv.FormatFloat(point.X, 'f', -1, 64), point.Tick)
		fmt.Fprintf(&buf, "timeshadedb_entity_y{%s} %s %d\n", labels, strconv.FormatFloat(point.Y, 'f', -1, 64), point.Tick)
	}
	return buf.Bytes()
}

func entityMetricLabels(point EntityPoint) string {
	pairs := []struct {
		name  string
		value string
	}{
		{name: "savefile", value: point.SavefileUUID},
		{name: "surface", value: point.Surface},
		{name: "force", value: point.Force},
		{name: "entity_type", value: point.EntityType},
		{name: "entity_id", value: point.EntityID},
	}
	if point.HasSegment {
		pairs = append(pairs, struct {
			name  string
			value string
		}{name: "segment", value: strconv.FormatUint(point.Segment, 10)})
	}
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, pair.name+`="`+escapePrometheusLabelValue(pair.value)+`"`)
	}
	return strings.Join(parts, ",")
}

func escapePrometheusLabelValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
