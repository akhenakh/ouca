// Package ouca provides reverse geocoding and path map-matching against
// vector tiles: given a latitude/longitude it returns the closest street,
// its geometry, distance, bearing and the nearest road intersection; given a
// GPS trace it returns the most likely matched road path (HMM/Viterbi).
//
// Tiles are downloaded from an OpenMapTiles-compatible provider (default:
// https://tiles.openfreemap.org/planet) at zoom level 14 and decoded with
// mvtgo. All geometric computations use peterstace/simplefeatures, with the
// carto.WebMercator projection as the working coordinate space.
package ouca

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/peterstace/simplefeatures/geom"
)

// Intersection describes a point where two or more named roads meet.
type Intersection struct {
	Lat      float64  `json:"lat"`
	Lng      float64  `json:"lng"`
	Distance float64  `json:"distance_meters"` // meters from the query point
	Streets  []string `json:"streets"`         // names of roads meeting here
}

// Address is the closest addressable map element for a position.
type Address struct {
	Street       string        `json:"street,omitempty"`
	Ref          string        `json:"ref,omitempty"`
	Class        string        `json:"class,omitempty"`
	Subclass     string        `json:"subclass,omitempty"`
	Distance     float64       `json:"distance_meters"` // meters from query to the road
	Lat          float64       `json:"lat"`             // snapped latitude
	Lng          float64       `json:"lng"`             // snapped longitude
	Bearing      float64       `json:"bearing_degrees"` // bearing of the matched segment
	RoadsNear    int           `json:"roads_near"`      // distinct named roads nearby
	Intersection *Intersection `json:"intersection,omitempty"`
}

// Index indexes roads decoded from vector tiles for reverse geocoding and
// map matching. It is safe for concurrent use.
type Index struct {
	zoom    int
	client  *tileClient
	maxRing int

	mu       sync.Mutex
	resolved bool
	tiles    map[uint64]*tileData
	nodes    map[string]*node // intersection candidates keyed by rounded tile coords
}

type tileData struct {
	z, x, y int
	roads   []*road
}

// road is a street geometry in global Web Mercator tile units.
type road struct {
	id       uint64
	name     string
	ref      string
	class    string
	subclass string
	oneway   bool
	line     geom.LineString
}

type node struct {
	x, y    float64
	streets map[string]struct{}
	count   int
}

// Option configures an Index.
type Option func(*Index)

// WithZoom sets the tile zoom level used for matching (default 14).
func WithZoom(z int) Option {
	return func(ix *Index) {
		if z >= 0 {
			ix.zoom = z
		}
	}
}

// WithProvider sets an OpenMapTiles-compatible tile provider URL
// (TileJSON URL or a {z}/{x}/{y}.pbf template).
func WithProvider(u string) Option {
	return func(ix *Index) { ix.client.tileURL = u }
}

// WithHTTPTimeout sets the HTTP timeout for tile downloads.
func WithHTTPTimeout(d time.Duration) Option {
	return func(ix *Index) { ix.client.http.Timeout = d }
}

// WithMaxRings sets how many rings of neighboring tiles may be loaded when
// expanding the search area (ring 0 = 1 tile, ring 2 = 5x5 tiles).
func WithMaxRings(r int) Option {
	return func(ix *Index) {
		if r >= 0 {
			ix.maxRing = r
		}
	}
}

// NewIndex creates a new reverse geocoder and map matcher.
func NewIndex(opts ...Option) *Index {
	ix := &Index{
		zoom:    DefaultZoom,
		client:  newTileClient(DefaultProvider, 30*time.Second),
		maxRing: 1,
		tiles:   make(map[uint64]*tileData),
		nodes:   make(map[string]*node),
	}
	for _, o := range opts {
		o(ix)
	}
	return ix
}

// resolve ensures the tile URL template has been resolved from the provider
// TileJSON. Callers must hold ix.mu.
func (ix *Index) resolve(ctx context.Context) error {
	if ix.resolved {
		return nil
	}
	if err := ix.client.resolveTileJSON(ctx); err != nil {
		return err
	}
	ix.resolved = true
	return nil
}

// Reverse returns the closest address for the given position.
func (ix *Index) Reverse(ctx context.Context, lat, lng float64) (*Address, error) {
	if err := validateLatLng(lat, lng); err != nil {
		return nil, err
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if err := ix.resolve(ctx); err != nil {
		return nil, err
	}

	wm := webMercator(ix.zoom)
	q := wm.Forward(geom.XY{X: lng, Y: lat})
	mpt := metersPerTileAtLat(lat, ix.zoom)
	tx, ty := int(math.Floor(q.X)), int(math.Floor(q.Y))

	bestPreferred := newBestMatch(q.X, q.Y)
	bestAny := newBestMatch(q.X, q.Y)
	ring := 0
	const expandThreshold = 250.0 // meters: keep expanding until close enough
	for {
		if err := ix.loadRing(ctx, tx, ty, ring); err != nil {
			return nil, err
		}
		roads := ix.roadsInRing(tx, ty, ring)
		for _, r := range roads {
			if isPreferredRoad(r) {
				bestPreferred.search([]*road{r})
			}
			bestAny.search([]*road{r})
		}
		closeEnough := bestPreferred.found && bestPreferred.meters*mpt <= expandThreshold
		allDone := bestAny.found && bestAny.meters*mpt <= expandThreshold && !bestPreferred.found
		if closeEnough || allDone || ring >= ix.maxRing {
			break
		}
		ring++
	}

	const preferredFallback = 200.0 // meters: max distance at which a preferred road wins
	var best *bestMatch
	switch {
	case bestPreferred.found && bestPreferred.meters*mpt <= preferredFallback:
		best = bestPreferred
	case bestAny.found:
		best = bestAny
	case bestPreferred.found:
		best = bestPreferred
	default:
		return nil, fmt.Errorf("no road found near (%f,%f)", lat, lng)
	}

	// Street names live in the transportation_name layer; if we matched an
	// unnamed feature, adopt the name of the closest named duplicate.
	if best.road.name == "" && best.road.ref == "" {
		const maxDonorMeters = 15.0
		best.road.name, best.road.ref = ix.nearestRoadName(best.point, maxDonorMeters/mpt)
	}

	snappedLat, snappedLng := tileLatLng(wm, best.point.X, best.point.Y)
	addr := &Address{
		Street:    best.road.name,
		Ref:       best.road.ref,
		Class:     best.road.class,
		Subclass:  best.road.subclass,
		Distance:  best.meters * mpt,
		Lat:       snappedLat,
		Lng:       snappedLng,
		Bearing:   bearingDeg(best.segStart, best.segEnd),
		RoadsNear: ix.countRoadsNear(tx, ty, ring, q, best.meters, mpt),
	}
	addr.Intersection = ix.nearestIntersection(q, mpt)
	return addr, nil
}

// nearestRoadName adopts the name/ref of the closest named road geometry to
// the given point (transportation and transportation_name are separate layers
// describing the same roads). maxUnits bounds the search in tile units.
func (ix *Index) nearestRoadName(p geom.XY, maxUnits float64) (string, string) {
	var name, ref string
	bestDist := maxUnits
	for _, t := range ix.tiles {
		for _, r := range t.roads {
			if r.name == "" && r.ref == "" {
				continue
			}
			d, proj, _, _, _ := closestOnLineString(p.X, p.Y, r.line)
			if d < bestDist && dist(proj, p) <= maxUnits {
				name, ref = r.name, r.ref
				bestDist = d
			}
		}
	}
	return name, ref
}

func dist(a, b geom.XY) float64 {
	return math.Hypot(a.X-b.X, a.Y-b.Y)
}

// nearestIntersection finds the closest junction of 2+ named roads to the
// query point within ~150m.
func (ix *Index) nearestIntersection(q geom.XY, mpt float64) *Intersection {
	const maxDistMeters = 150.0
	var (
		best     *node
		bestDist float64
	)
	for _, n := range ix.nodes {
		if len(n.streets) < 2 {
			continue
		}
		d := math.Hypot(n.x-q.X, n.y-q.Y) * mpt
		if d <= maxDistMeters && (best == nil || d < bestDist) {
			best, bestDist = n, d
		}
	}
	if best == nil {
		return nil
	}
	streets := make([]string, 0, len(best.streets))
	for s := range best.streets {
		streets = append(streets, s)
	}
	sort.Strings(streets)
	lat, lng := tileLatLng(webMercator(ix.zoom), best.x, best.y)
	return &Intersection{
		Lat:      lat,
		Lng:      lng,
		Distance: bestDist,
		Streets:  streets,
	}
}

// countRoadsNear counts distinct named roads whose distance is within 2x the
// best match distance — useful to gauge ambiguity of the position fix.
func (ix *Index) countRoadsNear(tx, ty, ring int, q geom.XY, bestUnits, mpt float64) int {
	limit := math.Max(bestUnits*2, bestUnits+100/mpt)
	names := make(map[string]struct{})
	for _, t := range ix.tilesInRange(tx, ty, ring) {
		for _, r := range t.roads {
			d := distToLineString(q.X, q.Y, r.line)
			if d <= limit && (r.name != "" || r.ref != "") {
				key := r.name
				if key == "" {
					key = r.ref
				}
				names[key] = struct{}{}
			}
		}
	}
	return len(names)
}

// loadRing ensures all tiles up to chebyshev distance `ring` around (tx,ty)
// are indexed. Already-loaded tiles are skipped by loadTile.
func (ix *Index) loadRing(ctx context.Context, tx, ty, ring int) error {
	for dx := -ring; dx <= ring; dx++ {
		for dy := -ring; dy <= ring; dy++ {
			if err := ix.loadTile(ctx, tx+dx, ty+dy); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ix *Index) loadTile(ctx context.Context, x, y int) error {
	key := tileID(ix.zoom, x, y)
	if _, ok := ix.tiles[key]; ok {
		return nil
	}
	n := int(1) << uint(ix.zoom)
	wx, wy := wrap(x, n), wrap(y, n)
	layers, err := ix.client.fetchTile(ctx, ix.zoom, wx, wy)
	if err != nil {
		return err
	}
	td := &tileData{z: ix.zoom, x: wx, y: wy}
	for _, layer := range layers {
		switch layer.Name {
		case "transportation", "transportation_name":
		default:
			continue
		}
		extent := float64(layer.Extent)
		if extent == 0 {
			extent = defaultExtent
		}
		for _, f := range layer.Features {
			g := f.Geometry
			if !g.IsLineString() && !g.IsMultiLineString() {
				continue
			}
			seq := g.DumpCoordinates()
			nCoords := seq.Length()
			vals := make([]float64, 0, nCoords*2)
			for i := 0; i < nCoords; i++ {
				p := seq.GetXY(i)
				vals = append(vals,
					float64(wx)+p.X/extent, // global tile units (carto.WebMercator space)
					float64(wy)+p.Y/extent,
				)
			}
			if len(vals) < 4 {
				continue
			}
			r := &road{
				id:       f.ID,
				name:     asString(f.Properties["name"]),
				ref:      asString(f.Properties["ref"]),
				class:    asString(f.Properties["class"]),
				subclass: asString(f.Properties["subclass"]),
				oneway:   asBool(f.Properties["oneway"]),
				line:     geom.NewLineString(geom.NewSequence(vals, geom.DimXY)),
			}
			if layer.Name == "transportation_name" && r.class == "" {
				r.class = "road"
			}
			td.roads = append(td.roads, r)
			ix.indexEndpoints(r)
		}
	}
	ix.tiles[key] = td
	return nil
}

// indexEndpoints records both ends of a named road as potential intersections.
func (ix *Index) indexEndpoints(r *road) {
	if r.name == "" && r.ref == "" {
		return
	}
	seq := r.line.Coordinates()
	first := seq.GetXY(0)
	last := seq.GetXY(seq.Length() - 1)
	for _, p := range []geom.XY{first, last} {
		k := nodeKey(p)
		n, ok := ix.nodes[k]
		if !ok {
			n = &node{x: p.X, y: p.Y, streets: make(map[string]struct{})}
			ix.nodes[k] = n
		}
		n.count++
		if r.name != "" {
			n.streets[r.name] = struct{}{}
		} else if r.ref != "" {
			n.streets[r.ref] = struct{}{}
		}
	}
}

// roadsInRing returns roads from tiles in rings 0..ring (deduplicated).
func (ix *Index) roadsInRing(tx, ty, ring int) []*road {
	var roads []*road
	seen := make(map[*tileData]struct{})
	for _, t := range ix.tilesInRange(tx, ty, ring) {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		roads = append(roads, t.roads...)
	}
	return roads
}

func (ix *Index) tilesInRange(tx, ty, ring int) []*tileData {
	var out []*tileData
	for dx := -ring; dx <= ring; dx++ {
		for dy := -ring; dy <= ring; dy++ {
			if t, ok := ix.tiles[tileID(ix.zoom, tx+dx, ty+dy)]; ok {
				out = append(out, t)
			}
		}
	}
	return out
}

func validateLatLng(lat, lng float64) error {
	if lat < -85.05112878 || lat > 85.05112878 || lng < -180 || lng >= 180 {
		return fmt.Errorf("lat/lng out of range: %f,%f", lat, lng)
	}
	return nil
}

func tileID(z, x, y int) uint64 {
	return uint64(z)<<40 | uint64(uint32(x))<<20 | uint64(uint32(y))
}

func wrap(v, n int) int {
	return ((v % n) + n) % n
}

func nodeKey(p geom.XY) string {
	// Quantize to ~1 tile unit to tolerate duplicate encoding across layers.
	return strconv.FormatInt(int64(math.Round(p.X)), 10) + ":" +
		strconv.FormatInt(int64(math.Round(p.Y)), 10)
}

func asString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case string:
		return t == "yes" || t == "true" || t == "1"
	}
	return false
}
