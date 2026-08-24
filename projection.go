package ouca

import (
	"math"

	"github.com/peterstace/simplefeatures/carto"
	"github.com/peterstace/simplefeatures/geom"
)

// EarthCircumferenceMeters is the WGS84 equatorial circumference.
const EarthCircumferenceMeters = 40075016.686

const degToRad = math.Pi / 180

const defaultExtent = 4096

// webMercator returns the Web Mercator projection for the given zoom.
// Coordinates are expressed in global tile units in [0, 2^z], with Y
// increasing downwards (carto.WebMercator convention).
func webMercator(z int) *carto.WebMercator {
	return carto.NewWebMercator(z)
}

// metersPerTileAtLat returns the ground size of one tile unit (and thus of a
// full tile) at the given latitude and zoom. Web Mercator is conformal, so
// this single scale factor converts tile-unit distances to ground meters
// around a point.
func metersPerTileAtLat(lat float64, z int) float64 {
	return math.Max(math.Cos(lat*degToRad), 1e-9) * EarthCircumferenceMeters / float64(int(1)<<uint(z))
}

// latLngToTileF returns the fractional tile coordinates containing lat/lng.
func latLngToTileF(wm *carto.WebMercator, lat, lng float64) (xf, yf float64) {
	p := wm.Forward(geom.XY{X: lng, Y: lat})
	return p.X, p.Y
}

// tileLatLng converts global tile units back to lat/lng degrees.
func tileLatLng(wm *carto.WebMercator, x, y float64) (lat, lng float64) {
	p := wm.Reverse(geom.XY{X: x, Y: y})
	return p.Y, p.X
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// bearingDeg returns the compass bearing of a segment given its start and end
// points in tile-unit space (Y axis points south).
func bearingDeg(start, end geom.XY) float64 {
	dx := end.X - start.X
	dy := end.Y - start.Y // positive = southwards
	if dx == 0 && dy == 0 {
		return 0
	}
	b := math.Atan2(dx, -dy) / degToRad
	if b < 0 {
		b += 360
	}
	return b
}
