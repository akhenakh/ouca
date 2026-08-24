package ouca

import (
	"context"
	"math"
	"testing"

	"github.com/peterstace/simplefeatures/geom"
)

// injectTile builds a fake tile at z14/x8800/y5373 with two crossing roads in
// global tile units: "Main Street" running north-south through the tile
// center, and "Side Lane" branching east at the middle.
func injectTile(ix *Index) {
	const (
		tx, ty = 8800.0, 5373.0
		cx, cy = 8800.5, 5373.5
	)
	mkLine := func(vals []float64) geom.LineString {
		return geom.NewLineString(geom.NewSequence(vals, geom.DimXY))
	}
	ix.tiles[tileID(14, int(tx), int(ty))] = &tileData{
		z: 14, x: int(tx), y: int(ty),
		roads: []*road{
			{id: 1, name: "Main Street", class: "residential",
				line: mkLine([]float64{cx, ty, cx, ty + 1})},
			{id: 2, name: "Side Lane", class: "residential",
				line: mkLine([]float64{cx, cy, tx + 1, cy})},
		},
	}
}

func unitsToLatLng(ix *Index, x, y float64) LatLng {
	lat, lng := tileLatLng(webMercator(ix.zoom), x, y)
	return LatLng{Lat: lat, Lng: lng}
}

func TestMatchPathValidation(t *testing.T) {
	ix := NewIndex()
	_, err := ix.MatchPath(context.Background(), []LatLng{{Lat: 52, Lng: 13}}, nil)
	if err == nil {
		t.Fatal("expected error for trace with < 2 points")
	}
}

func TestMatchPathViterbiSmoothing(t *testing.T) {
	ix := NewIndex()
	injectTile(ix)

	const cx = 8800.5 // X of Main Street
	opts := DefaultPathOptions("car")
	opts.TurnPenalty = 1000 // make jumping to Side Lane prohibitive

	// Trace along Main Street; point 2 is noisy and sits exactly on Side
	// Lane. Viterbi must keep the whole trace on Main Street.
	traceUnits := [][2]float64{
		{cx + 0.001, 5373.2},
		{cx + 0.001, 5373.4},
		{cx + 0.002, 5373.5}, // noisy: on the side road junction
		{cx - 0.0005, 5373.7},
	}
	var trace []LatLng
	for _, u := range traceUnits {
		trace = append(trace, unitsToLatLng(ix, u[0], u[1]))
	}

	match, err := ix.MatchPath(context.Background(), trace, &opts)
	if err != nil {
		t.Fatalf("MatchPath failed: %v", err)
	}
	if len(match.Points) != len(trace) {
		t.Fatalf("expected %d snapped points, got %d", len(trace), len(match.Points))
	}
	for i, p := range match.Points {
		if p.Street != "Main Street" {
			t.Errorf("point %d matched %q, want Main Street (Viterbi smoothing failed)", i, p.Street)
		}
		u := webMercator(14).Forward(geom.XY{X: p.Lng, Y: p.Lat})
		if math.Abs(u.X-cx) > 1e-6 {
			t.Errorf("point %d not snapped to Main Street X: got %f want %f", i, u.X, cx)
		}
		if !p.Matched {
			t.Errorf("point %d unexpectedly unmatched", i)
		}
	}
}

func TestMatchPathFollowsRoad(t *testing.T) {
	ix := NewIndex()
	injectTile(ix)

	// A clean trace along Main Street should interpolate intermediate road
	// vertices into Path.
	trace := []LatLng{
		unitsToLatLng(ix, 8800.5, 5373.1),
		unitsToLatLng(ix, 8800.5, 5373.9),
	}
	match, err := ix.MatchPath(context.Background(), trace, nil)
	if err != nil {
		t.Fatalf("MatchPath failed: %v", err)
	}
	for _, p := range match.Points {
		if p.Street != "Main Street" {
			t.Fatalf("matched %q, want Main Street", p.Street)
		}
	}
	// Trace spans 0.8 tile units; a z14 tile is ~1489m wide at this
	// latitude, so expect roughly 1190m.
	if match.Length < 1100 || match.Length > 1280 {
		t.Errorf("unexpected path length %.0fm for 0.8 tile units", match.Length)
	}
}

func TestClassPenalties(t *testing.T) {
	car := ClassPenalties("car")
	if car["footway"] != 100 || car["primary"] != 0 {
		t.Fatal("unexpected car penalties")
	}
	walk := ClassPenalties("walk")
	if walk["footway"] != 0 || walk["motorway"] != 100 {
		t.Fatal("unexpected walk penalties")
	}
}

// TestMatchPathLive matches a real noisy trace around Brandenburger Tor.
func TestMatchPathLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	ix := NewIndex()
	trace := []LatLng{
		{Lat: 52.5172, Lng: 13.3769},
		{Lat: 52.5168, Lng: 13.3773},
		{Lat: 52.5164, Lng: 13.3771}, // noise
		{Lat: 52.5160, Lng: 13.3775},
		{Lat: 52.5157, Lng: 13.3781}, // turn east
		{Lat: 52.5157, Lng: 13.3790},
	}
	match, err := ix.MatchPath(context.Background(), trace, nil)
	if err != nil {
		t.Fatalf("MatchPath failed: %v", err)
	}
	for i, p := range match.Points {
		t.Logf("%d %-25s %6.1fm matched=%v", i, p.Street, p.Distance, p.Matched)
	}
	if !match.Points[0].Matched || !match.Points[5].Matched {
		t.Fatal("expected endpoints to be matched")
	}
	if match.Points[0].Street == "" {
		t.Fatal("expected a street name on point 0")
	}
	if len(match.Path) < len(trace) || match.Length <= 0 {
		t.Fatalf("bad path: %d vertices %.0fm", len(match.Path), match.Length)
	}
}
