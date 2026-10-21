# Rail geometry: where a train runs

Factorio rails are a handful of fixed piece shapes, and rolling stock sits on them by fixed
rules, so a train's path and every carriage on it follow from the pieces it runs over and one
number: how far along them its leading joint is. This document gives those shapes and rules
for Factorio 2.x, so the capture (Lua), the server (Go) and the viewer (TS) can share one model.

Sources: the Factorio C++ source (the owner's checkout at commit `81138523`, 2026-10-17; paths
below are relative to its root, cited as `file:line`), checked against carriages recorded in
Factorio 2.1.19 on real maps (`bench/railbench`, numbers in section 6). The formulas here are
written from the source's math; no code was copied. The reference implementation is
`bench/railbench/railgeom.mjs`; the shared test vectors are
[rail-geometry-vectors.json](rail-geometry-vectors.json) (section 7).

## 1. Conventions

- Map coordinates in tiles, x east, y south. Directions are the 16-way `defines.direction`
  values d (0 north, 4 east, 8 south, 12 west). Orientations are Factorio's, in turns (0
  north, 0.25 east); the unit vector of orientation o is (sin 2πo, -cos 2πo).
- The odd directions are *not* 22.5-degree steps for rails: a half-diagonal piece runs at
  atan(1/2) = 26.565 degrees off its axis (section 2.3).
- A piece has a front end and a back end (`LuaEntity.get_rail_end(defines.rail_direction.front
  / back)`). Its path is P(s) for s from 0 at the front end to the piece's length L at the back
  end. s is the game's *rail distance*: the quantity a rail joint moves by when a train moves
  by its speed (section 3). It is the plan arc length on every piece except curved-rail-b
  (within 2%) and the rail ramp (section 2.6).
- Every piece below is given for the piece at the origin built facing north (direction 0);
  P is the offset from the entity's position. Section 2.1 says how the other directions follow.

## 2. Piece shapes

### 2.1 Symmetry

Curved and half-diagonal pieces use one canonical shape and eight placements. With
i = (d >> 1) & 7 (`Direction::getIndexOf8`, `src/Util/Direction.hpp:70`), apply in this
order: if bit 0 is set, mirror x (x -> -x); if bit 1 is set, turn a quarter clockwise
((x, y) -> (-y, x)); if bit 2 is set, turn half round ((x, y) -> (-x, -y))
(`src/Entity/RailPrototype.cpp:1158-1169` for curved-rail-a, `:1306-1317` for curved-rail-b,
`:1465-1471` for half-diagonal-rail, which uses bits 0 and 1 only: its pieces have directions
0, 2, 4 and 6). The legacy curved rail uses the same eight placements (its turn table,
`RailPrototype.cpp:775-782`, is the canonical turn under them).

Elevated pieces have exactly the shapes of the ground ones (`elevated-straight-rail`,
`elevated-half-diagonal-rail`, `elevated-curved-rail-a/b` derive from the ground prototypes,
`src/Entity/RailPrototype.hpp:533-600`).

### 2.2 straight-rail

`RailPrototype.cpp:1013-1047`. Length 2 for d = 0 or 4, 2√2 for d = 2 or 6 (the directions a
straight piece is built with). Orthogonal (d >> 1 even): P(s) = (0, s - 1). Diagonal: P(s) =
(-1 + s/√2, 1 - s/√2). Then, when (d >> 2) & 1 (`getIndexOf4() & 1`), turn a quarter clockwise.
The game rounds the offset to its 1/256 grid.

### 2.3 half-diagonal-rail

`RailPrototype.cpp:1449-1473`, length 2√5 = 4.472135955 (`RailPrototype.hpp:485`):
P(s) = (1, 2) (1 - s/√5), a straight from (1, 2) to (-1, -2) at 26.565 degrees off north.

### 2.4 curved-rail-a

Reference function: `tests/TestRail.cpp:1163-1176` (the "true position function" the graphics
use); the game evaluates a cubic polynomial fit of it within 0.01 tile
(`RailPrototype.cpp:1142-1171`, test bound at `TestRail.cpp:1215-1221`). A circular arc of
radius R = 13 (`RailPrototype.hpp:424`), length 5.132284556 = 13 atan(5/12):

    P(s) = (13 (cos(s/13) - 1), 2 - 13 sin(s/13))

from (0, 2) heading north to (-1, -3), where it heads 22.620 degrees west of north
(atan(5/12)), centre (-13, 2).

### 2.5 curved-rail-b

Reference function: `tests/TestRail.cpp:1224-1240`; the game again evaluates a cubic fit
within 0.01 tile (`RailPrototype.cpp:1290-1319`). Length 5.077891568 (`RailPrototype.hpp:454`).
It continues curved-rail-a's angle around the fixed centre (-11, 7) while its radius shrinks
linearly from 13 to 9√2 = 12.728:

    θ(s) = (5.132284556 + s) / 13
    r(s) = 13 + (s / 5.077891568) (9√2 - 13)
    P(s) = (-11 + r cos θ, 7 - r sin θ)

from (1, 2) to (-2, -2). It is not a circle: its heading at the front end is 25.687 degrees
west of north and 48.132 at the back end, and |dP/ds| runs from 1.0014 to 0.9805, so the plan
length is 5.0321 for 5.0779 of rail distance. A best-fit circle stays within 0.01 tile of it
(radius 12.77, section 6).

### 2.6 rail-ramp

`RailPrototype.cpp:1608-1709`. In plan a straight of 16 tiles: for d = 0 from (0, 8) to
(0, -8), turned a quarter clockwise per 4 of d. The rail distance includes the height: with
p in [0, 1] along the ramp, the height is h(p) = 3 (1 + sin(π q)) / 2 where q = clamp((p - 0.5)
16/14, -0.5, 0.5) (`RailPrototype.cpp:1615-1624`; ground 0 and elevated 3,
`src/Rail/RailLayer.hpp:39-40`), and s is the length of the curve (16 p, h(p) / 0.7071067811865)
(`RailPrototype.cpp:1626-1655`, `src/GlobalContext.hpp:280`): L = 16.762233. So equal steps of s
are up to 9.7% shorter in plan on the slope (dplan/ds = 0.903 in the middle). The game samples
the inverse mapping in a 129-entry table and interpolates linearly (`RailPrototype.cpp:1657-1695`).

### 2.7 legacy-straight-rail

`RailPrototype.cpp:705-724`, for pieces of 1.1 saves. Orthogonal (d = 0, 4): length 2, P(s) =
(1 - s) u(d), where u(d) is the unit vector of direction d. Diagonal (d = 2, 6, 10, 14): length
√2, P(s) = u(d - 2) + s u(d + 4): it starts one tile from the position, 45 degrees left of d,
and runs at right angles to d (for d = 2, from (0, -1) to (1, 0)).

### 2.8 legacy-curved-rail

`RailPrototype.cpp:750-884`. With the gauge g = 35/64 and b = √2 g 0.7071067811865 / 2 =
0.2734: radius r = (3 - b) / (1 - sin 45°) = 9.309067, sx = r + 1, sy = r sin 45° + 1 + b,
a straight part of 8 - sy = 0.144058 and length 0.144058 + 0.386699 + 2πr/8 = 7.842081
(`RailPrototype.cpp:763-772`). For d = 0:

    s < 0.144058:  P(s) = (1, 4 - s)
    otherwise:     a = (s - 0.144058) / r,  P(s) = (2 - sx + r cos a, sy - 4 - r sin a)

The arc formula also covers the last 0.3867 tiles, which the prototype calls a diagonal part,
so the piece ends heading 47.38 degrees off north and 0.011 tile from its connection point
(the source's own note, `RailPrototype.cpp:880-883`).

### 2.9 Where pieces meet

Headings of travel from the front end, in degrees west of north for the north-facing piece:

| Piece | Rail distance L | Plan length | At s = 0 | At s = L |
|---|---:|---:|---:|---:|
| straight-rail (orthogonal / diagonal) | 2 / 2.828427 | same | 0 / 45 | same |
| half-diagonal-rail | 4.472136 | 4.4721 | 26.565 | 26.565 |
| curved-rail-a | 5.132285 | 5.1323 | 0 | 22.620 |
| curved-rail-b | 5.077892 | 5.0321 | 25.687 | 48.132 |
| rail-ramp | 16.762233 | 16 | 0 | 0 |
| legacy-curved-rail | 7.842081 | 7.8421 | 0 | 47.380 |

So consecutive pieces meet with small kinks in heading: curved-rail-a to half-diagonal 3.94
degrees, half-diagonal to curved-rail-b 0.88, curved-rail-a to curved-rail-b 3.07, curved-rail-b
to a diagonal straight 3.13, legacy curve to a diagonal straight 2.38. Positions are
continuous (every piece ends exactly on its connection point, apart from the legacy curve's
0.011). A train's carriage orientation turns through each kink over one joint distance.

## 3. Rolling stock on the path

- A carriage has a front and a back joint (bogie), each a (rail, rail distance) pair whose
  position is P on that rail (`src/Rail/RailJoint.cpp:38-41`).
- The carriage's position is the midpoint of its two joints' positions and its orientation
  points from the back joint to the front joint (`src/Entity/RollingStock.cpp:350-356`). So on
  a curve the carriage centre lies inside the rail by the sagitta of the joint chord: 0.154 tile
  on radius 13 (0.157 on curved-rail-b's end, 0.214 on a legacy curve).
- Moving a carriage moves both joints by the same rail distance, crossing onto connected rails
  as needed (`RollingStock.cpp:387-460`, `RailJoint.cpp:55-67`). So a carriage's joints stay
  `joint_distance` apart in rail distance, and neighbouring carriages' facing joints
  `connection_distance` apart (set when they couple, `RollingStock.cpp:463-488`). Both come from
  the prototype (`src/Entity/RollingStockPrototype.cpp:269-270`); vanilla locomotives, cargo,
  fluid and artillery wagons all have joint_distance 4 and connection_distance 3
  (`data/base/prototypes/entity/trains.lua:511-512, 837-838, 1040-1041, 1156-1157`).
- A train moving by its speed v moves every joint by v rail distance per tick.

So with the rail path under a train as one arc-length coordinate U (pieces in order, each
running front to back or back to front), carriage j (0 at the leading end) has its front joint
at U0 - 7j and its back joint at U0 - 7j - 4 (vanilla), its centre at the midpoint and its
orientation along the chord. U0 and the path fix every carriage; U0 advances by the speed.

## 4. What Lua can read cheaply

- `LuaTrain.front_end` / `back_end`: the rail under the train's front or back end and the
  direction on it (`src/Script/LuaTrain.cpp:683-706`); `LuaTrain.get_rails()` lists the rails
  under the train; `LuaTrain.path` (automatic trains) is a `LuaRailPath` with `rails`,
  `current`, `total_distance` and `travelled_distance` (`src/Script/LuaRailPath.cpp:18-23`).
- A rail's `type`, `position`, `direction`, `get_rail_end(...)` (position and direction of each
  end, `src/Script/LuaRailEnd.cpp:20-32`) and `rail_length`.
- Carriage `position` and `orientation` (the joint midpoint and chord above). The joints'
  rail distances are not exposed.

## 5. Consequences for train samples

(What the capture and viewer now do with these findings is section 8.)

- **Heading.** The capture's sample heading `h` (second carriage towards the first) lags the
  rail tangent on curves by up to 18 degrees on 2.0 curves and 22 on legacy curves (p50 12.5
  and 15.9 on curves), so a pose built from `h` does not describe the curve. The leading
  carriage's own orientation (turned to point the way the train leads) is the tangent at its
  arc position: within 0.4 degrees p50 and 3.5 degrees max of the rail tangent (the excess is
  the kinks of 2.9).
- **The path is fixed by the pieces.** With the piece sequence between two samples, the head
  path is exact and only the timing needs samples (`simulate.mjs` rule `railpath`).
  A viewer with the rail layer can find the pieces itself: the shortest directed rail path
  between the pieces under two consecutive samples was the true path for all but 4 of 25,033
  sample pairs on the four maps (`simulate.mjs` rule `matched`); a rail distance travelled per
  sample (or speed and time) detects the rest.
- **From poses alone.** Between (position, tangent) pairs, a line/arc/line path of radius
  √(13² - 2²) = 12.85 (the carriage centre's radius on a curve) follows the curves to within
  0.28 tile p99 (simulate.mjs rule `railshape`), but the pieces' kinks and curved-rail-b's
  shrinking radius leave short gaps no such path fits (13% of sample pairs on the benchmark
  map), so poses alone need 60 to 80% more samples than the piece sequence on the four maps.

## 6. Checked against the game

`node bench/railbench/geometry.mjs --run <run>` (`bench/railbench/README.md`) on 10 minutes of
four real maps recorded tick by tick in Factorio 2.1.19: the 2.0 benchmark map
`benchmark_bizzlebazzle` (Space Age; ground, elevated and ramp pieces; 74 moving trains), the
2.0 map `Aron 2025`, and the 1.1 maps `l1n3 train smelter` and `clusterio_2.0_1` (legacy
pieces). Distances in tiles; a carriage's joints are first put at its centre ± 2 along its
axis, then moved along the rail until the model's carriage centre lies abreast of the
recorded one.

| Check | Result |
|---|---|
| Every piece's P(0) and P(L) against the ends the game reports (35,800 + 3,950 + 54,166 + 4,398 pieces) | exact (< 1e-6), except the legacy curve's far end: 0.011 (2.8) |
| Model length against `rail_length` | within 0.0004 (`rail_length` is reported in 1/1000) |
| Joints' distance from the model curve, 2.0 curves / half-diagonal / ramp / straight | max 0.012 / 0.007 / 0.003 / 0.009; p99 0.008 / 0.0045 / 0.002 / 0.003 |
| The same, legacy curve / legacy straight | max 0.008 / 0.011, p99 0.006 / 0.003 |
| The same against a quadratic Bezier through the ends (the old classifier) | curved-rail-b max 0.046, legacy curve 0.037 |
| Carriage centre off the rail centre line on curves | 2.0 p50 0.152, max 0.21; legacy p50 0.213, max 0.221 |
| Rail distance moved per tick / speed, curved-rail-b and ramp | p50 1.0013 and 0.9996 (plan: 0.990 and 0.930) |
| Carriage centre from its own front joint with the back joint 4 behind | max 0.0064 (all types) |
| Front joint to the next carriage's front joint | 7 within 0.012 |
| **Every carriage placed from the leading carriage's front joint and the rail path** | **p99 0.006, max 0.014 tile; orientation max 0.12 degrees** (benchmark map, 342,969 carriage placements) |

The game's own polynomial fits (0.01) and its 1/256 grid (0.004) account for the residuals.

## 7. Test vectors

[rail-geometry-vectors.json](rail-geometry-vectors.json) (`node bench/railbench/gen-vectors.mjs`):

- `pieces`: for every rail type and direction seen on the four maps, a real piece (`type`,
  `dir`, `x`, `y`), its `length` and the game's `game_length`, the `front_end` and `back_end`
  the game reported as `[x, y, direction]`, and `points`: `[s, x, y, orientation of the
  tangent towards growing s]` at nine rail distances from the model.
- `trains`: carriages as the game placed them (`game_carriages`: `[x, y, orientation]` from
  the leading end, the carriage's own facing), the rail `path` under them as pieces in order
  from the rear (`rev`: run from the back end to the front end) and `u0`, the leading
  carriage's front joint as a rail distance along that path.
- `tolerance`: 1e-6 for points (1e-3 on ramps, whose inverse table an implementation may
  sample differently), 1e-6 for the ends, 0.02 tile and 0.25 degrees for carriages against the
  game. `bench/railbench/railgeom.test.mjs` is the reference check
  (`node --test bench/railbench/railgeom.test.mjs`).
- `timing`: the timing model of section 8.3 at chosen points.

## 8. Train paths between samples

The v3 capture and viewer describe a train by its rail path and the timing along it
(`capture/lua/core.lua`, `capture/lua/movers.lua`; `web/src/movers/railpath.ts`,
`web/src/movers/track.ts`; protocol fields `v`, `rp`, `ru` in [PROTOCOL.md](PROTOCOL.md) 3.1,
storage in [FORMAT.md](FORMAT.md) 7.4). A curve costs no samples: its pieces travel with the
next sample, and only the timing needs samples.

### 8.1 The pieces a train runs over

- The capture notes at every read the rail under the leading carriage's front joint: that
  is `LuaTrain.front_end` (`back_end` while the train leads with its back), the rail of the
  front stock's open joint (`src/Train/TrainRollingStocks.cpp:150-161`,
  `src/Script/LuaTrain.cpp:683-706`), and `LuaRailEnd.direction` is the end of that rail the
  train runs towards. Reads are at most 5 tiles of travel apart (the wait rounded down; sooner
  while accelerating: the next read is where `v w + a w^2 / 2 = 5` with the acceleration
  since the last read; at most 30 ticks), less than the shortest curved piece (5.078), so no
  curved piece passes between two reads. (Rounded up, a train at 1.33 tiles a tick was read
  every 4 ticks, 5.3 tiles, and passed a curved-rail-a unseen on the Aron map. 5 tiles rounded
  down reads about as often as the 2026-10 capture's 4 rounded up, 1.4% more on the benchmark
  map; 4 rounded down read 39% more and cost 20% more Lua time.)
- Only curved pieces and ramps are recorded (`rp` kinds 1 to 4). Between two of them the
  rails are straight. The rail is read while the leading carriage turns (its orientation
  differs from the last read's: on a straight it stays exactly the same until its front
  joint is on a curve, and the next read comes at most 5 tiles later, still on that piece),
  while the last rail read was a curved piece or ramp, and otherwise every third read. A
  ramp shows no turn, and three reads are at most 15 tiles apart, less than its 16. (A
  chain without its ramps is right in plan, but its rail distance is 0.76 tile short per
  ramp, 4.8%, and every carriage behind a ramp is drawn that far off: carriage p99 0.74
  tile on the benchmark map when only turns were read, 0.14 with every third read.) A
  piece's ends are whole-tile points (every end the game reported in section 7's vectors
  is): for the piece built facing north, front end then back end, as offset and outward
  direction,

  | Kind | Front end | Back end |
  |---|---|---|
  | curved-rail-a | (0, 2), 8 | (-1, -3), 15 |
  | curved-rail-b | (1, 2), 7 | (-2, -2), 14 |
  | legacy-curved-rail | (1, 4), 8 | (-2, -3), 14 |
  | rail-ramp | (0, 8), 8 | (0, -8), 0 |

  and the other directions by section 2.1's placement, which turns an end direction d by
  mirroring (16 - d), a quarter turn (d + 4) and a half turn (d + 8); a ramp turns a quarter
  per 4 of its direction.
- A piece continues the path when its entry end faces the path's exit end (directions 8
  apart) and lies on the exit end's ray: equal, or joined by straight rails. With the
  direction's integer step (dx, dy) (even directions the compass steps, odd ones the 1:2
  half-diagonals (1, -2), (2, -1), ...), that is `(px * dy - py * dx) = 0` and
  `px * dx + py * dy >= 0` for the offset (px, py) from the exit to the entry. A piece that
  does not continue the path means one was missed (a read came late: the capture reads at
  most 32 trains a tick). The capture then walks back from the rail under the leading joint
  over the rails under the train (`LuaRailEnd.flip_direction`, then `move_natural` up to 64
  rails: the natural rail behind each connection is the one a train last took,
  `src/Script/LuaRailEnd.cpp:176-187`, which is this train) to the last recorded rail and
  records the curved pieces it passed. Only when that fails does it send the read before
  and the current one, which begins a new path (`ru`). The rail under the train's end leads
  the leading carriage, so a short piece can pass under the end between two reads that did
  not look at the rail, before the carriage turns; when a turning carriage's read finds a
  straight rail, the read walks back the same way (at most 12 rails) to the last rail it
  looked at and records what continues the path. (Without that walk, a curved-rail-a of an
  S-bend on the Aron map was only found at the next curve 100 tiles on, and a live viewer
  drew the train off its path for 22 frames.)
- Where a new path begins (first sight, a changed train, end switch, surface change, a
  missed piece, a resync) the sample lists the curved pieces and ramps under the whole train
  instead, rear first: `LuaTrain.get_rails()` lists the rails under the train from its front
  end to its back end (`src/Script/LuaTrain.cpp:627-638`, `src/Train/RailsUnderTrainIterator.cpp`).
  A changed train (`on_train_created` after a join or a split) begins a new path at its next
  read: its leading joint may have jumped back (carriages taken from the leading end) or
  ahead (carriages coupled there), and its last read before the change is sent first.
- A stop sample lists, besides the pieces entered since the previous sample, the pieces
  under the train that came before them (in front, rear first) and any under the train's
  end after them (a piece no read looked at), but begins no new path: a viewer that has the
  samples before it already holds those pieces and skips them as resends (8.2). A parked
  train is not sampled again until it moves, so a keyframe or the live payload holds only its
  stop sample, and that sample alone must place every carriage. (With only the pieces
  entered since the previous sample, 11.7% of the parked trains on the benchmark map were
  drawn more than 2 tiles off from a keyframe, up to 69 tiles: the body was laid along the
  straight lead-in through the curves it stood on.) The path recorded for the next pieces
  then goes on from the rail under the leading joint, as after a new path.

### 8.2 The chain

The path of a run of samples (from one with `ru` to the sample before the next one) is one
chain parametrised by rail distance u:

- a lead-in line ending at the first piece's entry (u = 0 there), running back without end;
- the pieces, each run front to back or back to front (the way that connects), with straight
  lines between them;
- a lead-out line from the last piece's exit, running on without end.

The first piece's way round is the one that connects to the second; a lone piece is run so
that its exit faces the run's first sample's direction of travel. A chain without pieces is
the line through the first sample's leading joint along its heading. Each piece is placed
exactly by section 2's formulas, so the chain is the rail centre line; rolling stock follows
section 3: the carriage whose front joint is at u has its centre midway between the points
at u and u - 4, and its orientation along their chord.

Each sample of the run lies on the chain at the rail distance of its leading joint, its
centre plus 2 tiles along `h` (half the joint chord: 1.9992 on a radius-13 curve), projected
onto the chain. Samples are projected in order, each no further than 1.5 times its faster
end speed over the ticks since the previous one plus 4 tiles ahead of it, and of points
within 0.1 tile of the nearest the one nearest the rail distance the speeds predict (their
mean over the ticks). A train that comes back past a place (a loop, a balloon, a straight
used both ways) must not be put on another pass: the run's first sample lies no earlier than
the start of the last piece it lists and no further than a train length past it (with `ru`;
a heartbeat's run without), and at u <= 0 when it lists none (it is the anchor, before the
first piece appended later). A sample whose leading joint lies more than 0.35
tiles from the chain has no rail distance, and the segments touching it are drawn without
the rail path. A piece that a sample lists but the chain already holds, and that does not
continue the chain, is a resend (after a resync, or the pieces under a stopped train) and is
skipped.

The viewer takes every carriage's joint distance as 4 and the distance between neighbouring
carriages' facing joints as 3 (section 3: vanilla rolling stock), and a sample's leading
joint 2 tiles ahead of its centre in plan (about 0.2 tile short while the leading carriage
stands on a ramp's slope). Pitches measured from a run's `car` replace the 7 between
carriages; a modded carriage with other joint distances is still placed by a leading joint 2
tiles ahead of its centre, so its body is drawn off the rails by the difference (not measured:
the benchmark maps are vanilla).

When a new run begins and the previous sample's leading joint lies on the new chain behind
the run's first sample (a first sight after a resync, a missed piece whose new path reaches
back), the segment between them runs on the new chain.

### 8.3 Timing

Between two samples of a run, T ticks and L rail distance apart, with speeds va and vb
(tiles a tick, `v`), the leading joint is u(tau) ahead of the first sample tau ticks after it:

- from rest (va = 0, vb > 0) when uniform acceleration to vb covers L in less than T
  (2 L < vb T): it stands until T - tm, tm = 2 L / vb, then u = vb s^2 / (2 tm) with
  s = tau - (T - tm). A start is found by the next scan, up to 30 ticks late;
- to rest (vb = 0, va > 0) when uniform braking from va covers L in less than T: it stops at
  tm = 2 L / va, u = va s - va s^2 / (2 tm) with s = min(tau, tm), and stands. A stop is read
  at the next read, up to 30 ticks late;
- otherwise the cubic Hermite in time with the speeds as end slopes, f = tau / T:
  u = (f^3 - 2 f^2 + f) T va + (3 f^2 - 2 f^3) L + (f^3 - f^2) T vb, held within [0, L].

Uniform acceleration is exact in all three. A stop sample links to the next sample however
long the train stood (a parked train is not sampled).

### 8.4 What the capture checks

Each read keeps an odometer: the chord between consecutive reads' leading joints, lengthened
by `(1 + turn^2 / 24)` for the turn of the leading carriage between them (a chord of 4 on a
radius-13 arc is 0.016 short of it). A read is sent when a read since the last sample lies
more than 0.1 tile of rail distance from where the timing model from that sample to the
current read puts it (the previous read is then sent as a vertex: it fitted when it was
read), at least every 120 ticks (60 below game speed 0.5; 2 s of real time at speed 1, 5 s at
speed 0.2, so a live viewer's next sample is that late at worst; a read is scheduled for
when that wait ends), on the 600-tick heartbeat, on first sight, stop, start, end switch,
surface change and a missed piece that the walk back did not find, and when 32 pieces wait.
The reads kept for the check are at most 16; a full buffer keeps every other one. `car` goes
with first sight, a changed train, a surface change and resyncs only: the path places every
carriage, and `car` only tells the viewer the carriages' pitches. A stop sample lists the
pieces under the whole train (8.1).

### 8.5 Test vectors

The vectors' `timing` section lists `[L, T, va, vb, tau, u, rate]` from the reference
implementation (`bench/railbench/railgeom.mjs` `timingU`); `rate` is du/dtau (0 where
held). The `trains` vectors check a chain built from a path's curved pieces and ramps alone
(`web/src/movers/railpath.test.ts`), and the Lua tests check the end table and the
connection rule on the same paths (`capture/test/unit/core_test.lua`).
