// Tag and recipe version tuples ported from illisis-minimap contracts.ts and
// store/overlays.ts. Blue Oak Model License 1.0.0: https://blueoakcouncil.org/license/1.0.0

package minimap

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"

	"timeshadedb/internal/core"
)

// OverlayReport counts what the overlay import read and wrote for one datastore.
type OverlayReport struct {
	DS string `json:"ds"`
	// Versions read from tags.json.gz and recipes.json.gz files.
	TagVersions    int `json:"tag_versions"`
	RecipeVersions int `json:"recipe_versions"`
	// Invalid tuples were dropped (bad shape, time or position).
	Invalid int `json:"invalid"`
	// Empty versions (end == start) are skipped. Overlaps counts versions that start
	// before the previous version of the same object ended; the later version wins.
	Empty    int `json:"empty"`
	Overlaps int `json:"overlaps"`
	// Intervals is the number of intervals the store should hold: versions minus empty
	// ones, minus versions that continue the previous one with the same content.
	Intervals int `json:"intervals"`
	// Events handed to the store (an open per version, a close per ended version that
	// the next version does not replace).
	Events int `json:"events"`
	// ErrorCount files failed to read or decode; Errors lists the first 20.
	ErrorCount int      `json:"error_count"`
	Errors     []string `json:"errors,omitempty"`
	// tiles are the z0 tiles holding the versions, for the read-back check.
	tiles map[core.TileCoord]struct{}
	first core.TimeMs
	last  core.TimeMs
}

// parseVersion reads [instance, id, force, start, end|null, x, y, a, b].
func parseVersion(v []any, kind core.IntervalKind) (core.Interval, string, bool) {
	var iv core.Interval
	if len(v) != 9 {
		return iv, "", false
	}
	inst, ok1 := integer(v[0], -1e15, 1e15)
	id, ok2 := integer(v[1], -1e15, 1e15)
	force, ok3 := str(v[2], core.MaxNameBytes)
	start, ok4 := integer(v[3], 0, math.MaxInt64)
	x, ok5 := v[5].(float64)
	y, ok6 := v[6].(float64)
	a, ok7 := str(v[7], 1000)
	b, ok8 := str(v[8], 10000)
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7 && ok8) || !saneTime(start) ||
		!finite(x, maxCoordAbs) || !finite(y, maxCoordAbs) || force == "" {
		return iv, "", false
	}
	iv = core.Interval{
		Kind:     kind,
		Key:      strconv.FormatInt(inst, 10) + "/" + strconv.FormatInt(id, 10),
		Instance: inst,
		Start:    core.TimeMs(start),
		X:        x,
		Y:        y,
	}
	if v[4] != nil {
		end, ok := integer(v[4], 0, math.MaxInt64)
		if !ok || !saneTime(end) || end < start {
			return iv, "", false
		}
		iv.End = core.TimeMs(end)
	}
	if kind == core.OverlayTag {
		iv.Icon, iv.Text = a, b
	} else {
		iv.Recipe, iv.Quality = a, b
	}
	return iv, force, true
}

func readVersions(path string) ([][]any, error) {
	b, err := readGz(path, maxJSONBytes)
	if err != nil {
		return nil, err
	}
	var list [][]any
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w: %v", path, ErrFormat, err)
	}
	return list, nil
}

// importOverlays turns every tag and recipe version into open and close events and
// applies them in time order (closes before opens at equal times).
func importOverlays(ctx context.Context, d *Dataset, opt *Options, store core.OverlayStore) ([]*OverlayReport, core.ApplyResult, error) {
	var res core.ApplyResult
	var reports []*OverlayReport
	var events []core.OverlayEvent
	type objKey struct {
		ds   core.DatastoreKey
		kind core.IntervalKind
		key  string
	}
	type version struct {
		iv  core.Interval
		rep *OverlayReport
	}
	objects := map[objKey][]version{}
	var order []objKey
	for _, src := range d.Datastores {
		if (src.Tags == "" && len(src.Recipes) == 0) || !opt.wantDS(src.Key) {
			continue
		}
		rep := &OverlayReport{DS: src.Key.ID(), tiles: map[core.TileCoord]struct{}{}}
		reports = append(reports, rep)
		files := []struct {
			path string
			kind core.IntervalKind
		}{}
		if src.Tags != "" {
			files = append(files, struct {
				path string
				kind core.IntervalKind
			}{src.Tags, core.OverlayTag})
		}
		for _, p := range src.Recipes {
			files = append(files, struct {
				path string
				kind core.IntervalKind
			}{p, core.OverlayRecipe})
		}
		for _, f := range files {
			list, err := readVersions(f.path)
			if err != nil {
				rep.ErrorCount++
				if len(rep.Errors) < 20 {
					rep.Errors = append(rep.Errors, err.Error())
				}
				continue
			}
			for _, v := range list {
				iv, force, ok := parseVersion(v, f.kind)
				if !ok {
					rep.Invalid++
					continue
				}
				ds := core.DatastoreKey{Map: src.Key.Map, Surface: src.Key.Surface, Force: force}
				if f.kind == core.OverlayTag {
					rep.TagVersions++
				} else {
					rep.RecipeVersions++
				}
				rep.tiles[core.TileAt(iv.X, iv.Y, 0)] = struct{}{}
				if rep.first == 0 || iv.Start < rep.first {
					rep.first = iv.Start
				}
				rep.last = max(rep.last, iv.Start, iv.End)
				if iv.End == iv.Start {
					rep.Empty++
					continue
				}
				k := objKey{ds, iv.Kind, iv.Key}
				if _, ok := objects[k]; !ok {
					order = append(order, k)
				}
				objects[k] = append(objects[k], version{iv, rep})
			}
		}
	}
	for _, k := range order {
		vs := objects[k]
		sort.SliceStable(vs, func(i, j int) bool { return vs[i].iv.Start < vs[j].iv.Start })
		for i, v := range vs {
			rep := v.rep
			contiguous := i > 0 && vs[i-1].iv.End != 0 && vs[i-1].iv.End >= v.iv.Start
			if i > 0 && (vs[i-1].iv.End == 0 || vs[i-1].iv.End > v.iv.Start) {
				rep.Overlaps++
				contiguous = true
			}
			if !(contiguous && vs[i-1].iv.SameContent(v.iv)) {
				rep.Intervals++
			}
			events = append(events, core.OverlayEvent{DS: k.ds, T: v.iv.Start, Interval: v.iv})
			rep.Events++
			if v.iv.End != 0 && (i == len(vs)-1 || vs[i+1].iv.Start > v.iv.End) {
				events = append(events, core.OverlayEvent{DS: k.ds, T: v.iv.End, Close: true, Interval: v.iv})
				rep.Events++
			}
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if a.T != b.T {
			return a.T < b.T
		}
		return a.Close && !b.Close
	})
	for i := 0; i < len(events); i += 8192 {
		if err := ctx.Err(); err != nil {
			return reports, res, err
		}
		r, err := store.Apply(ctx, events[i:min(i+8192, len(events))])
		res.Add(r)
		if err != nil {
			return reports, res, err
		}
	}
	return reports, res, nil
}
