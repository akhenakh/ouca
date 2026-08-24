package ouca

import (
	"context"
	"math"
	"sort"

	"github.com/peterstace/simplefeatures/geom"
)

// LatLng is a WGS84 coordinate in degrees.
type LatLng struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// PathOptions tunes the HMM map matcher. Zero values are replaced by the
// defaults of DefaultPathOptions.
type PathOptions struct {
	// Mode selects per-class penalties: "car" (default), "bike" or "walk".
	Mode string
	// SearchRadius is the candidate search radius around each point, meters.
	SearchRadius float64
	// Sigma is the expected GPS noise standard deviation, meters.
	Sigma float64
	// Beta scales the transition cost difference between GPS and route
	// distance, meters.
	Beta float64
	// TurnPenalty penalizes switching between different streets.
	TurnPenalty float64
	// GapPenalty scales the penalty for routing gaps between disconnected
	// roads.
	GapPenalty float64
	// WrongWayPenalty penalizes one-way violations and topologically
	// impossible jumps (near-absolute rejection).
	WrongWayPenalty float64
}

// DefaultPathOptions returns sane defaults for the given transport mode.
func DefaultPathOptions(mode string) PathOptions {
	if mode == "" {
		mode = "car"
	}
	return PathOptions{
		Mode:            mode,
		SearchRadius:    50.0,
		Sigma:           10.0,
		Beta:            15.0,
		TurnPenalty:     25.0,
		GapPenalty:      5.0,
		WrongWayPenalty: 100000.0,
	}
}

// ClassPenalties returns how unlikely a transport mode is to travel on each
// OpenMapTiles road class; higher means less likely.
func ClassPenalties(mode string) map[string]float64 {
	penalties := make(map[string]float64)
	switch mode {
	case "walk":
		penalties["motorway"] = 100.0
		penalties["trunk"] = 100.0
		penalties["footway"] = 0.0
		penalties["path"] = 0.0
		penalties["pedestrian"] = 0.0
		penalties["steps"] = 0.0
		penalties["cycleway"] = 2.0
		penalties["residential"] = 1.0
		penalties["primary"] = 5.0
		penalties["secondary"] = 5.0
		penalties["rail"] = 100.0
		penalties["transit"] = 100.0
		penalties["aerialway"] = 100.0
	case "bike":
		penalties["motorway"] = 100.0
		penalties["trunk"] = 50.0
		penalties["cycleway"] = 0.0
		penalties["path"] = 1.0
		penalties["footway"] = 10.0
		penalties["pedestrian"] = 10.0
		penalties["steps"] = 20.0
		penalties["residential"] = 1.0
		penalties["primary"] = 3.0
		penalties["secondary"] = 2.0
		penalties["rail"] = 100.0
		penalties["transit"] = 100.0
		penalties["aerialway"] = 100.0
	default: // car
		penalties["path"] = 100.0
		penalties["footway"] = 100.0
		penalties["steps"] = 100.0
		penalties["pedestrian"] = 100.0
		penalties["cycleway"] = 100.0
		penalties["service"] = 5.0
		penalties["parking_aisle"] = 8.0
		penalties["driveway"] = 8.0
		penalties["track"] = 10.0
		penalties["corridor"] = 100.0
		penalties["living_street"] = 2.0
		penalties["residential"] = 0.0
		penalties["primary"] = 0.0
		penalties["secondary"] = 0.0
		penalties["tertiary"] = 0.0
		penalties["motorway"] = 0.0
		penalties["trunk"] = 0.0
		// Rail-like geometries often parallel roads (e.g. subway lines
		// under a street) and would otherwise win on raw proximity.
		penalties["rail"] = 100.0
		penalties["transit"] = 100.0
		penalties["aerialway"] = 100.0
		penalties["ferry"] = 50.0
	}
	return penalties
}

// classPenalty returns the penalty for a class, applying mode defaults for
// unknown classes.
func classPenalty(penalties map[string]float64, mode, class string) float64 {
	if p, ok := penalties[class]; ok {
		return p
	}
	if mode == "car" {
		return 10.0
	}
	if _, isLow := lowValueClasses[class]; isLow && mode == "walk" {
		return 0.0 // paths are fine on foot
	}
	return 5.0
}

// SnappedPoint is the result of matching one input trace point.
type SnappedPoint struct {
	Input    LatLng  `json:"input"`            // original GPS position
	Lat      float64 `json:"lat"`              // snapped latitude
	Lng      float64 `json:"lng"`              // snapped longitude
	Street   string  `json:"street,omitempty"` // matched street name
	Class    string  `json:"class,omitempty"`  // matched road class
	Distance float64 `json:"distance_meters"`  // GPS -> road distance
	Matched  bool    `json:"matched"`          // false if no road within radius
}

// PathMatch is the outcome of matching a full GPS trace.
type PathMatch struct {
	Points []SnappedPoint `json:"points"`        // one per input point
	Path   []LatLng       `json:"path"`          // matched polyline incl. intermediate vertices
	Length float64        `json:"length_meters"` // length of Path
}

// MatchPath maps a GPS trace to the road network using a Hidden Markov Model
// with Viterbi decoding. Candidates are sampled around each trace point;
// emission costs favor points close to a road, transition costs enforce
// smooth trajectories consistent with the GPS step lengths, street topology
// and (for cars) one-way restrictions.
func (ix *Index) MatchPath(ctx context.Context, trace []LatLng, opts *PathOptions) (*PathMatch, error) {
	if len(trace) < 2 {
		return nil, errTraceTooShort
	}
	for _, p := range trace {
		if err := validateLatLng(p.Lat, p.Lng); err != nil {
			return nil, err
		}
	}
	if opts == nil {
		d := DefaultPathOptions("car")
		opts = &d
	} else {
		d := DefaultPathOptions(opts.Mode)
		if opts.SearchRadius <= 0 {
			opts.SearchRadius = d.SearchRadius
		}
		if opts.Sigma <= 0 {
			opts.Sigma = d.Sigma
		}
		if opts.Beta <= 0 {
			opts.Beta = d.Beta
		}
		if opts.TurnPenalty <= 0 {
			opts.TurnPenalty = d.TurnPenalty
		}
		if opts.GapPenalty <= 0 {
			opts.GapPenalty = d.GapPenalty
		}
		if opts.WrongWayPenalty <= 0 {
			opts.WrongWayPenalty = d.WrongWayPenalty
		}
	}
	penalties := ClassPenalties(opts.Mode)

	ix.mu.Lock()
	defer ix.mu.Unlock()
	if err := ix.resolve(ctx); err != nil {
		return nil, err
	}

	wm := webMercator(ix.zoom)
	n := len(trace)

	dp := make([][]viterbiState, n)
	prevUnits := geom.XY{}

	for i, p := range trace {
		gps := wm.Forward(geom.XY{X: p.Lng, Y: p.Lat})
		mpt := metersPerTileAtLat(p.Lat, ix.zoom)

		if err := ix.loadTilesAround(gps, opts.SearchRadius, p.Lat); err != nil {
			return nil, err
		}

		candidates := ix.candidatesForPoint(gps, p.Lat, opts.SearchRadius, mpt, penalties, opts.Mode)
		if len(candidates) == 0 {
			// No road nearby: fall back to the raw GPS point so the trace
			// stays connected.
			candidates = []candidate{{
				point:      gps,
				distToGPS:  opts.SearchRadius * 2,
				classScore: 0,
			}}
		}

		dp[i] = make([]viterbiState, len(candidates))

		if i == 0 {
			for j, c := range candidates {
				dp[i][j] = viterbiState{candidate: c, cost: emissionCost(c, opts)}
			}
			prevUnits = gps
			continue
		}

		diffGps := gps.Sub(prevUnits)
		gpsStepMeters := math.Sqrt(diffGps.Dot(diffGps)) * mpt

		for j, curr := range candidates {
			bestCost := math.MaxFloat64
			bestPtr := -1
			for k, prev := range dp[i-1] {
				total := prev.cost + emissionCost(curr, opts) +
					transitionCost(prev.candidate, curr, diffGps, gpsStepMeters, mpt, opts)

				if total < bestCost {
					bestCost = total
					bestPtr = k
				}
			}
			dp[i][j] = viterbiState{candidate: curr, cost: bestCost, backPtr: bestPtr}
		}
		prevUnits = gps
	}

	// Backtrack the optimal path.
	idx := 0
	minCost := math.MaxFloat64
	for j, s := range dp[n-1] {
		if s.cost < minCost {
			minCost = s.cost
			idx = j
		}
	}
	states := make([]viterbiState, n)
	for i := n - 1; i >= 0; i-- {
		states[i] = dp[i][idx]
		idx = dp[i][idx].backPtr
	}

	// Rebuild the geometric path between snapped points.
	var unitsPath []geom.XY
	for i := 0; i < n; i++ {
		c := states[i].candidate
		if i > 0 {
			mpt := metersPerTileAtLat(trace[i].Lat, ix.zoom)
			unitsPath = append(unitsPath, bridgePath(states[i-1].candidate, c, mpt)...)
		}
		unitsPath = append(unitsPath, c.point)
	}
	unitsPath = removeSpurs(unitsPath)

	match := &PathMatch{Points: make([]SnappedPoint, n)}
	last := geom.XY{}
	for i, s := range states {
		c := s.candidate
		lat, lng := tileLatLng(wm, c.point.X, c.point.Y)
		street, ref := c.name, ""
		if c.roadRef != nil {
			ref = c.roadRef.ref
		}
		if street == "" && ref == "" && c.roadRef != nil {
			mpt := metersPerTileAtLat(trace[i].Lat, ix.zoom)
			const maxDonorMeters = 15.0
			street, ref = ix.nearestRoadName(c.point, maxDonorMeters/mpt)
		}
		match.Points[i] = SnappedPoint{
			Input:    trace[i],
			Lat:      lat,
			Lng:      lng,
			Street:   street,
			Class:    c.class,
			Distance: c.distToGPS,
			Matched:  c.roadRef != nil,
		}
		if i > 0 {
			d := dist(c.point, last)
			match.Length += d * metersPerTileAtLat(trace[i].Lat, ix.zoom)
		}
		last = c.point
	}
	for i, p := range unitsPath {
		if i > 0 && p == unitsPath[i-1] {
			continue
		}
		lat, lng := tileLatLng(wm, p.X, p.Y)
		match.Path = append(match.Path, LatLng{Lat: lat, Lng: lng})
	}
	return match, nil
}

var errTraceTooShort = &traceError{"trace must contain at least 2 points"}

// removeSpurs drops out-and-back excursions from a path: when the path
// returns to a previously visited point (within tol), the excursion in
// between is spliced out provided it is short. Such excursions appear when
// the matcher briefly snaps onto a side road and comes back.
func removeSpurs(path []geom.XY) []geom.XY {
	const (
		tol     = 0.004 // ~6m in tile units
		maxSpur = 0.045 // ~70m excursion length in tile units
	)
	for changed := true; changed; {
		changed = false
		for i := 0; i < len(path); i++ {
			for j := i + 2; j < len(path); j++ {
				if dist(path[i], path[j]) > tol {
					continue
				}
				excursion := 0.0
				for k := i; k < j; k++ {
					excursion += dist(path[k], path[k+1])
				}
				if excursion > maxSpur {
					continue
				}
				path = append(path[:i+1], path[j:]...)
				changed = true
				break
			}
			if changed {
				break
			}
		}
	}
	return path
}

type traceError struct{ msg string }

func (e *traceError) Error() string { return e.msg }

// candidate is one possible road match for a single trace point.
type candidate struct {
	point      geom.XY // snapped position, tile units
	distToGPS  float64 // meters from GPS to snapped position
	roadRef    *road   // nil for unmatched fallbacks
	featureID  uint64
	name       string
	class      string
	classScore float64
	segDir     geom.XY // direction of the matched segment
	oneway     bool
	onewayFlip bool
	seq        geom.Sequence
	snapIdx    int
}

type viterbiState struct {
	candidate candidate
	cost      float64
	backPtr   int
}

func emissionCost(c candidate, opts *PathOptions) float64 {
	return 0.5*math.Pow(c.distToGPS/opts.Sigma, 2) + c.classScore
}

// transitionCost scores moving from prev to curr given the observed GPS step.
func transitionCost(prev, curr candidate, diffGps geom.XY, gpsMeters, mpt float64, opts *PathOptions) float64 {
	diffRoute := curr.point.Sub(prev.point)
	routeMeters := math.Sqrt(diffRoute.Dot(diffRoute)) * mpt

	cost := math.Abs(gpsMeters-routeMeters) / opts.Beta

	if prev.featureID != curr.featureID {
		// Penalize jumping between distinct features.
		if prev.name == "" || curr.name == "" || prev.name != curr.name {
			cost += opts.TurnPenalty
		}

		// Topological awareness: if the gap between two roads is much larger
		// than the distance actually driven, reject the jump.
		gapMeters := minSeqDistanceMeters(prev.seq, curr.seq, mpt)
		switch {
		case gapMeters > gpsMeters+15.0:
			cost += opts.WrongWayPenalty
		case gapMeters > 2.0:
			cost += gapMeters * opts.GapPenalty
		}
	}

	// One-way awareness for cars.
	if curr.oneway {
		// Effective allowed direction: the segment's vertex order, reversed
		// when the restriction was inherited from an opposite-running
		// fragment.
		dir := curr.segDir
		if curr.onewayFlip {
			dir = geom.XY{X: -dir.X, Y: -dir.Y}
		}
		lenSqGps := diffGps.Dot(diffGps)
		lenSqSeg := dir.Dot(dir)
		if lenSqGps > 0 && lenSqSeg > 0 {
			cosTheta := diffGps.Dot(dir) / math.Sqrt(lenSqGps*lenSqSeg)
			if cosTheta < 0 && gpsMeters > 2.0 {
				cost += opts.WrongWayPenalty
			}
		}
	}
	return cost
}

// bridgePath returns the intermediate geometry between two consecutive
// snapped candidates: along the same road if possible, otherwise via the
// shared junction.
func bridgePath(prev, curr candidate, mpt float64) []geom.XY {
	if prev.roadRef == nil || curr.roadRef == nil {
		return nil
	}
	if prev.featureID == curr.featureID && sameSequence(prev.seq, curr.seq) {
		var out []geom.XY
		if prev.snapIdx < curr.snapIdx {
			for k := prev.snapIdx + 1; k <= curr.snapIdx; k++ {
				out = append(out, prev.seq.GetXY(k))
			}
		} else if prev.snapIdx > curr.snapIdx {
			for k := prev.snapIdx; k > curr.snapIdx; k-- {
				out = append(out, prev.seq.GetXY(k))
			}
		}
		return out
	}
	tolUnits := 5.0 / mpt // ~5 ground meters
	pt, idx1, idx2, found := findSharedPoint(prev.seq, curr.seq, tolUnits)
	if !found {
		return nil // no clean junction: keep the straight connector minimal
	}
	out := getIntermediatePoints(prev.seq, prev.snapIdx, idx1)
	out = append(out, pt)
	out = append(out, pointsFromTargetToSnap(curr.seq, idx2, curr.snapIdx)...)
	return out
}

// loadTilesAround ensures enough tiles around a GPS point are indexed to
// cover the search radius.
func (ix *Index) loadTilesAround(gps geom.XY, radiusMeters, lat float64) error {
	mpt := metersPerTileAtLat(lat, ix.zoom)
	tx, ty := int(math.Floor(gps.X)), int(math.Floor(gps.Y))
	ring := clampRing(int(radiusMeters/mpt), ix.maxRing)
	return ix.loadRing(context.Background(), tx, ty, ring)
}

// candidatesForPoint samples road candidates around a GPS point.
func (ix *Index) candidatesForPoint(gps geom.XY, lat, radiusMeters, mpt float64, penalties map[string]float64, mode string) []candidate {
	tx, ty := int(math.Floor(gps.X)), int(math.Floor(gps.Y))
	ring := clampRing(int(radiusMeters/mpt), ix.maxRing)

	var candidates []candidate
	for _, r := range ix.roadsInRing(tx, ty, ring) {
		dUnits, proj, segA, segB, snapIdx := closestOnLineString(gps.X, gps.Y, r.line)
		dMeters := dUnits * mpt
		if dMeters > radiusMeters {
			continue
		}
		candidates = append(candidates, candidate{
			point:      proj,
			distToGPS:  dMeters,
			roadRef:    r,
			featureID:  r.id,
			name:       r.name,
			class:      r.class,
			classScore: classPenalty(penalties, mode, r.class),
			segDir:     segB.Sub(segA),
			oneway:     mode == "car" && r.oneway,
			onewayFlip: r.onewayFlip,
			seq:        r.line.Coordinates(),
			snapIdx:    snapIdx,
		})
	}

	// Rank by a cheap heuristic cost; the Viterbi pass makes the final call.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].distToGPS+candidates[i].classScore <
			candidates[j].distToGPS+candidates[j].classScore
	})
	const maxCandidates = 32
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}
	return candidates
}

func clampRing(rings, max int) int {
	rings++
	if rings > max {
		return max
	}
	return rings
}

// minSeqDistanceMeters calculates the shortest physical distance between two
// line sequences (tile units converted to meters).
func minSeqDistanceMeters(s1, s2 geom.Sequence, mpt float64) float64 {
	if s1.Length() == 0 || s2.Length() == 0 {
		return 0
	}
	minSq := math.MaxFloat64
	for i := 0; i < s1.Length(); i++ {
		pt := s1.GetXY(i)
		for j := 0; j+1 < s2.Length(); j++ {
			a, b := s2.GetXY(j), s2.GetXY(j+1)
			proj := projectPoint(pt, a, b)
			if d := pt.Sub(proj).Dot(pt.Sub(proj)); d < minSq {
				minSq = d
			}
		}
	}
	for i := 0; i < s2.Length(); i++ {
		pt := s2.GetXY(i)
		for j := 0; j+1 < s1.Length(); j++ {
			a, b := s1.GetXY(j), s1.GetXY(j+1)
			proj := projectPoint(pt, a, b)
			if d := pt.Sub(proj).Dot(pt.Sub(proj)); d < minSq {
				minSq = d
			}
		}
	}
	return math.Sqrt(minSq) * mpt
}

func sameSequence(s1, s2 geom.Sequence) bool {
	if s1.Length() != s2.Length() {
		return false
	}
	if s1.Length() == 0 {
		return true
	}
	p1, p2 := s1.GetXY(0), s2.GetXY(0)
	p3, p4 := s1.GetXY(s1.Length()-1), s2.GetXY(s2.Length()-1)
	return p1 == p2 && p3 == p4
}

// findSharedPoint finds the first pair of vertices within tolerance between
// two sequences — typically the shared junction of two connected roads.
func findSharedPoint(seq1, seq2 geom.Sequence, tolUnits float64) (geom.XY, int, int, bool) {
	if seq1.Length() == 0 || seq2.Length() == 0 {
		return geom.XY{}, -1, -1, false
	}
	tolSq := tolUnits * tolUnits
	for i := 0; i < seq1.Length(); i++ {
		p1 := seq1.GetXY(i)
		for j := 0; j < seq2.Length(); j++ {
			p2 := seq2.GetXY(j)
			if p1.Sub(p2).Dot(p1.Sub(p2)) < tolSq {
				return p1, i, j, true
			}
		}
	}
	return geom.XY{}, -1, -1, false
}

func getIntermediatePoints(seq geom.Sequence, fromIdx, toIdx int) []geom.XY {
	var path []geom.XY
	if toIdx > fromIdx {
		for k := fromIdx + 1; k <= toIdx; k++ {
			path = append(path, seq.GetXY(k))
		}
	} else {
		for k := fromIdx; k >= toIdx && k >= 0; k-- {
			path = append(path, seq.GetXY(k))
		}
	}
	return path
}

func pointsFromTargetToSnap(seq geom.Sequence, targetIdx, snapIdx int) []geom.XY {
	var path []geom.XY
	if targetIdx <= snapIdx {
		for k := targetIdx + 1; k <= snapIdx; k++ {
			path = append(path, seq.GetXY(k))
		}
	} else {
		for k := targetIdx - 1; k >= snapIdx+1; k-- {
			path = append(path, seq.GetXY(k))
		}
	}
	return path
}

// projectPoint projects p onto segment a-b, clamped to its endpoints.
func projectPoint(p, a, b geom.XY) geom.XY {
	ab := b.Sub(a)
	lenSq := ab.Dot(ab)
	if lenSq == 0 {
		return a
	}
	t := p.Sub(a).Dot(ab) / lenSq
	switch {
	case t < 0:
		return a
	case t > 1:
		return b
	}
	return a.Add(ab.Scale(t))
}
