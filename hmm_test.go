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

// TestInheritOnewayFromFragments mirrors providers like OpenFreeMap that
// emit direction attributes only on unnamed geometry fragments of a way:
// the named fragment must adopt the restriction, including its orientation
// relative to its own vertex order.
func TestInheritOnewayFromFragments(t *testing.T) {
	const (
		tx, ty = 8800.0, 5373.0
	)
	mkLine := func(vals []float64) geom.LineString {
		return geom.NewLineString(geom.NewSequence(vals, geom.DimXY))
	}
	// Named fragment without a oneway attribute; unnamed tagged fragments
	// overlapping it, one aligned and one reversed.
	named := &road{id: 1, name: "One Way Ave", class: "secondary",
		line: mkLine([]float64{tx + 0.5, ty, tx + 0.5, ty + 1})}
	fragAligned := &road{id: 2, class: "secondary", oneway: true, onewayTagged: true,
		line: mkLine([]float64{tx + 0.5, ty + 0.3, tx + 0.5, ty + 0.6})}
	fragReversed := &road{id: 3, class: "secondary", oneway: true, onewayTagged: true,
		line: mkLine([]float64{tx + 0.5, ty + 0.7, tx + 0.5, ty + 0.4})}

	inheritOneway([]*road{named, fragAligned})
	if !named.oneway {
		t.Fatal("named road did not inherit oneway from overlapping fragment")
	}
	if named.onewayFlip {
		t.Fatal("flip set for aligned fragment")
	}

	named2 := &road{id: 4, name: "One Way Ave", class: "secondary",
		line: mkLine([]float64{tx + 0.5, ty, tx + 0.5, ty + 1})}
	inheritOneway([]*road{named2, fragReversed})
	if !named2.oneway || !named2.onewayFlip {
		t.Fatalf("expected inherited flipped oneway, got oneway=%v flip=%v",
			named2.oneway, named2.onewayFlip)
	}
}

// TestTransitionCostWrongWay checks that traveling against an inherited,
// flipped one-way restriction is penalized.
func TestTransitionCostWrongWay(t *testing.T) {
	opts := DefaultPathOptions("car")
	base := candidate{
		point:     geom.XY{X: 8800.5, Y: 5373.5},
		roadRef:   &road{},
		featureID: 1,
		name:      "One Way Ave",
	}
	prev := base

	// GPS step northbound along the segment.
	diffGps := geom.XY{X: 0, Y: -0.001}
	withOneway := base
	withOneway.oneway = true
	withOneway.segDir = geom.XY{X: 0, Y: -1} // encoded northbound (Y grows south)
	c := transitionCost(prev, withOneway, diffGps, 10, 1500, &opts)
	plain := transitionCost(prev, base, diffGps, 10, 1500, &opts)
	if c-plain > 1 {
		t.Fatalf("legal direction unexpectedly penalized: %f vs %f", c, plain)
	}

	// Same step but the restriction was inherited from a reversed fragment.
	flipped := withOneway
	flipped.onewayFlip = true
	c = transitionCost(prev, flipped, diffGps, 10, 1500, &opts)
	if c-plain < opts.WrongWayPenalty/2 {
		t.Fatalf("wrong-way travel not penalized when flipped: %f vs %f", c, plain)
	}
}

func TestClassPenalties(t *testing.T) {
	car := ClassPenalties("car")
	if car["footway"] != 100 || car["primary"] != 0 {
		t.Fatal("unexpected car penalties")
	}
	// Rail-like classes must be near-excluded for cars: their geometries
	// often parallel roads (subway under a street) and win on proximity.
	if car["transit"] != 100 || car["rail"] != 100 {
		t.Fatal("rail-like classes not penalized for cars")
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
