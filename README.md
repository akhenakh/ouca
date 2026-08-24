# ouca

Reverse geocoding library for Go: given a latitude/longitude, `ouca` returns the
closest street, its distance and bearing from the query point, and the nearest
road intersection — enough to infer positions from noisy GPS fixes.

Roads are read on demand from [OpenMapTiles](https://openmaptiles.org/) vector
tiles served by [OpenFreeMap](https://www.openfreemap.org) (default provider:
`https://tiles.openfreemap.org/planet`, zoom 14 ≈ 9.5 m/pixel), decoded with
[github.com/akhenakh/mvtgo](https://github.com/akhenakh/mvtgo), with all
geometry math done via
[github.com/peterstace/simplefeatures](https://github.com/peterstace/simplefeatures).

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
4. Distances are computed in Web Mercator meters and converted to ground
   meters with the local scale factor (cos φ).

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
snapped at: 40.757992, -73.985483
bearing:    209°
```

## Options

```go
ix := ouca.NewIndex(
	ouca.WithZoom(14),                                        // tile zoom level (default 14)
	ouca.WithProvider("https://tiles.openfreemap.org/planet"), // TileJSON URL or {z}/{x}/{y}.pbf template
	ouca.WithHTTPTimeout(30*time.Second),
	ouca.WithMaxRings(1), // up to 3x3 tiles searched around the point
)
```

The index caches downloaded tiles in memory, so repeated lookups in the same
area cost no additional network traffic.

## Result

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

## License

MIT
