package ouca

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/peterstace/simplefeatures/geom"
)

func TestProjectionRoundTrip(t *testing.T) {
	wm := webMercator(14)
	cases := [][2]float64{
		{48.8566, 2.3522},
		{52.5163, 13.3777},
		{-33.8688, 151.2093},
	}
	for _, c := range cases {
		u := wm.Forward(geom.XY{X: c[1], Y: c[0]})
		gotLat, gotLng := tileLatLng(wm, u.X, u.Y)
		if math.Abs(gotLat-c[0]) > 1e-9 || math.Abs(gotLng-c[1]) > 1e-9 {
			t.Fatalf("round trip failed for %v: got %f,%f", c, gotLat, gotLng)
		}
	}
}

func TestTileMath(t *testing.T) {
	wm := webMercator(14)
	xf, yf := latLngToTileF(wm, 52.5163, 13.3777)
	if x, y := int(xf), int(yf); x != 8800 || y != 5373 {
		t.Fatalf("expected tile 8800/5373 got %d/%d", x, y)
	}

	// Meters-per-tile sanity: at the equator a z14 tile is ~2446m wide,
	// scaled by cos(lat).
	if m := metersPerTileAtLat(0, 14); math.Abs(m-EarthCircumferenceMeters/16384) > 1e-6 {
		t.Fatalf("bad metersPerTile at equator: %f", m)
	}
	if m := metersPerTileAtLat(60, 14); math.Abs(m-(EarthCircumferenceMeters/16384)*0.5) > 1e-3 {
		t.Fatalf("bad metersPerTile at 60°: %f", m)
	}
}

func TestBearingYDown(t *testing.T) {
	north := bearingDeg(geom.XY{}, geom.XY{X: 0, Y: -1})
	east := bearingDeg(geom.XY{}, geom.XY{X: 1, Y: 0})
	south := bearingDeg(geom.XY{}, geom.XY{X: 0, Y: 1})
	if north != 0 || east != 90 || south != 180 {
		t.Fatalf("bad bearings: N=%f E=%f S=%f", north, east, south)
	}
}

// TestClosestOnSegment checks the projection of a point onto a segment.
func TestClosestOnSegment(t *testing.T) {
	a := geom.XY{X: 0, Y: 0}
	b := geom.XY{X: 10, Y: 0}

	p, d := closestOnSegment(5, 3, a, b)
	if p.X != 5 || p.Y != 0 || d != 3 {
		t.Fatalf("interior projection failed: %v dist %f", p, d)
	}
	p, d = closestOnSegment(-4, 1, a, b)
	if p.X != 0 || d != math.Sqrt(17) {
		t.Fatalf("clamped projection failed: %v dist %f", p, d)
	}
}

func TestReverseLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ix := NewIndex()
	addr, err := ix.Reverse(ctx, 52.5163, 13.3777) // Brandenburger Tor, Berlin
	if err != nil {
		t.Fatalf("reverse failed: %v", err)
	}
	t.Logf("street=%s ref=%s class=%s distance=%.1fm bearing=%.0f snapped=(%.6f,%.6f)",
		addr.Street, addr.Ref, addr.Class, addr.Distance, addr.Bearing, addr.Lat, addr.Lng)
	if addr.Street == "" {
		t.Fatal("expected a named street")
	}
	if addr.Distance > 100 {
		t.Fatalf("unexpected large distance %.1fm", addr.Distance)
	}
	if addr.Intersection != nil {
		t.Logf("nearest intersection: %v at %.1fm (%.6f,%.6f)",
			addr.Intersection.Streets, addr.Intersection.Distance,
			addr.Intersection.Lat, addr.Intersection.Lng)
	}
}
