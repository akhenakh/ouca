package ouca

import (
	"math"

	"github.com/peterstace/simplefeatures/geom"
)

// lowValueClasses are road classes that are poor position references
// (footpaths, tracks, ...) and only used as a last resort.
var lowValueClasses = map[string]struct{}{
	"path":    {},
	"track":   {},
	"pier":    {},
	"raceway": {},
}

// isPreferredRoad reports whether a road is a good candidate for inferring
// positions: named streets or drivable roads.
func isPreferredRoad(r *road) bool {
	if _, bad := lowValueClasses[r.class]; bad {
		return false
	}
	return r.name != "" || r.ref != ""
}

// bestMatch tracks the closest road to a query point in global tile units.
type bestMatch struct {
	qx, qy float64
	found  bool
	meters float64 // tile units; multiply by metersPerTileAtLat for ground meters
	road   *road
	point  geom.XY // snapped point on the road
	segStart,
	segEnd geom.XY // matched segment, for bearing
	snapIdx int
}

func newBestMatch(qx, qy float64) *bestMatch {
	return &bestMatch{qx: qx, qy: qy}
}

// search scans roads and updates the current best match.
func (b *bestMatch) search(roads []*road) {
	for _, r := range roads {
		d, p, s0, s1, idx := closestOnLineString(b.qx, b.qy, r.line)
		if !b.found || d < b.meters {
			b.found = true
			b.meters = d
			b.road = r
			b.point = p
			b.segStart = s0
			b.segEnd = s1
			b.snapIdx = idx
		}
	}
}

// closestOnLineString returns the distance from (px,py) to the line string in
// tile units, the closest point, the segment endpoints containing it and the
// index of the segment's first vertex.
func closestOnLineString(px, py float64, ls geom.LineString) (float64, geom.XY, geom.XY, geom.XY, int) {
	seq := ls.Coordinates()
	n := seq.Length()
	bestDist := math.MaxFloat64
	var bestPoint, segA, segB geom.XY
	bestIdx := -1
	for i := 0; i+1 < n; i++ {
		a := seq.GetXY(i)
		c := seq.GetXY(i + 1)
		p, d := closestOnSegment(px, py, a, c)
		if d < bestDist {
			bestDist = d
			bestPoint = p
			segA = a
			segB = c
			bestIdx = i
		}
	}
	return bestDist, bestPoint, segA, segB, bestIdx
}

// closestOnSegment returns the closest point on segment a-c to (px,py)
// and the distance to it.
func closestOnSegment(px, py float64, a, c geom.XY) (geom.XY, float64) {
	dx := c.X - a.X
	dy := c.Y - a.Y
	lenSq := dx*dx + dy*dy
	if lenSq == 0 {
		return a, math.Hypot(px-a.X, py-a.Y)
	}
	t := ((px-a.X)*dx + (py-a.Y)*dy) / lenSq
	t = math.Min(1, math.Max(0, t))
	p := geom.XY{X: a.X + t*dx, Y: a.Y + t*dy}
	return p, math.Hypot(px-p.X, py-p.Y)
}

// distToLineString returns the distance in tile units from (px,py) to ls.
func distToLineString(px, py float64, ls geom.LineString) float64 {
	d, _, _, _, _ := closestOnLineString(px, py, ls)
	return d
}
