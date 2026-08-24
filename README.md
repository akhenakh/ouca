# ouca

Cheap reverse geocoding and map matching library for Go, it uses public map tiles as a data source.

- **`Reverse`** — given a latitude/longitude, returns the closest street, its
  distance and bearing from the query point, and the nearest road intersection.
- **`MatchPath`** — given a GPS trace, returns the most likely matched road
  path (HMM/Viterbi), snapping each point to the road network and following
  street topology.

Roads are read on demand from [OpenMapTiles](https://openmaptiles.org/) vector
tiles served by [OpenFreeMap](https://www.openfreemap.org) (default provider:
`https://tiles.openfreemap.org/planet`, zoom 14 ≈ 9.5 m/pixel), decoded with
[github.com/akhenakh/mvtgo](https://github.com/akhenakh/mvtgo). All geometry
and projections use
[github.com/peterstace/simplefeatures](https://github.com/peterstace/simplefeatures)
(`geom` + `carto.WebMercator`).

## How it works

1. The query point is located in its containing tile at zoom 14; tiles are
   downloaded and decoded lazily, expanding to neighboring rings only if the
   best match is still far away.
2. Every road segment of the `transportation` and `transportation_name` layers
   is projected against the query point (point-to-segment projection); the
   closest one wins. Named/drivable roads are preferred over paths and tracks,
   with a fallback when nothing better exists.
3. Road endpoints shared by two or more named roads are indexed as
   intersections; the nearest one within ~150 m is reported with the crossing
   street names.
4. All coordinates live in the `carto.WebMercator` space (global tile units);
   distances are converted to ground meters with the local scale factor
   (cos φ · EarthCircumference / 2^z).

### Map matching

`MatchPath` decodes the most likely road path of a noisy GPS trace with a
Hidden Markov Model and Viterbi decoding:

- **Emission cost** — squared distance to candidate roads (σ = expected GPS
  noise) plus per-class penalties per transport mode (`car`, `bike`, `walk`).
- **Transition cost** — disagreement between GPS step length and route
  distance (β), penalties for switching streets, topologically impossible
  jumps through city blocks, and one-way violations for cars.
- Unmatched points fall back to raw GPS positions so traces stay connected;
  the returned path interpolates along matched roads and bridges connected
  streets at junctions.

## The matching algorithm in detail

Unlike traditional map-matching engines (OSRM, Valhalla) that require
downloading and compiling massive routing graphs, ouca works directly on the
vector tiles themselves. There is no routing graph: topology awareness is
recovered mathematically, and the optimal path over the *whole* trace is
computed globally in a single Viterbi pass — `O(N · K²)` for `N` GPS points
and up to `K = 32` candidates each — guaranteeing the lowest-cost trajectory
for the given trace without windowing heuristics.

1. **Coordinate transformation** — WGS84 input points are projected with
   `carto.WebMercator` into a continuous global tile grid at zoom z (14 by
   default). Because the space is continuous across tile borders, vehicles
   crossing tile boundaries do not break the match.
2. **Candidate selection** — for each GPS point, roads from the loaded tiles
   within `SearchRadius` are collected. Each candidate is a true
   segment projection (dot-product point-on-segment math, clamped to the
   endpoints), not just the nearest vertex; candidates carry the road's
   feature ID, name, class, one-way flag and the snapped vertex index.
   Candidates are ranked by emission cost and truncated to the top 32.
   Multi-part geometries are split into one road per connected part, and
   features from layers with unusable ids (the `transportation` layer emits
   the same id for every feature) get synthetic unique ids, so feature
   identity — and with it the turn/gap logic — stays reliable.
3. **Emission cost** — negative log-likelihood of a Gaussian centered on the
   candidate:
   `0.5 · (distance / σ)² + classPenalty`
4. **Transition cost** — moving between candidates costs:
   - **Distance delta**: `|gpsStep − routeStep| / β` — the straight-line GPS
     step vs the straight line between the two snapped points must agree.
   - **Turn penalty**: a flat cost when consecutive candidates sit on
     different features with different street names, discouraging zig-zagging
     at intersections (MVT tiles provide no routing graph to forbid turns).
   - **Topological gap rejection**: the physical gap between two disconnected
     geometries is measured; if it greatly exceeds the distance actually
     driven, the transition is near-absolute rejected (`WrongWayPenalty`),
     preventing shortcuts through building blocks.
   - **One-way awareness** (`Mode: "car"`): driving against a segment whose
     `oneway` property is set adds `WrongWayPenalty`.
5. **Viterbi decoding & backtracking** — the lowest-cost state sequence is
   recovered by backpointers. Consecutive snaps on the same road are expanded
   along the real geometry; snaps on connected roads are bridged through the
   shared junction; unbridgeable pairs keep a minimal straight connector so
   no spikes are ever emitted. Finally, out-and-back excursions (the path
   briefly entering a side road and returning to a visited point) are
   spliced out, keeping the emitted geometry monotonic.

**Graceful degradation:** if the vehicle enters a tunnel or private lot and
no road data exists within the search radius, a "virtual road" candidate is
created directly at the GPS position with a high penalty. The Markov chain
never breaks and every input point keeps exactly one output point.

## Install

```sh
go get github.com/akhenakh/ouca
```

## Usage

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/akhenakh/ouca"
)

func main() {
	ix := ouca.NewIndex()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Times Square, NYC
	addr, err := ix.Reverse(ctx, 40.7580, -73.9855)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("street:     %s (%s)\n", addr.Street, addr.Class)
	fmt.Printf("distance:   %.1f m\n", addr.Distance)
	fmt.Printf("snapped at: %.6f, %.6f\n", addr.Lat, addr.Lng)
	fmt.Printf("bearing:    %.0f°\n", addr.Bearing)

	if addr.Intersection != nil {
		fmt.Printf("nearest intersection: %s @ %.0f m\n",
			addr.Intersection.Streets, addr.Intersection.Distance)
	}
}
```

Output:

```
street:     7th Avenue (secondary)
distance:   0.6 m
snapped at: 40.758002, -73.985506
bearing:    209°
```

## Options

```go
ix := ouca.NewIndex(
	ouca.WithZoom(14),                                        // tile zoom level (default 14)
	ouca.WithProvider("https://tiles.openfreemap.org/planet"), // TileJSON URL or {z}/{x}/{y}.pbf template
	ouca.WithHTTPTimeout(30*time.Second),
	ouca.WithMaxRings(1), // up to 3x3 tiles searched around the point

	// On-disk tile cache (enabled by default at ~/.cache/ouca).
	ouca.WithCacheDir("/var/cache/ouca"),   // custom location
	ouca.WithCacheTTL(7*24*time.Hour),      // expiry; <= 0 disables expiry
	// ouca.WithoutCache(),                 // or disable it entirely
)
```

### Tile cache

Downloaded tiles are cached on disk (default `~/.cache/ouca`) and reused
across runs, so repeated lookups in the same area cost no additional network
traffic. The cache is designed to be **shared between applications**: every
tile is written to a temporary file and atomically renamed into place, so
concurrent writers from separate processes never corrupt a reader — partial
tiles are impossible and no locks are required. Cache keys are SHA-256 hashes
of the resolved tile URLs, and entries expire after two weeks by default.

## Map matching a GPS trace

```go
ix := ouca.NewIndex()

// A noisy drive in Berlin: south on Ebertstraße, then east onto
// Straße des 17. Juni.
trace := []ouca.LatLng{
	{Lat: 52.5172, Lng: 13.3769},
	{Lat: 52.5168, Lng: 13.3773},
	{Lat: 52.5164, Lng: 13.3771}, // noise
	{Lat: 52.5160, Lng: 13.3775},
	{Lat: 52.5157, Lng: 13.3781}, // turn east
	{Lat: 52.5157, Lng: 13.3790},
}

match, err := ix.MatchPath(ctx, trace, nil) // nil = defaults for "car"
if err != nil {
	log.Fatal(err)
}

for i, p := range match.Points {
	fmt.Printf("%d: %-15s %.0fm matched=%v\n", i, p.Street, p.Distance, p.Matched)
}
fmt.Printf("matched path: %d vertices, %.0f m\n", len(match.Path), match.Length)
```

Output:

```
0: Ebertstraße       14m matched=true
1: Ebertstraße        3m matched=true
2: Ebertstraße       20m matched=true
3: Ebertstraße       17m matched=true
4:                   11m matched=true
5: Pariser Platz     21m matched=true
matched path: 38 vertices, 285 m
```

Tune the matcher per transport mode and sensor quality:

```go
opts := ouca.DefaultPathOptions("bike")
opts.SearchRadius = 30 // meters
opts.Sigma = 8         // expected GPS noise
match, err := ix.MatchPath(ctx, trace, &opts)
```

### Defaults (`DefaultPathOptions`)

| Option | Default | Meaning |
|---|---|---|
| `SearchRadius` | 50 m | candidate radius around each point |
| `Sigma` | 10 m | expected GPS noise standard deviation |
| `Beta` | 15 m | route/GPS distance disagreement scale |
| `TurnPenalty` | 25 | flat cost for switching streets |
| `GapPenalty` | 5 | multiplier for routing gaps between roads |
| `WrongWayPenalty` | 100000 | near-absolute rejection (wrong way, impossible jumps) |

### Class penalties

The HMM penalizes classes a given transport mode is unlikely to travel on
(see `ouca.ClassPenalties(mode)`); unknown classes get a small default
penalty. Highlights for `"car"`:

| Class | Penalty |
|---|---|
| primary / secondary / residential / motorway / trunk | 0 |
| living_street | 2 |
| service | 5 |
| parking_aisle / driveway | 8 |
| track | 10 |
| path / footway / steps / pedestrian / cycleway / corridor | 100 |

`"bike"` and `"walk"` invert the scheme (cycleways and footpaths become
cheap, motorways prohibitive).

## Academic foundation

The matching engine is heavily inspired by:

> **An adaptive Markov chain algorithm applied over map-matching of vehicle
> trip GPS data**
> *Bilge Kaan Karamete, Louai Adhami & Eli Glaser (2021), Geo-spatial
> Information Science*
> [DOI: 10.1080/10095020.2020.1866956](https://doi.org/10.1080/10095020.2020.1866956)

Where the paper discusses adaptive sliding windows over expensive
shortest-path networks, ouca's Viterbi implementation evaluates the entire
trace globally in one pass, which is feasible here because candidates are
capped per point.

## Result

### `Reverse` → `Address`

| Field | Description |
|---|---|
| `Street` | street name (from `transportation_name`) |
| `Ref` | road reference number (e.g. `A1`) |
| `Class` / `Subclass` | OpenMapTiles road class / subclass |
| `Distance` | meters from the query point to the road |
| `Lat`, `Lng` | snapped position on the road |
| `Bearing` | bearing of the matched segment, degrees clockwise from north |
| `RoadsNear` | distinct named roads within 2x the best distance (ambiguity gauge) |
| `Intersection` | nearest junction of ≥2 named roads, if any |

### `MatchPath` → `PathMatch`

| Field | Description |
|---|---|
| `Points[].Street`, `.Class` | road each input point was matched to |
| `Points[].Distance` | meters from the GPS point to the road |
| `Points[].Matched` | false if no road within `SearchRadius` (raw GPS kept) |
| `Path` | matched polyline, including intermediate road vertices and junction bridges |
| `Length` | length of `Path` in meters |

## License

MIT
