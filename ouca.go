// Package ouca provides reverse geocoding against vector tiles: given a
// latitude/longitude it returns the closest street, its geometry, distance,
// bearing and the nearest road intersection, which can be used to infer
// positions.
//
// Tiles are downloaded from an OpenMapTiles-compatible provider (default:
// https://tiles.openfreemap.org/planet) at zoom level 14 and decoded with
// mvtgo. All geometric computations use peterstace/simplefeatures.
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

// Intersection describes a point where two or more roads meet.
type Intersection struct {
	Lat      float64  `json:"lat"`
	Lng      float64  `json:"lng"`
	Distance float64  `json:"distance_meters"` // meters from the query point
	Streets  []string `json:"streets"`         // names of roads meeting here
}

// Address is the closest addressable map element for a position.
type Address struct {
	Street       string        `json:"street,omitempty"`
	Ref          string        `json:"ref,omitempty"`      // road reference number (e.g. A1)
	Class        string        `json:"class,omitempty"`    // openmaptiles road class
	Subclass     string        `json:"subclass,omitempty"` // highway subclass
	Distance     float64       `json:"distance_meters"`    // meters from query to the road
	Lat          float64       `json:"lat"`                // snapped latitude
	Lng          float64       `json:"lng"`                // snapped longitude
	Bearing      float64       `json:"bearing_degrees"`    // bearing of the matched segment
	RoadsNear    int           `json:"roads_near"`         // number of roads considered nearby
	Intersection *Intersection `json:"intersection,omitempty"`
}

// Index indexes roads decoded from vector tiles for reverse geocoding.
type Index struct {
	zoom    int
	client  *tileClient
	maxRing int

	mu       sync.Mutex
	resolved bool
	tiles    map[uint64]*tileData
	nodes    map[string]*node // intersection candidates keyed by rounded mercator coords
}

type tileData struct {
	z, x, y int
	roads   []*road
}

type road struct {
	name     string
	ref      string
	class    string
	subclass string
	line     geom.LineString // EPSG:3857 meters
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

// NewIndex creates a new reverse geocoder.
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

// Reverse returns the closest address for the given position.
func (ix *Index) Reverse(ctx context.Context, lat, lng float64) (*Address, error) {
	if lat < -85.05112878 || lat > 85.05112878 || lng < -180 || lng >= 180 {
		return nil, fmt.Errorf("lat/lng out of range: %f,%f", lat, lng)
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if !ix.resolved {
		if err := ix.client.resolveTileJSON(ctx); err != nil {
			return nil, err
		}
		ix.resolved = true
	}

	mx := lngToMercatorX(lng)
	my := latToMercatorY(lat)
	scale := mercatorScaleAtLat(lat)

	txf, tyf := latLngToTileF(lat, lng, ix.zoom)
	tx, ty := int(math.Floor(txf)), int(math.Floor(tyf))

	bestPreferred := newBestMatch(mx, my)
	bestAny := newBestMatch(mx, my)
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
		closeEnough := bestPreferred.found && bestPreferred.meters*scale <= expandThreshold
		allDone := bestAny.found && bestAny.meters*scale <= expandThreshold && !bestPreferred.found
		if closeEnough || allDone || ring >= ix.maxRing {
			break
		}
		ring++
	}

	const preferredFallback = 200.0 // meters: max distance at which a preferred road wins
	var best *bestMatch
	switch {
	case bestPreferred.found && bestPreferred.meters*scale <= preferredFallback:
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
		ix.enrichStreetName(best, tx, ty, ring)
	}

	snappedLng := mercatorXToLng(best.point.X)
	snappedLat := mercatorYToLat(best.point.Y)
	addr := &Address{
		Street:    best.road.name,
		Ref:       best.road.ref,
		Class:     best.road.class,
		Subclass:  best.road.subclass,
		Distance:  best.meters * scale,
		Lat:       snappedLat,
		Lng:       snappedLng,
		Bearing:   bearingDeg(best.segStart, best.segEnd),
		RoadsNear: ix.countRoadsNear(tx, ty, ring, mx, my, best.meters),
	}
	addr.Intersection = ix.nearestIntersection(mx, my, scale)
	return addr, nil
}

// enrichStreetName adopts the name of the closest named road geometry to the
// snapped point (transportation and transportation_name are separate layers
// describing the same roads).
func (ix *Index) enrichStreetName(best *bestMatch, tx, ty, ring int) {
	const maxSnap = 15.0 // mercator meters
	var name, ref string
	bestDist := maxSnap
	for _, t := range ix.tilesInRange(tx, ty, ring) {
		for _, r := range t.roads {
			if r.name == "" && r.ref == "" {
				continue
			}
			d, p, _, _ := closestOnLineString(best.point.X, best.point.Y, r.line)
			if d < bestDist && dist(p, best.point) <= maxSnap {
				name, ref = r.name, r.ref
				bestDist = d
			}
		}
	}
	if name != "" || ref != "" {
		best.road.name = name
		best.road.ref = ref
	}
}

func dist(a, b geom.XY) float64 {
	return math.Hypot(a.X-b.X, a.Y-b.Y)
}

// loadRing ensures all tiles at chebyshev distance `ring` around (tx,ty) are indexed.
func (ix *Index) loadRing(ctx context.Context, tx, ty, ring int) error {
	for dx := -ring; dx <= ring; dx++ {
		for dy := -ring; dy <= ring; dy++ {
			if maxAbs(dx, dy) != ring {
				continue // inner rings already loaded
			}
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
	tileSize := tileSizeMeters(ix.zoom)
	originX := -maxMercator + float64(wx)*tileSize
	originY := maxMercator - float64(wy)*tileSize
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
					originX+p.X/extent*tileSize,
					originY-p.Y/extent*tileSize,
				)
			}
			if len(vals) < 4 {
				continue
			}
			r := &road{
				name:     asString(f.Properties["name"]),
				ref:      asString(f.Properties["ref"]),
				class:    asString(f.Properties["class"]),
				subclass: asString(f.Properties["subclass"]),
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

// nearestIntersection finds the closest junction of 2+ named roads to the
// query point within ~150m.
func (ix *Index) nearestIntersection(mx, my, scale float64) *Intersection {
	const maxDist = 150.0
	var (
		best     *node
		bestDist float64
	)
	for _, n := range ix.nodes {
		if len(n.streets) < 2 {
			continue
		}
		d := math.Hypot(n.x-mx, n.y-my) * scale
		if d <= maxDist && (best == nil || d < bestDist) {
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
	return &Intersection{
		Lat:      mercatorYToLat(best.y),
		Lng:      mercatorXToLng(best.x),
		Distance: bestDist,
		Streets:  streets,
	}
}

// countRoadsNear counts distinct named roads whose distance is within 2x the
// best match distance — useful to gauge ambiguity of the position fix.
func (ix *Index) countRoadsNear(tx, ty, ring int, mx, my, bestMetersMerc float64) int {
	limit := math.Max(bestMetersMerc*2, bestMetersMerc+100)
	names := make(map[string]struct{})
	for _, t := range ix.tilesInRange(tx, ty, ring) {
		for _, r := range t.roads {
			d := distToLineString(mx, my, r.line)
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

func tileID(z, x, y int) uint64 {
	return uint64(z)<<40 | uint64(uint32(x))<<20 | uint64(uint32(y))
}

func wrap(v, n int) int {
	return ((v % n) + n) % n
}

func maxAbs(a, b int) int {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	if a > b {
		return a
	}
	return b
}

func nodeKey(p geom.XY) string {
	// Quantize to ~1 meter to tolerate duplicate encoding across tiles.
	return strconv.FormatInt(int64(math.Round(p.X)), 10) + ":" +
		strconv.FormatInt(int64(math.Round(p.Y)), 10)
}

func asString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func bearingDeg(start, end geom.XY) float64 {
	dx := end.X - start.X
	dy := end.Y - start.Y
	if dx == 0 && dy == 0 {
		return 0
	}
	b := math.Atan2(dx, dy) / degToRad
	if b < 0 {
		b += 360
	}
	return b
}
