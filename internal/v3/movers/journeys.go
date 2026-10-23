// The owner rule, adoption, resync and stopped-instance handling are ported from
// illisis-minimap store/movers.ts (Movers.move, adopt, begin, resynced, stopped, end).
// Blue Oak Model License 1.0.0: https://blueoakcouncil.org/license/1.0.0

package movers

import (
	"container/list"
	"maps"
	"strings"

	"timeshadedb/internal/v3/core"
)

// Journeys applies the journey rules of PLAN.md §4.4 to decoded move messages before
// they reach a MoverStore. Ingest (WP6) keeps one per map and feeds it every move
// message in envelope order; the store then assigns FlagHandoff at commit.
//
//   - Owner rule (illisis K28): a journey belongs to the first source that samples it.
//     Samples and gones from any other source are dropped, until an "adopt" message
//     moves the journey to the adopting source (Universal Edges, gridworld crossings).
//     Across an adopt the rule goes by sample time: the previous owner's samples from
//     before the adopt (its time, or the adopting source's first sample when earlier:
//     the cut) and its gone are kept when they arrive after the adopt (Stats.LateKept).
//     Each instance batches its envelopes about once a second and a Universal Edges
//     crossing takes less, so the destination's adopt often reaches ingest before the
//     source's last samples and gone. A late gone after the cut is kept just before it,
//     so the journey's rows stay one path with a handoff; late samples after it are
//     dropped. An adopt of a journey Journeys does not know (the server restarted, as
//     it does with the Clusterio controller, and the instances' backlogs arrive in any
//     order) takes the first other source that sends samples from before the cut as
//     the previous owner. Known limits: only the last adopt is remembered, so when a
//     journey returns to its source (A to B to A) before A's late tail and gone of the
//     first crossing arrive, they are taken as the owner's and the gone ends the
//     journey there.
//     Each instance's clock is anchored on its own, so the next owner's first samples
//     can come a few ms before the gone of the last one. Those up to goneSlack before
//     it are moved just after it, so the gone stays between the two sources' rows
//     (Stats.AfterGone).
//     A gone from the owner clears the owner, so the next source to sample the journey
//     owns it: a train crossing between gridworld instances keeps its id, and the gone
//     followed by a sample within core.HandoffWindow becomes a handoff in the store.
//     For core.HandoffWindow after its gone (by sample time) the old owner cannot take
//     the journey back while no other source has claimed it: a stale sample it still
//     sends would otherwise make it the owner again and lock out the instance the
//     journey moved to (Stats.Quarantined). The same source can resume the journey
//     after that window, or at once once another source has owned it.
//   - Gones carry no meta on the wire (PROTOCOL.md 5); Journeys fills in the journey's
//     last meta and surface. For a journey it does not know (forgotten by the bound, or
//     the server restarted since its last sample) it asks Lookup, the store's last row of
//     the journey on the gone's surface (Stats.ResolvedGones); a gone of a journey that
//     neither knows, or that already ended, is dropped (as illisis drops gones of
//     journeys without an owner).
//   - A sample on another surface than the journey's last one first emits a gone on
//     the old surface at the sample's time (the mover left that surface).
//   - Train carriage count, kinds and colour are carried forward from the previous
//     sample when a sample omits them.
//   - Resync: after Begin(src), Resynced(src, t) emits gones for every journey src owns
//     that it did not re-send. Stopped(src, t) emits gones for src's players (its
//     trains and vehicles stay parked).
//
// Memory is bounded: at most max journeys are tracked; the least recently seen is
// forgotten first (its next sample starts a new owner).
//
// Mark starts recording the changes, which Rollback undoes and Commit keeps: ingest
// applies a batch's journey rules, then stores the samples, and undoes the rules when
// the store refused the batch, so a retried batch meets the journeys as the first try did.
type Journeys struct {
	// Lookup, if set, returns the meta of journey id's last row on surface sfc when the
	// journey is live there (the store's state), for gones of journeys Journeys forgot.
	Lookup func(sfc core.SurfaceKey, id string) (core.MoverMeta, bool)

	max   int
	byID  map[string]*list.Element
	lru   *list.List // front = most recently seen
	seen  map[string]map[string]bool
	stats JourneyStats
	undo  *journeyUndo
}

// journeyUndo records what a batch changed since Mark.
type journeyUndo struct {
	before  map[*journey]journey       // journeys changed: their state at Mark
	added   []*journey                 // journeys added since Mark
	evicted []*journey                 // journeys forgotten since Mark (re-added at the back)
	seen    map[string]map[string]bool // resync sets at Mark (nil: had none)
	stats   JourneyStats
}

// Mark starts recording changes for Rollback (or Commit).
func (js *Journeys) Mark() {
	js.undo = &journeyUndo{before: map[*journey]journey{}, seen: map[string]map[string]bool{}, stats: js.stats}
}

// Commit keeps the changes since Mark and stops recording.
func (js *Journeys) Commit() { js.undo = nil }

// Rollback undoes every change since Mark and stops recording.
func (js *Journeys) Rollback() {
	u := js.undo
	if u == nil {
		return
	}
	js.undo = nil
	for _, j := range u.added {
		if e := js.byID[j.id]; e != nil && e.Value.(*journey) == j {
			js.lru.Remove(e)
			delete(js.byID, j.id)
		}
	}
	for _, j := range u.evicted {
		if js.byID[j.id] == nil {
			js.byID[j.id] = js.lru.PushBack(j)
		}
	}
	for j, b := range u.before {
		*j = b
	}
	for src, set := range u.seen {
		if set == nil {
			delete(js.seen, src)
		} else {
			js.seen[src] = set
		}
	}
	js.stats = u.stats
}

// change records j's state before a change (once per Mark).
func (js *Journeys) change(j *journey) {
	if u := js.undo; u != nil {
		if _, ok := u.before[j]; !ok {
			u.before[j] = *j
		}
	}
}

// changeSeen records src's resync set before a change (once per Mark).
func (js *Journeys) changeSeen(src string) {
	if u := js.undo; u != nil {
		if _, ok := u.seen[src]; !ok {
			if cur := js.seen[src]; cur != nil {
				u.seen[src] = maps.Clone(cur)
			} else {
				u.seen[src] = nil
			}
		}
	}
}

// JourneyStats counts what Journeys did.
type JourneyStats struct {
	// OwnerDrops are samples and gones dropped by the owner rule.
	OwnerDrops int64
	// UnknownGones are gones of journeys that were not tracked and that Lookup did not
	// know either (or that had ended already).
	UnknownGones int64
	// ResolvedGones are gones of journeys that were not tracked, completed from Lookup.
	ResolvedGones int64
	// SurfaceChanges are journeys that moved to another surface (a gone was emitted on
	// the old one).
	SurfaceChanges int64
	// Evicted are journeys forgotten because of the bound.
	Evicted int64
	// Quarantined are samples dropped because their source sent the journey's gone
	// less than core.HandoffWindow earlier and no other source has claimed it yet.
	Quarantined int64
	// LateKept are samples and gones of a journey's previous owner from before another
	// source adopted it, kept although they arrived after the adopt.
	LateKept int64
	// AfterGone are samples of a journey's next owner from just before the last
	// owner's gone, moved to just after it.
	AfterGone int64
}

type journey struct {
	id    string
	owner string // "" = no owner
	sfc   core.SurfaceKey
	meta  core.MoverMeta
	known bool // sfc and meta are set (a sample was seen)
	lastT core.TimeMs
	// goneBy is the source whose gone cleared the owner, at goneT; it cannot reclaim
	// the journey before goneT + HandoffWindow unless another source owned it since.
	goneBy string
	goneT  core.TimeMs
	// prev is the owner an adopt took the journey from, with its surface and meta then
	// (prevKnown); its samples before cut, and its gone, are still the journey's (late).
	// anyPrev: the adopt did not know the journey, so the first other source with a
	// sample before cut becomes prev. Cleared by prev's gone and whenever the journey
	// changes owner otherwise.
	prev      string
	anyPrev   bool
	prevKnown bool
	prevSfc   core.SurfaceKey
	prevMeta  core.MoverMeta
	cut       core.TimeMs
	// afterBy sent the gone that ended the last ownership at afterT: another source's
	// samples up to goneSlack before afterT are moved after it. Cleared by the first
	// sample kept at its own time and when an adopt takes the journey from an owner.
	afterBy string
	afterT  core.TimeMs
}

// goneSlack is how far a next owner's sample may come before the last owner's gone and
// still be moved after it. Instance clocks differ by tens of ms at a crossing; a larger
// overlap is left as it is.
const goneSlack core.TimeMs = 250

// NewJourneys returns a tracker of at most max journeys (default 100,000 when max <= 0).
func NewJourneys(max int) *Journeys {
	if max <= 0 {
		max = 100_000
	}
	return &Journeys{max: max, byID: map[string]*list.Element{}, lru: list.New(), seen: map[string]map[string]bool{}}
}

func (js *Journeys) get(id string) *journey {
	if e := js.byID[id]; e != nil {
		js.lru.MoveToFront(e)
		return e.Value.(*journey)
	}
	return nil
}

func (js *Journeys) add(id string) *journey {
	j := &journey{id: id}
	js.byID[id] = js.lru.PushFront(j)
	if js.undo != nil {
		js.undo.added = append(js.undo.added, j)
	}
	for js.lru.Len() > js.max {
		e := js.lru.Back()
		js.lru.Remove(e)
		old := e.Value.(*journey)
		delete(js.byID, old.id)
		js.stats.Evicted++
		if js.undo != nil {
			js.undo.evicted = append(js.undo.evicted, old)
		}
	}
	return j
}

// goneOf returns a gone sample for j at t.
func goneOf(j *journey, t core.TimeMs, tick int64, flags core.MoverFlags) core.MoverSample {
	return core.MoverSample{Surface: j.sfc, Meta: j.meta, T: t, Tick: tick, Flags: core.FlagGone | flags&core.FlagNoTick}
}

// Move filters and completes one move message's samples from src (in message order) and
// appends the samples to store to out, which it returns.
func (js *Journeys) Move(src string, samples []core.MoverSample, out []core.MoverSample) []core.MoverSample {
	for _, s := range samples {
		j := js.get(s.Meta.ID)
		if j != nil && j.owner != "" && j.owner != src {
			if j.prev == src || j.anyPrev {
				out = js.late(j, src, s, out)
				continue
			}
			js.stats.OwnerDrops++
			continue
		}
		if s.Flags&core.FlagGone != 0 {
			if (j == nil || !j.known) && js.Lookup != nil {
				if meta, ok := js.Lookup(s.Surface, s.Meta.ID); ok {
					if j == nil {
						j = js.add(s.Meta.ID)
					}
					js.change(j)
					j.owner, j.sfc, j.meta, j.known = src, s.Surface, meta, true
					js.stats.ResolvedGones++
				}
			}
			if j == nil || j.owner == "" || !j.known {
				js.stats.UnknownGones++
				continue
			}
			js.change(j)
			out = append(out, goneOf(j, s.T, s.Tick, s.Flags))
			// The owner's gone clears the owner; meta and surface are kept so a resumed
			// journey carries forward its train details.
			j.owner, j.goneBy, j.goneT, j.prev, j.anyPrev = "", src, s.T, "", false
			j.afterBy, j.afterT = src, s.T
			j.lastT = s.T
			continue
		}
		if j != nil && j.owner == "" && j.goneBy == src && s.T < j.goneT+core.HandoffWindow {
			js.stats.Quarantined++
			continue
		}
		if j == nil {
			j = js.add(s.Meta.ID)
		}
		js.change(j)
		if j.known && j.sfc != s.Surface {
			if j.owner != "" { // still live on the old surface
				out = append(out, goneOf(j, s.T, s.Tick, s.Flags))
			}
			js.stats.SurfaceChanges++
		}
		if j.known && s.Meta.Kind == core.KindTrain && j.meta.Kind == core.KindTrain {
			if s.Meta.Carriages == 0 {
				s.Meta.Carriages = j.meta.Carriages
			}
			if s.Meta.Kinds == "" {
				s.Meta.Kinds = j.meta.Kinds
			}
			if s.Meta.Color == "" {
				s.Meta.Color = j.meta.Color
			}
		}
		if j.afterBy != "" {
			if j.afterBy != src && s.T <= j.afterT && j.afterT-s.T <= goneSlack {
				s.T = j.afterT + 1
				j.afterT = s.T // a second such sample goes after this one
				js.stats.AfterGone++
			} else {
				j.afterBy = ""
			}
		}
		if j.owner != src {
			j.prev, j.anyPrev = "", false
		} else if (j.prev != "" || j.anyPrev) && s.T < j.cut {
			j.cut = s.T // the adopting source's first sample came before the adopt
		}
		j.owner, j.sfc, j.meta, j.known, j.lastT = src, s.Surface, s.Meta, true, s.T
		j.goneBy = ""
		if seen := js.seen[src]; seen != nil {
			js.changeSeen(src)
			js.seen[src][s.Meta.ID] = true
		}
		out = append(out, s)
	}
	return out
}

// late handles sample s from src, j's previous owner (or, with anyPrev, any other
// source), which arrived after another source adopted j: a sample from before the cut
// is kept, and so is the gone (just before the cut at the latest), which ends what the
// previous owner may still send.
func (js *Journeys) late(j *journey, src string, s core.MoverSample, out []core.MoverSample) []core.MoverSample {
	if s.Flags&core.FlagGone != 0 {
		if j.anyPrev {
			// A gone before any sample of an unknown previous owner: the store's last row
			// of the journey gives its meta, as for gones of journeys Journeys forgot.
			var meta core.MoverMeta
			ok := js.Lookup != nil
			if ok {
				meta, ok = js.Lookup(s.Surface, s.Meta.ID)
			}
			if !ok {
				js.stats.OwnerDrops++
				return out
			}
			js.change(j)
			j.prev, j.anyPrev, j.prevKnown, j.prevSfc, j.prevMeta = src, false, true, s.Surface, meta
			js.stats.ResolvedGones++
		}
		js.change(j)
		g := core.MoverSample{Surface: j.prevSfc, Meta: j.prevMeta, T: s.T, Tick: s.Tick, Flags: core.FlagGone | s.Flags&core.FlagNoTick}
		if g.T >= j.cut {
			// Moved before the cut, its tick no longer matches its time. (A sample of the
			// previous owner at exactly cut-1 is replaced by it in the store.)
			g.T, g.Tick, g.Flags = j.cut-1, 0, g.Flags|core.FlagNoTick
		}
		j.prev, j.anyPrev = "", false
		if !j.prevKnown {
			js.stats.UnknownGones++
			return out
		}
		js.stats.LateKept++
		return append(out, g)
	}
	if s.T >= j.cut {
		js.stats.OwnerDrops++
		return out
	}
	js.change(j)
	if j.anyPrev {
		j.prev, j.anyPrev = src, false
	}
	if j.prevKnown && s.Meta.Kind == core.KindTrain && j.prevMeta.Kind == core.KindTrain {
		if s.Meta.Carriages == 0 {
			s.Meta.Carriages = j.prevMeta.Carriages
		}
		if s.Meta.Kinds == "" {
			s.Meta.Kinds = j.prevMeta.Kinds
		}
		if s.Meta.Color == "" {
			s.Meta.Color = j.prevMeta.Color
		}
	}
	j.prevKnown, j.prevSfc, j.prevMeta = true, s.Surface, s.Meta
	js.stats.LateKept++
	return append(out, s)
}

// Adopt makes src the owner of journey id (the "adopt" message) at t, the adopt's
// time, and returns the previous owner ("" when none). The previous owner's samples
// from before t and its gone are still taken when they arrive later (Move); when
// Journeys did not know the journey, those of the first other source that sends any.
func (js *Journeys) Adopt(src, id string, t core.TimeMs) string {
	j := js.get(id)
	if j == nil {
		j = js.add(id)
	}
	js.change(j)
	prev := j.owner
	switch {
	case prev == src:
	case prev != "" && j.known:
		j.prev, j.anyPrev, j.prevKnown, j.prevSfc, j.prevMeta, j.cut = prev, false, true, j.sfc, j.meta, t
		j.afterBy = ""
	case prev == "" && j.goneBy == "" && !j.known:
		// unknown: forgotten by the bound, or the server restarted since its last sample
		j.prev, j.anyPrev, j.prevKnown, j.cut = "", true, false, t
		j.afterBy = ""
	default:
		// the previous owner's gone came first: nothing of it is still to come
		j.prev, j.anyPrev = "", false
	}
	j.owner, j.goneBy = src, ""
	return prev
}

// Owner returns the owner of journey id ("" when none or unknown).
func (js *Journeys) Owner(id string) string {
	if e := js.byID[id]; e != nil {
		return e.Value.(*journey).owner
	}
	return ""
}

// Begin starts a resync of src: Resynced will end the journeys it does not re-send.
func (js *Journeys) Begin(src string) {
	js.changeSeen(src)
	js.seen[src] = map[string]bool{}
}

// Resynced ends a resync of src at t: every journey src owns that it did not sample
// since Begin gets a gone, appended to out.
func (js *Journeys) Resynced(src string, t core.TimeMs, out []core.MoverSample) []core.MoverSample {
	seen := js.seen[src]
	if seen == nil {
		return out
	}
	js.changeSeen(src)
	delete(js.seen, src)
	return js.endOwned(src, t, out, func(j *journey) bool { return !seen[j.id] })
}

// Stopped handles a stopped source at t: its players are gone; its trains and vehicles
// stay parked (illisis §7.3).
func (js *Journeys) Stopped(src string, t core.TimeMs, out []core.MoverSample) []core.MoverSample {
	js.changeSeen(src)
	delete(js.seen, src)
	return js.endOwned(src, t, out, func(j *journey) bool { return strings.HasPrefix(j.id, "p:") })
}

func (js *Journeys) endOwned(src string, t core.TimeMs, out []core.MoverSample, match func(*journey) bool) []core.MoverSample {
	var ended []*journey
	for e := js.lru.Front(); e != nil; e = e.Next() {
		j := e.Value.(*journey)
		if j.owner == src && j.known && match(j) {
			ended = append(ended, j)
		}
	}
	for _, j := range ended {
		js.change(j)
		out = append(out, goneOf(j, t, 0, core.FlagNoTick))
		j.owner = ""
		j.lastT = t
	}
	return out
}

// Len returns the number of tracked journeys.
func (js *Journeys) Len() int { return js.lru.Len() }

// Stats returns the counters.
func (js *Journeys) Stats() JourneyStats { return js.stats }
