package ouca

import "math"

const (
	earthRadius   = 6378137.0
	maxMercator   = earthRadius * math.Pi // 20037508.34...
	degToRad      = math.Pi / 180
	defaultExtent = 4096
)

// lngToMercatorX converts a longitude in degrees to EPSG:3857 X meters.
func lngToMercatorX(lng float64) float64 {
	return lng * degToRad * earthRadius
}

// latToMercatorY converts a latitude in degrees to EPSG:3857 Y meters.
func latToMercatorY(lat float64) float64 {
	lat = clamp(lat, -85.05112878, 85.05112878)
	return math.Log(math.Tan(math.Pi/4+lat*degToRad/2)) * earthRadius
}

// mercatorXToLng converts EPSG:3857 X meters to longitude degrees.
func mercatorXToLng(x float64) float64 {
	return x / earthRadius / degToRad
}

// mercatorYToLat converts EPSG:3857 Y meters to latitude degrees.
func mercatorYToLat(y float64) float64 {
	return (2*math.Atan(math.Exp(y/earthRadius)) - math.Pi/2) / degToRad
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

// tileSizeMeters returns the width of a tile in EPSG:3857 meters at zoom z.
func tileSizeMeters(z int) float64 {
	return 2 * maxMercator / float64(int(1)<<uint(z))
}

// latLngToTileF returns the fractional tile coordinates containing lat/lng.
func latLngToTileF(lat, lng float64, z int) (xf, yf float64) {
	n := float64(int(1) << uint(z))
	x := (lng + 180.0) / 360.0 * n
	y := (1 - math.Log(math.Tan(lat*degToRad)+1/math.Cos(lat*degToRad))/math.Pi) / 2 * n
	return x, y
}

// tileToLatLng returns the north-west corner of the given tile.
func tileToLatLng(x, y int, z int) (lat, lng float64) {
	n := float64(int(1) << uint(z))
	lng = float64(x)/n*360.0 - 180.0
	latRad := math.Atan(math.Sinh(math.Pi * (1 - 2*float64(y)/n)))
	lat = latRad / degToRad
	return lat, lng
}

// mercatorScaleAtLat is the local ground scale factor of Web Mercator.
// Mercator distances must be multiplied by this factor to get meters on the
// ground at the given latitude.
func mercatorScaleAtLat(lat float64) float64 {
	return math.Max(math.Cos(lat*degToRad), 1e-9)
}
