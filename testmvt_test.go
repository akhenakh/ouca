package ouca

import (
	"encoding/binary"
)

// The following helpers hand-encode a minimal Mapbox Vector Tile protobuf so
// tests can serve realistic tiles without extra dependencies.

func pvarint(b []byte, v uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(b, tmp[:n]...)
}

func ptag(b []byte, field, wire int) []byte {
	return pvarint(b, uint64(field<<3|wire))
}

func pbytes(b []byte, field int, v []byte) []byte {
	b = ptag(b, field, 2)
	b = pvarint(b, uint64(len(v)))
	return append(b, v...)
}

func pstring(b []byte, field int, s string) []byte {
	return pbytes(b, field, []byte(s))
}

// buildTestMVT returns a valid MVT containing one named residential road
// running horizontally through the middle of the tile.
func buildTestMVT() []byte {
	geometry := []byte{}
	geometry = pvarint(geometry, 9) // MoveTo, count 1
	geometry = pvarint(geometry, 0) // dx = 0
	geometry = pvarint(geometry, 4096)
	geometry = pvarint(geometry, 10) // LineTo, count 1
	geometry = pvarint(geometry, 8192)
	geometry = pvarint(geometry, 0)

	feature := []byte{}
	feature = pvarint(feature, 1<<3|0) // field 1 (id), varint
	feature = pvarint(feature, 42)
	feature = pbytes(feature, 2, []byte{0, 0, 1, 1}) // tags: name->v0, class->v1
	feature = pvarint(feature, 3<<3|0)               // field 3 (type)
	feature = pvarint(feature, 2)                    // LineString
	feature = pbytes(feature, 4, geometry)

	valueName := pstring(nil, 1, "Test Street")
	valueClass := pstring(nil, 1, "residential")

	layer := []byte{}
	layer = pvarint(layer, 15<<3|0) // field 15 (version)
	layer = pvarint(layer, 2)
	layer = pstring(layer, 1, "transportation")
	layer = pstring(layer, 3, "name")
	layer = pstring(layer, 3, "class")
	layer = pbytes(layer, 4, valueName)
	layer = pbytes(layer, 4, valueClass)
	layer = pbytes(layer, 2, feature)
	layer = pvarint(layer, 5<<3|0) // field 5 (extent)
	layer = pvarint(layer, 4096)

	return pbytes(nil, 3, layer) // field 3 (layer)
}
